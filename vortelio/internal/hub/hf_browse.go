package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Browsing Hugging Face from the GUI (like LM Studio's Discover): one
// repository with its model card and the downloadable GGUF quants.

// HFRepo describes a Hugging Face model repository.
type HFRepo struct {
	ID           string       `json:"id"`
	Author       string       `json:"author"`
	Pipeline     string       `json:"pipeline_tag"`
	License      string       `json:"license"`
	BaseModel    string       `json:"base_model"`
	LastModified string       `json:"last_modified"`
	Downloads    int          `json:"downloads"`
	Likes        int          `json:"likes"`
	Gated        bool         `json:"gated"`
	Tags         []string     `json:"tags"`
	Files        []HFRepoFile `json:"files"`   // GGUF quants, smallest first
	Weights      int          `json:"weights"` // non-GGUF weight files (safetensors, bin…)
	Vision       bool         `json:"vision"`  // the repo ships a vision projector
	Readme       string       `json:"readme"`  // model card, front matter removed
	ReadmeError  string       `json:"readme_error,omitempty"`
}

// HFRepoFile is one downloadable GGUF quant (all its shards together).
type HFRepoFile struct {
	Path   string `json:"path"`   // first shard, as in the repo
	Hint   string `json:"hint"`   // file name to put after ":" in a model reference
	Quant  string `json:"quant"`  // e.g. Q4_K_M, UD-Q4_K_XL, F16 ("" when unknown)
	Size   int64  `json:"size"`   // all shards
	Shards int    `json:"shards"` // 1 for a single file
}

var quantNameRe = regexp.MustCompile(`(?i)(UD-)?(I?Q[1-8](?:_[0-9A-Z]+)*|BF16|F16|F32|MXFP4(?:_MOE)?)`)

// QuantOf extracts the quantization from a GGUF file name.
func QuantOf(name string) string {
	base := strings.TrimSuffix(path.Base(name), path.Ext(name))
	base = shardRe.ReplaceAllString(base+".gguf", "$1")
	base = strings.TrimSuffix(base, ".gguf")
	m := quantNameRe.FindAllString(base, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.ToUpper(m[len(m)-1])
}

// FetchHFRepo reads a repository's metadata, files and model card.
func FetchHFRepo(ctx context.Context, id string) (*HFRepo, error) {
	owner, repo, ok := strings.Cut(strings.Trim(id, "/"), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("invalid repository %q (expected owner/name)", id)
	}
	var raw struct {
		ID           string       `json:"id"`
		Author       string       `json:"author"`
		Pipeline     string       `json:"pipeline_tag"`
		LastModified string       `json:"lastModified"`
		Downloads    int          `json:"downloads"`
		Likes        int          `json:"likes"`
		Gated        any          `json:"gated"` // false or "auto"/"manual"
		Tags         []string     `json:"tags"`
		Siblings     []hfFileInfo `json:"siblings"`
		CardData     struct {
			License   any `json:"license"`
			BaseModel any `json:"base_model"`
		} `json:"cardData"`
	}
	if err := hfGetJSON(ctx, fmt.Sprintf("https://huggingface.co/api/models/%s/%s?blobs=true", owner, repo), &raw); err != nil {
		return nil, err
	}
	r := &HFRepo{ID: raw.ID, Author: raw.Author, Pipeline: raw.Pipeline, LastModified: raw.LastModified,
		Downloads: raw.Downloads, Likes: raw.Likes, Tags: raw.Tags, License: firstString(raw.CardData.License),
		BaseModel: firstString(raw.CardData.BaseModel)}
	if g, ok := raw.Gated.(string); ok && g != "" {
		r.Gated = true
	}
	if r.Author == "" {
		r.Author = owner
	}

	// Group GGUF shards into one entry per quant; count other weights.
	for _, f := range raw.Siblings {
		name := f.Rfilename
		low := strings.ToLower(name)
		switch {
		case isGGUF(name) && isMmproj(name):
			r.Vision = true
		case isGGUF(name):
			if m := shardRe.FindStringSubmatch(path.Base(name)); m != nil && m[2] != "00001" {
				continue
			}
			plan := planGGUF(raw.Siblings, f, false)
			e := HFRepoFile{Path: name, Hint: path.Base(name), Quant: QuantOf(name), Size: plan.size(), Shards: len(plan.Shards)}
			r.Files = append(r.Files, e)
		case strings.HasSuffix(low, ".safetensors") || strings.HasSuffix(low, ".bin") || strings.HasSuffix(low, ".pth") || strings.HasSuffix(low, ".ckpt"):
			r.Weights++
		}
	}
	sort.SliceStable(r.Files, func(i, j int) bool { return r.Files[i].Size < r.Files[j].Size })

	readme, err := hfGetText(ctx, fmt.Sprintf("https://huggingface.co/%s/%s/raw/main/README.md", owner, repo), 256<<10)
	if err != nil {
		r.ReadmeError = err.Error()
	}
	r.Readme = stripFrontMatter(readme)
	return r, nil
}

func firstString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		if len(x) > 0 {
			if s, ok := x[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

func stripFrontMatter(s string) string {
	s = strings.TrimPrefix(s, string(rune(0xFEFF)))
	if strings.HasPrefix(s, "---") {
		if i := strings.Index(s[3:], "\n---"); i >= 0 {
			s = s[3+i+4:]
		}
	}
	return strings.TrimSpace(s)
}

func hfRequest(ctx context.Context, url string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header = hfHeader()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("Hugging Face is not reachable: %w", err)
	}
	resp.Body = cancelOnClose{resp.Body, cancel}
	switch resp.StatusCode {
	case 200:
		return resp, nil
	case 401, 403:
		resp.Body.Close()
		return nil, fmt.Errorf("this repository is gated: accept its terms on huggingface.co and add your token (Downloads → Hugging Face token)")
	case 404:
		resp.Body.Close()
		return nil, fmt.Errorf("not found on Hugging Face")
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("Hugging Face: %s", resp.Status)
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error { err := c.ReadCloser.Close(); c.cancel(); return err }

func hfGetJSON(ctx context.Context, url string, v any) error {
	resp, err := hfRequest(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

func hfGetText(ctx context.Context, url string, max int64) (string, error) {
	resp, err := hfRequest(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return string(b), err
}
