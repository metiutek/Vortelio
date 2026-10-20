package hub

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A GGUF model is not always one file: big quants are split into shards
// ("…-00001-of-00003.gguf", all needed, llama.cpp opens the first and finds the
// rest beside it), and vision models ship a projector ("mmproj-F16.gguf")
// next to the weights. ggufPlan turns the file the user picked into the full
// set to download.

var shardRe = regexp.MustCompile(`(?i)^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

func isGGUF(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".gguf") }

func isMmproj(name string) bool {
	return strings.Contains(strings.ToLower(path.Base(name)), "mmproj")
}

// ggufPlan is the set of files one GGUF model needs.
type ggufPlan struct {
	Shards []hfFileInfo // weights, first shard first
	Mmproj *hfFileInfo  // vision projector, nil when the repo has none
}

func (p ggufPlan) size() int64 {
	var n int64
	for _, f := range p.Shards {
		n += f.Size
	}
	if p.Mmproj != nil {
		n += p.Mmproj.Size
	}
	return n
}

// planGGUF expands chosen to every shard of its set and finds the projector
// that belongs with it (same folder first, then the repo root).
func planGGUF(files []hfFileInfo, chosen hfFileInfo, withMmproj bool) ggufPlan {
	plan := ggufPlan{Shards: []hfFileInfo{chosen}}
	if m := shardRe.FindStringSubmatch(chosen.Rfilename); m != nil {
		var set []hfFileInfo
		for _, f := range files {
			if g := shardRe.FindStringSubmatch(f.Rfilename); g != nil && g[1] == m[1] && g[3] == m[3] {
				set = append(set, f)
			}
		}
		sort.Slice(set, func(i, j int) bool { return set[i].Rfilename < set[j].Rfilename })
		if len(set) > 0 {
			plan.Shards = set
		}
	}
	if withMmproj {
		plan.Mmproj = pickMmproj(files, path.Dir(chosen.Rfilename))
	}
	return plan
}

// pickMmproj prefers a projector in dir, then anywhere; F16 > BF16 > Q8 > F32.
func pickMmproj(files []hfFileInfo, dir string) *hfFileInfo {
	rank := func(name string) int {
		n := strings.ToUpper(path.Base(name))
		switch {
		case strings.Contains(n, "F16") && !strings.Contains(n, "BF16"):
			return 0
		case strings.Contains(n, "BF16"):
			return 1
		case strings.Contains(n, "Q8"):
			return 2
		case strings.Contains(n, "F32"):
			return 3
		}
		return 4
	}
	var best *hfFileInfo
	bestScore := 1 << 30
	for i := range files {
		f := &files[i]
		if !isGGUF(f.Rfilename) || !isMmproj(f.Rfilename) {
			continue
		}
		score := rank(f.Rfilename)
		if path.Dir(f.Rfilename) != dir {
			score += 10
		}
		if score < bestScore {
			best, bestScore = f, score
		}
	}
	return best
}

// fetchGGUF downloads every file of plan into dir and returns the paths of
// the first shard and of the projector ("" when none). Shards keep their base
// names so llama.cpp finds them next to each other.
func (d *Downloader) fetchGGUF(owner, repo, dir string, plan ggufPlan, progress ProgressCallback) (string, string, error) {
	total := plan.size()
	var before int64
	files := append([]hfFileInfo{}, plan.Shards...)
	if plan.Mmproj != nil {
		files = append(files, *plan.Mmproj)
	}
	var mainPath, mmprojPath string
	for i, f := range files {
		dest := filepath.Join(dir, path.Base(f.Rfilename))
		if i == 0 {
			mainPath = dest
		}
		if plan.Mmproj != nil && f.Rfilename == plan.Mmproj.Rfilename {
			mmprojPath = dest
		}
		if len(files) > 1 {
			fmt.Printf("  ⬇️   [%d/%d] %s (%.0f MB)\n", i+1, len(files), path.Base(f.Rfilename), float64(f.Size)/1e6)
		}
		url := fmt.Sprintf("https://huggingface.co/%s/%s/resolve/main/%s", owner, repo, f.Rfilename)
		base := before
		err := d.downloadWithProgress(url, dest, f.Size, f.sha(), func(done, _ int64) {
			if progress != nil {
				progress(base+done, total)
			}
		})
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", path.Base(f.Rfilename), err)
		}
		before += f.Size
	}
	return mainPath, mmprojPath, nil
}

// preferredGGUF picks the default quant when the user gave no hint: Q4_K_M
// (the usual quality/size balance), then its Unsloth dynamic twin, else the
// smallest whole model. Projectors and non-first shards are never picked.
func preferredGGUF(files []hfFileInfo) (hfFileInfo, bool) {
	type cand struct {
		f    hfFileInfo
		size int64
	}
	var cands []cand
	for _, f := range files {
		if !isGGUF(f.Rfilename) || isMmproj(f.Rfilename) {
			continue
		}
		if m := shardRe.FindStringSubmatch(f.Rfilename); m != nil {
			if m[2] != "00001" {
				continue
			}
			cands = append(cands, cand{f, planGGUF(files, f, false).size()})
			continue
		}
		cands = append(cands, cand{f, f.Size})
	}
	if len(cands) == 0 {
		return hfFileInfo{}, false
	}
	for _, pref := range []string{"Q4_K_M", "UD-Q4_K_XL", "Q4_K_S", "Q4_0"} {
		for _, c := range cands {
			if strings.Contains(strings.ToUpper(path.Base(c.f.Rfilename)), strings.ToUpper(pref)) {
				return c.f, true
			}
		}
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.size > 0 && (best.size == 0 || c.size < best.size) {
			best = c
		}
	}
	return best.f, true
}
