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
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// llama.cpp is not bundled: it is downloaded from the upstream nightly releases
// (github.com/ggml-org/llama.cpp, tags b1234…) into ~/.vortelio/bin and kept up
// to date, so new model architectures work without a Vortelio release.

const llamaReleasesAPI = "https://api.github.com/repos/ggml-org/llama.cpp/releases?per_page=15"

// llamaInstallMu is held for writing while binaries are being replaced, and for
// reading while a llama-server is being launched, so a chat never starts a
// half-copied install.
var llamaInstallMu sync.RWMutex

// LlamaDir is where Vortelio installs llama.cpp.
func LlamaDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".vortelio", "bin")
}

func llamaServerName() string {
	if runtime.GOOS == "windows" {
		return "llama-server.exe"
	}
	return "llama-server"
}

// LlamaServerBin returns the llama-server Vortelio manages, falling back to one
// on PATH. "" when none is installed.
func LlamaServerBin() string {
	if d := LlamaDir(); d != "" {
		p := filepath.Join(d, llamaServerName())
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(llamaServerName()); err == nil {
		return p
	}
	return ""
}

var llamaBuildRe = regexp.MustCompile(`build[: ]+(\d+)`)

// LlamaInstalledBuild reports the build number of an installed llama-server
// (e.g. 11012), or 0 when it cannot be determined.
func LlamaInstalledBuild(bin string) int {
	if bin == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Dir = filepath.Dir(bin)
	HideWindow(cmd)
	out, _ := cmd.CombinedOutput()
	if m := llamaBuildRe.FindSubmatch(out); m != nil {
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	return 0
}

type llamaAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type llamaRelease struct {
	TagName string       `json:"tag_name"`
	Assets  []llamaAsset `json:"assets"`
}

var llamaTagRe = regexp.MustCompile(`^b(\d+)$`)

// latestLlamaRelease returns the newest nightly that carries build assets. The
// "latest" endpoint can point at a version tag with no binaries, so the list is
// scanned instead.
func latestLlamaRelease() (*llamaRelease, int, error) {
	req, err := http.NewRequest("GET", llamaReleasesAPI, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vortelio")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}
	var rels []llamaRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, 0, err
	}
	for i := range rels {
		m := llamaTagRe.FindStringSubmatch(rels[i].TagName)
		if m == nil || len(rels[i].Assets) == 0 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		return &rels[i], n, nil
	}
	return nil, 0, errors.New("no llama.cpp build with binaries found")
}

// llamaAssetPatterns lists, best first, the asset names that suit this machine.
// NVIDIA → CUDA 12 (covers Maxwell…Blackwell; CUDA 13 dropped older cards),
// AMD → Vulkan (no ROCm version matching needed), otherwise CPU.
func llamaAssetPatterns(goos, goarch string, backend HardwareBackend) []*regexp.Regexp {
	var p []string
	switch goos {
	case "windows":
		arch := "x64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		if arch == "x64" {
			switch backend {
			case BackendCUDA:
				p = append(p, `-bin-win-cuda-12\.[\d.]+-x64\.zip$`)
			case BackendROCm:
				p = append(p, `-bin-win-vulkan-x64\.zip$`)
			}
		}
		p = append(p, `-bin-win-cpu-`+arch+`\.zip$`)
	case "darwin":
		arch := "x64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		p = append(p, `-bin-macos-`+arch+`\.(tar\.gz|zip)$`)
	default:
		arch := "x64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		if arch == "x64" {
			switch backend {
			case BackendCUDA:
				p = append(p, `-bin-ubuntu-cuda-12\.[\d.]+-x64\.tar\.gz$`)
			case BackendROCm:
				p = append(p, `-bin-ubuntu-vulkan-x64\.tar\.gz$`)
			}
		}
		p = append(p, `-bin-ubuntu-`+arch+`\.(tar\.gz|zip)$`)
	}
	out := make([]*regexp.Regexp, len(p))
	for i, s := range p {
		out[i] = regexp.MustCompile(`^llama-b\d+` + s)
	}
	return out
}

// pickLlamaAssets returns the binary archive for this machine and, for a CUDA
// build, the matching CUDA runtime archive (nil when not needed).
func pickLlamaAssets(rel *llamaRelease, goos, goarch string, backend HardwareBackend) (*llamaAsset, *llamaAsset) {
	for _, re := range llamaAssetPatterns(goos, goarch, backend) {
		for i := range rel.Assets {
			a := &rel.Assets[i]
			if !re.MatchString(a.Name) {
				continue
			}
			var cudart *llamaAsset
			if m := regexp.MustCompile(`-cuda-([\d.]+)-(x64|arm64)`).FindStringSubmatch(a.Name); m != nil {
				want := "-cuda-" + m[1] + "-" + m[2]
				for j := range rel.Assets {
					c := &rel.Assets[j]
					if strings.HasPrefix(c.Name, "cudart-") && strings.Contains(c.Name, want) {
						cudart = c
						break
					}
				}
			}
			return a, cudart
		}
	}
	return nil, nil
}

// LlamaStatus describes the installed llama.cpp and whether a newer one exists.
type LlamaStatus struct {
	Installed       bool   `json:"installed"`
	Path            string `json:"path,omitempty"`
	Build           int    `json:"build"`
	LatestBuild     int    `json:"latest_build"`
	UpdateAvailable bool   `json:"update_available"`
	Managed         bool   `json:"managed"` // installed by Vortelio in ~/.vortelio/bin
	CheckError      string `json:"check_error,omitempty"`
}

var (
	llamaLatestMu   sync.Mutex
	llamaLatestAt   time.Time
	llamaLatestN    int
	llamaLatestErr  error
	llamaLatestTTL  = 6 * time.Hour
	llamaLatestFail = 10 * time.Minute
)

func cachedLatestBuild() (int, error) {
	llamaLatestMu.Lock()
	defer llamaLatestMu.Unlock()
	ttl := llamaLatestTTL
	if llamaLatestErr != nil {
		ttl = llamaLatestFail
	}
	if !llamaLatestAt.IsZero() && time.Since(llamaLatestAt) < ttl {
		return llamaLatestN, llamaLatestErr
	}
	_, n, err := latestLlamaRelease()
	llamaLatestAt, llamaLatestN, llamaLatestErr = time.Now(), n, err
	return n, err
}

func invalidateLlamaLatest() {
	llamaLatestMu.Lock()
	llamaLatestAt = time.Time{}
	llamaLatestMu.Unlock()
}

// CheckLlama returns the current llama.cpp status (the upstream lookup is cached).
func CheckLlama() LlamaStatus {
	st := LlamaStatus{}
	st.Path = LlamaServerBin()
	st.Installed = st.Path != ""
	if st.Installed {
		st.Build = LlamaInstalledBuild(st.Path)
		if d := LlamaDir(); d != "" {
			st.Managed = filepath.Dir(st.Path) == d
		}
	}
	n, err := cachedLatestBuild()
	if err != nil {
		st.CheckError = err.Error()
	}
	st.LatestBuild = n
	st.UpdateAvailable = n > 0 && (!st.Installed || (st.Build > 0 && st.Build < n))
	return st
}

// InstallLlamaCpp downloads the newest llama.cpp for this machine into
// LlamaDir, replacing an older install. Any llama-server started by this process
// must be stopped first (GlobalModelManager.UnloadAll). progress may be nil.
func InstallLlamaCpp(hw *Hardware, progress func(msg string, pct float64)) (int, error) {
	if progress == nil {
		progress = func(string, float64) {}
	}
	if hw == nil {
		hw = DetectHardware()
	}
	dir := LlamaDir()
	if dir == "" {
		return 0, errors.New("cannot determine the home directory")
	}
	progress("Looking for the latest llama.cpp…", 0)
	rel, build, err := latestLlamaRelease()
	if err != nil {
		return 0, fmt.Errorf("cannot query llama.cpp releases: %w", err)
	}
	bin, cudart := pickLlamaAssets(rel, runtime.GOOS, runtime.GOARCH, hw.Backend)
	if bin == nil {
		return 0, fmt.Errorf("llama.cpp %s has no build for %s/%s", rel.TagName, runtime.GOOS, runtime.GOARCH)
	}

	stage, err := os.MkdirTemp(filepath.Dir(dir), ".llama-stage-")
	if err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return 0, err
		}
		if stage, err = os.MkdirTemp(filepath.Dir(dir), ".llama-stage-"); err != nil {
			return 0, err
		}
	}
	defer os.RemoveAll(stage)

	if err := fetchAndExtract(bin, stage, func(p float64) {
		progress(fmt.Sprintf("Downloading llama.cpp %s…", rel.TagName), p*0.9)
	}); err != nil {
		return 0, err
	}
	// The CUDA runtime (cudart/cublas) is large and rarely changes: only fetch it
	// when it is not already installed.
	if cudart != nil && !hasCudaRuntime(dir) {
		if err := fetchAndExtract(cudart, stage, func(p float64) {
			progress("Downloading the CUDA runtime…", 0.9+p*0.05)
		}); err != nil {
			return 0, fmt.Errorf("CUDA runtime: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(stage, llamaServerName())); err != nil {
		return 0, fmt.Errorf("%s does not contain %s", bin.Name, llamaServerName())
	}

	progress("Installing…", 0.96)
	llamaInstallMu.Lock()
	defer llamaInstallMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	if err := moveInto(stage, dir); err != nil {
		return 0, err
	}
	llamaLatestMu.Lock()
	llamaLatestAt, llamaLatestN, llamaLatestErr = time.Now(), build, nil
	llamaLatestMu.Unlock()
	progress(fmt.Sprintf("llama.cpp %s installed", rel.TagName), 1)
	return build, nil
}

func hasCudaRuntime(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if strings.HasPrefix(n, "cudart64_") || strings.HasPrefix(n, "libcudart.so") {
			return true
		}
	}
	return false
}

// fetchAndExtract downloads an archive and unpacks it flat into dest.
func fetchAndExtract(a *llamaAsset, dest string, progress func(float64)) error {
	req, err := http.NewRequest("GET", a.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vortelio")
	resp, err := (&http.Client{Timeout: 60 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "llama-*.download")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	total := resp.ContentLength
	if total <= 0 {
		total = a.Size
	}
	var done int64
	buf := make([]byte, 256*1024)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				tmp.Close()
				return werr
			}
			done += int64(n)
			if total > 0 && time.Since(last) > 250*time.Millisecond {
				last = time.Now()
				progress(float64(done) / float64(total))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tmp.Close()
			return fmt.Errorf("download interrupted: %w", rerr)
		}
	}
	tmp.Close()
	progress(1)

	if strings.HasSuffix(a.Name, ".zip") {
		return extractZipFlat(tmpPath, dest)
	}
	return extractTarGzFlat(tmpPath, dest)
}

// safeBase keeps only the file name of an archive entry.
func safeBase(name string) string {
	b := filepath.Base(filepath.FromSlash(name))
	if b == "." || b == ".." || b == string(os.PathSeparator) {
		return ""
	}
	return b
}

func extractZipFlat(path, dest string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("cannot open archive: %w", err)
	}
	defer r.Close()
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := safeBase(f.Name)
		if name == "" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(filepath.Join(dest, name), rc, f.Mode()|0o644)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// extractTarGzFlat unpacks a .tar.gz flat into dest. Linux/macOS builds ship
// their shared libraries as symlink chains (libllama.so → libllama.so.0 → …),
// which are recreated as-is.
func extractTarGzFlat(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("cannot open archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := safeBase(h.Name)
		if name == "" {
			continue
		}
		out := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeReg:
			if err := writeFile(out, tr, os.FileMode(h.Mode)|0o644); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target := safeBase(h.Linkname)
			if target == "" {
				continue
			}
			os.Remove(out)
			if err := os.Symlink(target, out); err != nil {
				return err
			}
		}
	}
}

func writeFile(path string, r io.Reader, mode os.FileMode) error {
	w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r) //nolint:gosec // release archive from a pinned repo
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// moveInto moves every entry of src into dst, replacing existing files. On
// Windows a just-killed llama-server can hold its DLLs for a moment, so each
// replacement is retried briefly.
func moveInto(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		var lastErr error
		for try := 0; try < 20; try++ {
			if fi, err := os.Lstat(to); err == nil && !fi.IsDir() {
				if err := os.Remove(to); err != nil {
					lastErr = err
					time.Sleep(250 * time.Millisecond)
					continue
				}
			}
			if lastErr = os.Rename(from, to); lastErr == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if lastErr != nil {
			return fmt.Errorf("cannot replace %s (is a model still running?): %w", e.Name(), lastErr)
		}
	}
	return nil
}

var (
	autoUpdateOnce sync.Once
)

// AutoUpdateLlama installs llama.cpp when missing and keeps a Vortelio-managed
// install current. Meant to run once in the background at server start, before
// any model is loaded. Installs that Vortelio did not make (e.g. a system
// llama-server on PATH) are left alone.
func AutoUpdateLlama(hw *Hardware, logf func(format string, args ...any)) {
	autoUpdateOnce.Do(func() {
		if os.Getenv("VORTELIO_NO_LLAMA_UPDATE") != "" {
			return
		}
		st := CheckLlama()
		if st.CheckError != "" || !st.UpdateAvailable {
			return
		}
		if st.Installed && !st.Managed {
			return
		}
		if len(GlobalModelManager.ListLoaded()) > 0 {
			return
		}
		logf("llama.cpp: installing build %d (installed: %d)", st.LatestBuild, st.Build)
		if n, err := InstallLlamaCpp(hw, nil); err != nil {
			logf("llama.cpp update failed: %v", err)
		} else {
			logf("llama.cpp: build %d ready", n)
		}
	})
}
