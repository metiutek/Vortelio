package runtime

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/downloads"
)

// Native inference engines (stable-diffusion.cpp, audio.cpp, whisper.cpp) are
// prebuilt C++ servers downloaded from a pinned GitHub release, so media
// generation needs no Python, pip or torch. Each engine lives in
// ~/.vortelio/engines/<id>/current with a marker file recording what is
// installed. An install is staged next to it, must start (`--help`) before it
// replaces current, and the previous install is kept for one-step rollback.
//
// CUDA builds share one copy of the CUDA runtime DLLs (~/.vortelio/engines/
// cuda12), hard-linked into each engine folder: llama.cpp already ships most
// of them, so they are rarely downloaded at all.

// EngineSpec describes one managed engine.
type EngineSpec struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Repo    string `json:"repo"`
	Tag     string `json:"tag"` // pinned release
	// Server is the executable Vortelio launches (without .exe).
	Server string `json:"server"`
	// Extra executables that must be present (without .exe).
	Extra []string `json:"-"`
	// assets lists, best first, the archive for this machine and the
	// accelerator it targets.
	assets func(goos, goarch string, b HardwareBackend) []assetChoice
	// CUDA runtime DLLs the CUDA build needs beside it (Windows).
	cudaRuntime []string
	// Members of the archive provided by the shared CUDA runtime instead; when
	// set, only the remaining members are fetched (ranged zip reads).
	skipMembers []string
}

type assetChoice struct {
	re    *regexp.Regexp
	accel string // cuda | vulkan | cpu | metal
}

func choice(accel, pattern string) assetChoice {
	return assetChoice{re: regexp.MustCompile(pattern), accel: accel}
}

// cuda12Runtime is where the shared CUDA 12 runtime DLLs are fetched from
// when llama.cpp does not already provide them: a release asset holding
// cudart, cuBLAS, cuBLASLt and cuFFT (only the needed members are read).
const cuda12RuntimeURL = "https://github.com/unslothai/audio.cpp/releases/download/v0.9.2-drama3-base-small-unsloth.1/audio-v0.9.2-drama3-base-small-unsloth.1-cudart-windows-x64-cuda12.zip"

var cudaBase = []string{"cudart64_12.dll", "cublas64_12.dll", "cublasLt64_12.dll"}

var engineSpecs = []*EngineSpec{
	{
		ID: "sdcpp", Name: "stable-diffusion.cpp", Purpose: "Images and video",
		Repo: "unslothai/stable-diffusion.cpp", Tag: "master-968-8cd731e-ubdb47d1",
		Server: "sd-server", Extra: []string{"sd-cli"},
		assets: func(goos, goarch string, b HardwareBackend) []assetChoice {
			var c []assetChoice
			switch goos {
			case "windows":
				if b == BackendCUDA {
					c = append(c, choice("cuda", `-bin-win-cuda12-x64\.zip$`))
				}
				if b == BackendCUDA || b == BackendROCm {
					c = append(c, choice("vulkan", `-bin-win-vulkan-x64\.zip$`))
				}
				c = append(c, choice("cpu", `-bin-win-cpu-x64\.zip$`))
			case "darwin":
				if goarch == "arm64" {
					c = append(c, choice("metal", `-bin-Darwin-macOS-arm64\.zip$`))
				} else {
					c = append(c, choice("cpu", `-bin-Darwin-macOS-x86_64\.zip$`))
				}
			default:
				if goarch == "arm64" {
					c = append(c, choice("cpu", `-bin-Linux-Ubuntu-[\d.]+-aarch64\.zip$`))
					break
				}
				if b == BackendCUDA {
					c = append(c, choice("cuda", `-bin-Linux-Ubuntu-22\.04-x86_64-cuda12\.zip$`))
				}
				if b == BackendCUDA || b == BackendROCm {
					c = append(c, choice("vulkan", `-bin-Linux-Ubuntu-22\.04-x86_64-vulkan\.zip$`))
				}
				c = append(c, choice("cpu", `-bin-Linux-Ubuntu-22\.04-x86_64\.zip$`))
			}
			return c
		},
		cudaRuntime: cudaBase,
	},
	{
		ID: "audiocpp", Name: "audio.cpp", Purpose: "Speech, music, voice editing and cloning",
		Repo: "unslothai/audio.cpp", Tag: "v0.9.2-drama3-base-small-unsloth.1",
		Server: "audiocpp_server",
		assets: func(goos, goarch string, b HardwareBackend) []assetChoice {
			var c []assetChoice
			switch goos {
			case "windows":
				if b == BackendCUDA {
					c = append(c, choice("cuda", `-bin-windows-x64-cuda12\.zip$`))
				}
				if b == BackendCUDA || b == BackendROCm {
					c = append(c, choice("vulkan", `-bin-windows-x64-vulkan\.zip$`))
				}
				c = append(c, choice("cpu", `-bin-windows-x64-cpu\.zip$`))
			case "darwin":
				if goarch == "arm64" {
					c = append(c, choice("metal", `-bin-macos-arm64-metal\.tar\.gz$`))
				} else {
					c = append(c, choice("metal", `-bin-macos-x64-metal\.tar\.gz$`))
				}
			default:
				if goarch == "arm64" {
					c = append(c, choice("cpu", `-bin-ubuntu-arm64-cpu\.tar\.gz$`))
					break
				}
				if b == BackendCUDA {
					c = append(c, choice("cuda", `-bin-ubuntu-x64-cuda12\.tar\.gz$`))
				}
				if b == BackendCUDA || b == BackendROCm {
					c = append(c, choice("vulkan", `-bin-ubuntu-x64-vulkan\.tar\.gz$`))
				}
				c = append(c, choice("cpu", `-bin-ubuntu-x64-cpu\.tar\.gz$`))
			}
			return c
		},
		cudaRuntime: append(append([]string{}, cudaBase...), "cufft64_11.dll"),
	},
	{
		ID: "whisper", Name: "whisper.cpp", Purpose: "Speech to text",
		Repo: "ggml-org/whisper.cpp", Tag: "b5454",
		Server: "whisper-server", Extra: []string{"whisper-cli"},
		assets: func(goos, goarch string, b HardwareBackend) []assetChoice {
			var c []assetChoice
			switch goos {
			case "windows":
				if goarch == "arm64" {
					return []assetChoice{choice("cpu", `^whisper-bin-win-cpu-arm64\.zip$`)}
				}
				if b == BackendCUDA {
					c = append(c, choice("cuda", `^whisper-bin-win-cuda-12\.[\d.]+-x64\.zip$`))
				}
				c = append(c, choice("cpu", `^whisper-blas-bin-x64\.zip$`))
			default:
				if goos == "linux" && goarch == "arm64" {
					c = append(c, choice("cpu", `^whisper-bin-ubuntu-arm64\.tar\.gz$`))
				} else if goos == "linux" {
					c = append(c, choice("cpu", `^whisper-bin-ubuntu-x64\.tar\.gz$`))
				}
			}
			return c
		},
		cudaRuntime: cudaBase,
		skipMembers: []string{"cublas64_12.dll", "cublasLt64_12.dll"},
	},
}

// Engines lists the managed engines.
func Engines() []*EngineSpec { return engineSpecs }

// EngineByID returns the spec for id, or nil.
func EngineByID(id string) *EngineSpec {
	for _, s := range engineSpecs {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// pinnedTag is the release to install: VORTELIO_ENGINE_<ID>_TAG overrides.
func (s *EngineSpec) pinnedTag() string {
	if v := strings.TrimSpace(os.Getenv("VORTELIO_ENGINE_" + strings.ToUpper(s.ID) + "_TAG")); v != "" {
		return v
	}
	return s.Tag
}

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

// EnginesDir is the root of all managed engines.
func EnginesDir() string {
	d := LlamaDir() // ~/.vortelio/bin
	if d == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d), "engines")
}

func engineRoot(id string) string { return filepath.Join(EnginesDir(), id) }

const engineMarker = "vortelio-engine.json"

// EngineInstall is the marker written into an installed engine folder.
type EngineInstall struct {
	ID          string    `json:"id"`
	Repo        string    `json:"repo"`
	Tag         string    `json:"tag"`
	Asset       string    `json:"asset"`
	Digest      string    `json:"digest,omitempty"`
	Accel       string    `json:"accel"`
	Server      string    `json:"server"` // path relative to the install folder
	InstalledAt time.Time `json:"installed_at"`
}

func readMarker(dir string) (*EngineInstall, error) {
	data, err := os.ReadFile(filepath.Join(dir, engineMarker))
	if err != nil {
		return nil, err
	}
	var m EngineInstall
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// EngineStatus is what the UI shows for one engine.
type EngineStatus struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Purpose         string `json:"purpose"`
	Installed       bool   `json:"installed"`
	Tag             string `json:"tag,omitempty"`
	PinnedTag       string `json:"pinned_tag"`
	Accel           string `json:"accel,omitempty"`
	Path            string `json:"path,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	CanRollback     bool   `json:"can_rollback"`
	PreviousTag     string `json:"previous_tag,omitempty"`
	SizeBytes       int64  `json:"size_bytes"`
	Broken          string `json:"broken,omitempty"`
}

// GetEngineStatus reports the install state of one engine.
func GetEngineStatus(id string) EngineStatus {
	s := EngineByID(id)
	if s == nil {
		return EngineStatus{ID: id, Broken: "unknown engine"}
	}
	st := EngineStatus{ID: s.ID, Name: s.Name, Purpose: s.Purpose, PinnedTag: s.pinnedTag()}
	cur := filepath.Join(engineRoot(id), "current")
	if m, err := readMarker(cur); err == nil {
		st.Tag, st.Accel = m.Tag, m.Accel
		bin := filepath.Join(cur, filepath.FromSlash(m.Server))
		if fi, err := os.Stat(bin); err == nil && !fi.IsDir() {
			st.Installed, st.Path = true, bin
		} else {
			st.Broken = "server executable missing — repair the engine"
		}
		st.UpdateAvailable = m.Tag != st.PinnedTag
		st.SizeBytes = dirSize(cur)
	}
	if m, err := readMarker(filepath.Join(engineRoot(id), "previous")); err == nil {
		st.CanRollback, st.PreviousTag = true, m.Tag
	}
	return st
}

// AllEngineStatus reports every engine.
func AllEngineStatus() []EngineStatus {
	out := make([]EngineStatus, 0, len(engineSpecs))
	for _, s := range engineSpecs {
		out = append(out, GetEngineStatus(s.ID))
	}
	return out
}

// EngineBin returns the path of an executable of an installed engine ("" =
// not installed). name "" means the engine's server.
func EngineBin(id, name string) string {
	s := EngineByID(id)
	if s == nil {
		return ""
	}
	cur := filepath.Join(engineRoot(id), "current")
	m, err := readMarker(cur)
	if err != nil {
		return ""
	}
	p := filepath.Join(cur, filepath.FromSlash(m.Server))
	if name != "" && name != s.Server {
		p = filepath.Join(filepath.Dir(p), exeName(name))
	}
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// ── stop hooks ──────────────────────────────────────────────────────────────

var (
	engineHooksMu sync.Mutex
	engineStops   = map[string][]func(){}
	engineLocks   = map[string]*sync.Mutex{}
)

// OnEngineReplace registers fn to stop processes using engine id before its
// files are replaced or removed.
func OnEngineReplace(id string, fn func()) {
	engineHooksMu.Lock()
	engineStops[id] = append(engineStops[id], fn)
	engineHooksMu.Unlock()
}

func stopEngine(id string) {
	engineHooksMu.Lock()
	fns := append([]func(){}, engineStops[id]...)
	engineHooksMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func engineLock(id string) *sync.Mutex {
	engineHooksMu.Lock()
	defer engineHooksMu.Unlock()
	if engineLocks[id] == nil {
		engineLocks[id] = &sync.Mutex{}
	}
	return engineLocks[id]
}

// ── release lookup ──────────────────────────────────────────────────────────

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type ghTagRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

func fetchRelease(ctx context.Context, repo, tag string) (*ghTagRelease, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/tags/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vortelio")
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("release %s of %s not found", tag, repo)
	case http.StatusForbidden, http.StatusTooManyRequests:
		return nil, errors.New("GitHub rate limit reached — try again in a few minutes (or set GITHUB_TOKEN)")
	default:
		return nil, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	var rel ghTagRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func pickEngineAsset(s *EngineSpec, rel *ghTagRelease, goos, goarch string, b HardwareBackend) (*ghAsset, string) {
	for _, c := range s.assets(goos, goarch, b) {
		for i := range rel.Assets {
			if c.re.MatchString(rel.Assets[i].Name) {
				return &rel.Assets[i], c.accel
			}
		}
	}
	return nil, ""
}

// ── install ─────────────────────────────────────────────────────────────────

// EngineProgress receives install progress (downloads.Progress satisfies it).
type EngineProgress interface {
	Phase(string)
	SetTotal(int64)
	SetDone(int64)
}

type nopProgress struct{}

func (nopProgress) Phase(string)   {}
func (nopProgress) SetTotal(int64) {}
func (nopProgress) SetDone(int64)  {}

// InstallEngine installs (or repairs / updates to the pinned release) engine id.
func InstallEngine(ctx context.Context, id string, hw *Hardware, p EngineProgress) error {
	s := EngineByID(id)
	if s == nil {
		return fmt.Errorf("unknown engine %q", id)
	}
	if p == nil {
		p = nopProgress{}
	}
	if hw == nil {
		hw = DetectHardware()
	}
	lk := engineLock(id)
	lk.Lock()
	defer lk.Unlock()

	tag := s.pinnedTag()
	p.Phase(fmt.Sprintf("Looking up %s %s…", s.Name, tag))
	rel, err := fetchRelease(ctx, s.Repo, tag)
	if err != nil {
		return err
	}
	asset, accel := pickEngineAsset(s, rel, runtime.GOOS, runtime.GOARCH, hw.Backend)
	if asset == nil {
		return fmt.Errorf("%s %s has no build for %s/%s", s.Name, tag, runtime.GOOS, runtime.GOARCH)
	}

	root := engineRoot(id)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Archives are cached under engines/<id>/downloads so a cancelled or
	// failed install resumes instead of starting over.
	dlDir := filepath.Join(root, "downloads")
	stage := filepath.Join(root, "staging")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	needCUDA := accel == "cuda" && runtime.GOOS == "windows" && len(s.cudaRuntime) > 0
	if len(s.skipMembers) > 0 && needCUDA && strings.HasSuffix(asset.Name, ".zip") {
		p.Phase(fmt.Sprintf("Downloading %s (%s)…", s.Name, accel))
		if err := extractZipWithout(ctx, asset.URL, s.skipMembers, stage, p); err != nil {
			return err
		}
	} else {
		archive := filepath.Join(dlDir, asset.Name)
		p.Phase(fmt.Sprintf("Downloading %s (%s)…", s.Name, accel))
		p.SetTotal(asset.Size)
		err := downloads.Fetch(ctx, downloads.Spec{URL: asset.URL, Dest: archive, Size: asset.Size, SHA256: asset.Digest},
			func(h int64) { p.SetDone(h) })
		if err != nil {
			return err
		}
		p.Phase("Unpacking…")
		if err := extractTree(archive, stage); err != nil {
			os.Remove(archive) // unreadable archive: fetch a fresh copy next time
			return fmt.Errorf("cannot unpack %s: %w", asset.Name, err)
		}
	}

	serverPath := findFile(stage, exeName(s.Server))
	if serverPath == "" {
		return fmt.Errorf("%s does not contain %s", asset.Name, exeName(s.Server))
	}
	binDir := filepath.Dir(serverPath)
	for _, x := range s.Extra {
		if _, err := os.Stat(filepath.Join(binDir, exeName(x))); err != nil {
			return fmt.Errorf("%s does not contain %s", asset.Name, exeName(x))
		}
	}

	if needCUDA {
		if err := provideCUDARuntime(ctx, s.cudaRuntime, binDir, p); err != nil {
			return fmt.Errorf("CUDA runtime: %w", err)
		}
	}

	p.Phase("Checking that it starts…")
	if err := smokeTest(serverPath); err != nil {
		return fmt.Errorf("%s does not start on this machine: %w", s.Name, err)
	}

	rel2, _ := filepath.Rel(stage, serverPath)
	marker := EngineInstall{
		ID: id, Repo: s.Repo, Tag: tag, Asset: asset.Name, Digest: asset.Digest, Accel: accel,
		Server: filepath.ToSlash(rel2), InstalledAt: time.Now(),
	}
	data, _ := json.MarshalIndent(marker, "", "  ")
	if err := os.WriteFile(filepath.Join(stage, engineMarker), data, 0o644); err != nil {
		return err
	}

	p.Phase("Activating…")
	stopEngine(id)
	if err := swapIn(root, stage); err != nil {
		return err
	}
	// The archive is no longer needed once installed.
	os.RemoveAll(dlDir)
	p.Phase(fmt.Sprintf("%s %s ready", s.Name, tag))
	return nil
}

// swapIn makes stage the current install, keeping the old one as previous.
func swapIn(root, stage string) error {
	cur := filepath.Join(root, "current")
	prev := filepath.Join(root, "previous")
	if _, err := os.Stat(cur); err == nil {
		if err := retryFS(func() error { return os.RemoveAll(prev) }); err != nil {
			return fmt.Errorf("cannot clear the previous install: %w", err)
		}
		if err := retryFS(func() error { return os.Rename(cur, prev) }); err != nil {
			return fmt.Errorf("the engine is still in use — stop running generations and retry: %w", err)
		}
	}
	if err := retryFS(func() error { return os.Rename(stage, cur) }); err != nil {
		// Put the old install back so the engine keeps working.
		_ = os.Rename(prev, cur)
		return fmt.Errorf("cannot activate the new install: %w", err)
	}
	return nil
}

func retryFS(fn func() error) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

// RollbackEngine swaps the current and previous installs.
func RollbackEngine(id string) error {
	if EngineByID(id) == nil {
		return fmt.Errorf("unknown engine %q", id)
	}
	lk := engineLock(id)
	lk.Lock()
	defer lk.Unlock()
	root := engineRoot(id)
	cur, prev, tmp := filepath.Join(root, "current"), filepath.Join(root, "previous"), filepath.Join(root, "swap")
	if _, err := readMarker(prev); err != nil {
		return errors.New("no previous install to restore")
	}
	stopEngine(id)
	os.RemoveAll(tmp)
	if err := retryFS(func() error { return os.Rename(prev, tmp) }); err != nil {
		return err
	}
	if _, err := os.Stat(cur); err == nil {
		if err := retryFS(func() error { return os.Rename(cur, prev) }); err != nil {
			_ = os.Rename(tmp, prev)
			return fmt.Errorf("the engine is still in use: %w", err)
		}
	}
	return retryFS(func() error { return os.Rename(tmp, cur) })
}

// RemoveEngine deletes an engine (models are untouched).
func RemoveEngine(id string) error {
	if EngineByID(id) == nil {
		return fmt.Errorf("unknown engine %q", id)
	}
	lk := engineLock(id)
	lk.Lock()
	defer lk.Unlock()
	stopEngine(id)
	return retryFS(func() error { return os.RemoveAll(engineRoot(id)) })
}

// smokeTest runs `<server> --help`: a build that misses a DLL or needs a newer
// driver fails here, before it replaces a working install.
func smokeTest(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	cmd.Dir = filepath.Dir(bin)
	HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("timed out")
	}
	text := strings.ToLower(string(out))
	if err == nil || strings.Contains(text, "usage") || strings.Contains(text, "--port") || strings.Contains(text, "options") {
		return nil
	}
	tail := strings.TrimSpace(string(out))
	if len(tail) > 300 {
		tail = tail[len(tail)-300:]
	}
	if tail == "" {
		tail = err.Error()
		if strings.Contains(tail, "0xc0000135") || strings.Contains(tail, "3221225781") {
			tail = "a required DLL is missing"
		}
	}
	return errors.New(tail)
}

// findFile returns the shallowest file named name under root.
func findFile(root, name string) string {
	var best string
	bestDepth := 1 << 30
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !strings.EqualFold(fi.Name(), name) {
			return nil
		}
		if d := strings.Count(p, string(os.PathSeparator)); d < bestDepth {
			best, bestDepth = p, d
		}
		return nil
	})
	return best
}

// ── archives ────────────────────────────────────────────────────────────────

// cleanMember returns a safe relative path for an archive entry, with a
// single top-level folder shared by every entry stripped (strip), or "".
func cleanMember(name, strip string) string {
	name = strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, "\\", "/")), "/")
	if strip != "" {
		if !strings.HasPrefix(name, strip+"/") {
			return ""
		}
		name = strings.TrimPrefix(name, strip+"/")
	}
	if name == "" || name == "." || strings.HasPrefix(name, "../") {
		return ""
	}
	return filepath.FromSlash(name)
}

// commonRoot is the single top-level folder of all names, or "".
func commonRoot(names []string) string {
	root := ""
	for _, n := range names {
		n = strings.TrimPrefix(strings.ReplaceAll(n, "\\", "/"), "./")
		i := strings.Index(n, "/")
		if i <= 0 {
			return ""
		}
		if root == "" {
			root = n[:i]
		} else if n[:i] != root {
			return ""
		}
	}
	return root
}

// extractTree unpacks a zip or tar.gz into dest keeping its folder layout
// (audio.cpp reads model_specs/ beside its server).
func extractTree(archive, dest string) error {
	if strings.HasSuffix(strings.ToLower(archive), ".zip") {
		r, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer r.Close()
		var names []string
		for _, f := range r.File {
			if !f.FileInfo().IsDir() {
				names = append(names, f.Name)
			}
		}
		strip := commonRoot(names)
		for _, f := range r.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rel := cleanMember(f.Name, strip)
			if rel == "" {
				continue
			}
			out := filepath.Join(dest, rel)
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeFile(out, rc, f.Mode()|0o644)
			rc.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}
	return extractTarTree(archive, dest)
}

func extractTarTree(archive, dest string) error {
	open := func() (*tar.Reader, func(), error) {
		f, err := os.Open(archive)
		if err != nil {
			return nil, nil, err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		return tar.NewReader(gz), func() { gz.Close(); f.Close() }, nil
	}
	tr, done, err := open()
	if err != nil {
		return err
	}
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			done()
			return err
		}
		if h.Typeflag != tar.TypeDir {
			names = append(names, h.Name)
		}
	}
	done()
	strip := commonRoot(names)

	tr, done, err = open()
	if err != nil {
		return err
	}
	defer done()
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel := cleanMember(h.Name, strip)
		if rel == "" {
			continue
		}
		out := filepath.Join(dest, rel)
		switch h.Typeflag {
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			if err := writeFile(out, tr, os.FileMode(h.Mode)|0o644); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := safeBase(h.Linkname)
			if target == "" {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(out), 0o755)
			os.Remove(out)
			if err := os.Symlink(target, out); err != nil {
				return err
			}
		}
	}
}

// extractZipWithout fetches every member of a remote zip except skip into
// dest (layout kept, common root stripped), reading only the needed ranges.
func extractZipWithout(ctx context.Context, url string, skip []string, dest string, p EngineProgress) error {
	names, _, err := downloads.RemoteZipListing(ctx, url)
	if err != nil {
		return err
	}
	skipSet := map[string]bool{}
	for _, s := range skip {
		skipSet[strings.ToLower(s)] = true
	}
	var want []downloads.ZipMember
	var size int64
	strip := commonRoot(namesOf(names))
	for _, m := range names {
		if skipSet[strings.ToLower(path.Base(m.Name))] {
			continue
		}
		rel := cleanMember(m.Name, strip)
		if rel == "" {
			continue
		}
		m.Dest = filepath.Join(dest, rel)
		want = append(want, m)
		size += m.Size
	}
	p.SetTotal(size)
	return downloads.ExtractRemoteZip(ctx, url, want, func(h int64) { p.SetDone(h) })
}

func namesOf(ms []downloads.ZipMember) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

// ── shared CUDA runtime ─────────────────────────────────────────────────────

func cudaStore() string { return filepath.Join(EnginesDir(), "cuda12") }

// provideCUDARuntime places the named CUDA DLLs into binDir: hard links to the
// shared store (no extra disk space), filled from llama.cpp's copy when
// present and otherwise extracted from the CUDA runtime bundle.
func provideCUDARuntime(ctx context.Context, names []string, binDir string, p EngineProgress) error {
	store := cudaStore()
	if err := os.MkdirAll(store, 0o755); err != nil {
		return err
	}
	var missing []string
	for _, n := range names {
		dst := filepath.Join(store, n)
		if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
			continue
		}
		if src := filepath.Join(LlamaDir(), n); fileExists(src) {
			if err := linkOrCopy(src, dst); err == nil {
				continue
			}
		}
		missing = append(missing, n)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		p.Phase("Downloading the CUDA runtime (" + strings.Join(missing, ", ") + ")…")
		if total, err := downloads.ZipMemberSizes(ctx, cuda12RuntimeURL, missing); err == nil {
			p.SetTotal(total)
		}
		if err := downloads.ExtractRemoteZipMembers(ctx, cuda12RuntimeURL, missing, store, func(h int64) { p.SetDone(h) }); err != nil {
			return err
		}
	}
	for _, n := range names {
		dst := filepath.Join(binDir, n)
		if fileExists(dst) {
			continue
		}
		if err := linkOrCopy(filepath.Join(store, n), dst); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

func linkOrCopy(src, dst string) error {
	os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	return os.Rename(tmp, dst)
}

// ── background jobs ─────────────────────────────────────────────────────────

// EngineJobKey is the download-manager key of an engine install.
func EngineJobKey(id string) string { return "engine:" + id }

// StartEngineInstall queues an install of id in the download manager and
// returns its job (joining one already running).
func StartEngineInstall(id string, hw *Hardware) (downloads.Job, error) {
	s := EngineByID(id)
	if s == nil {
		return downloads.Job{}, fmt.Errorf("unknown engine %q", id)
	}
	j, _ := downloads.Default().Start("engine", EngineJobKey(id), s.Name+" engine", map[string]string{"engine": id},
		engineTask(id, hw))
	return j, nil
}

func engineTask(id string, hw *Hardware) downloads.Task {
	return func(ctx context.Context, p *downloads.Progress) error {
		return InstallEngine(ctx, id, hw, p)
	}
}

// RegisterEngineResumer lets interrupted engine installs continue after a
// restart.
func RegisterEngineResumer(hw func() *Hardware) {
	downloads.Default().RegisterKind("engine", func(key string, meta map[string]string) (downloads.Task, error) {
		id := meta["engine"]
		if id == "" {
			id = strings.TrimPrefix(key, "engine:")
		}
		if EngineByID(id) == nil {
			return nil, fmt.Errorf("unknown engine %q", id)
		}
		return engineTask(id, hw()), nil
	})
}

// EnsureEngine returns the server path of engine id, installing it in the
// foreground when missing (used by runners that need it right now).
func EnsureEngine(ctx context.Context, id string, hw *Hardware) (string, error) {
	if p := EngineBin(id, ""); p != "" {
		return p, nil
	}
	j, err := StartEngineInstall(id, hw)
	if err != nil {
		return "", err
	}
	fin, err := downloads.Default().Wait(ctx, j.ID)
	if err != nil {
		return "", err
	}
	if fin.State != downloads.Done {
		if fin.Error != "" {
			return "", errors.New(fin.Error)
		}
		return "", fmt.Errorf("engine install %s", fin.State)
	}
	if p := EngineBin(id, ""); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("engine %s installed but its server is missing", id)
}
