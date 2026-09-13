package runtime

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// stable-diffusion.cpp ships its CLI as `sd-cli` (older releases: `sd`) plus a
// set of shared libraries (ggml*.dll / stable-diffusion.dll / libwebp…). The
// whole archive must be extracted next to the binary or it will not start, so
// everything lives in its own directory: ~/.vortelio/bin/sdcpp/.

var sdcppBinNames = func() []string {
	if runtime.GOOS == "windows" {
		return []string{"sd-cli.exe", "sd.exe"}
	}
	return []string{"sd-cli", "sd"}
}()

// SDCppDir is the install directory for the stable-diffusion.cpp distribution.
func SDCppDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".vortelio", "bin", "sdcpp")
}

// SDCppBin returns the path to the stable-diffusion.cpp CLI, or "" if not installed.
func SDCppBin() string {
	var dirs []string
	if d := SDCppDir(); d != "" {
		dirs = append(dirs, d)
		// Legacy layout: the binary used to be dropped straight into bin/.
		dirs = append(dirs, filepath.Dir(d))
	}
	for _, dir := range dirs {
		for _, name := range sdcppBinNames {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	for _, name := range sdcppBinNames {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// InstallSDCpp downloads the latest stable-diffusion.cpp release into ~/.vortelio/bin/sdcpp/.
func InstallSDCpp(hw *Hardware) error {
	rel, err := sdcppLatestRelease()
	if err != nil {
		return fmt.Errorf("cannot query releases: %w", err)
	}

	url, assetName, err := sdcppPickAsset(rel, hw)
	if err != nil {
		return err
	}

	dest := SDCppDir()
	if dest == "" {
		return fmt.Errorf("cannot determine the home directory")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	fmt.Printf("📦  Download stable-diffusion.cpp (%s)...\n", assetName)
	if err := sdcppFetchAndExtract(url, dest); err != nil {
		return err
	}

	// A CUDA build needs the CUDA runtime DLLs, shipped in a separate 500 MB
	// asset. llama.cpp already ships the same ones in bin/, so reuse those when
	// they are there and only download as a last resort.
	if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(assetName), "cuda") {
		if !sdcppCopyCUDARuntime(dest) {
			if curl, cname := sdcppFindAsset(rel, assetRule{include: []string{"cudart", "win"}}); curl != "" {
				fmt.Printf("📦  Download CUDA runtime (%s)...\n", cname)
				if err := sdcppFetchAndExtract(curl, dest); err != nil {
					fmt.Printf("⚠️   CUDA runtime download failed (%v) — generation may fall back to CPU.\n", err)
				}
			}
		}
	}

	bin := SDCppBin()
	if bin == "" {
		return fmt.Errorf("archive extracted but no %s binary found", strings.Join(sdcppBinNames, "/"))
	}
	fmt.Printf("✅  stable-diffusion.cpp installed: %s\n", bin)
	return nil
}

// sdcppCudaDLLs are the CUDA runtime libraries a CUDA sd build links against.
var sdcppCudaDLLs = []string{"cudart64_12.dll", "cublas64_12.dll", "cublasLt64_12.dll"}

// sdcppCopyCUDARuntime copies the CUDA runtime DLLs from the sibling bin/
// directory (installed with llama.cpp) into dest. Reports whether all of them
// are now present.
func sdcppCopyCUDARuntime(dest string) bool {
	src := filepath.Dir(dest) // ~/.vortelio/bin
	for _, name := range sdcppCudaDLLs {
		dst := filepath.Join(dest, name)
		if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
			continue
		}
		in, err := os.Open(filepath.Join(src, name))
		if err != nil {
			return false
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			in.Close()
			return false
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			os.Remove(dst)
			return false
		}
	}
	fmt.Println("✅  CUDA runtime reused from the existing Vortelio bin directory.")
	return true
}

// sdcppFetchAndExtract downloads a zip and unpacks every file into dest (flat).
func sdcppFetchAndExtract(url, dest string) error {
	client := &http.Client{Timeout: 45 * time.Minute}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vortelio")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "sdcpp-*.zip")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download write error: %w", err)
	}
	tmp.Close()

	return sdcppExtractAll(tmpPath, dest)
}

// sdcppExtractAll unpacks every regular file of the zip into dest, flattening the
// archive layout (releases nest everything under a build/bin/ prefix).
func sdcppExtractAll(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("cannot open zip: %w", err)
	}
	defer r.Close()

	extracted := 0
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(filepath.FromSlash(f.Name))
		if name == "" || name == "." || name == ".." {
			continue
		}
		out := filepath.Join(dest, name)
		// Defensive: Base() already strips any path, so out can't escape dest.
		if !strings.HasPrefix(out, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(strings.ToLower(name), ".exe") || f.FileInfo().Mode()&0o111 != 0 {
			mode = 0o755
		}
		w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(w, rc) //nolint:gosec // release archive from a pinned repo
		rc.Close()
		w.Close()
		if err != nil {
			return err
		}
		extracted++
	}
	if extracted == 0 {
		return fmt.Errorf("archive is empty")
	}
	return nil
}

type ghRelease struct {
	Assets []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func sdcppLatestRelease() (*ghRelease, error) {
	req, err := http.NewRequest("GET", "https://api.github.com/repos/leejet/stable-diffusion.cpp/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vortelio")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if len(rel.Assets) == 0 {
		return nil, fmt.Errorf("release has no assets")
	}
	return &rel, nil
}

// assetRule matches a release asset by substrings that must (or must not) appear.
type assetRule struct {
	include []string
	exclude []string
}

func sdcppFindAsset(rel *ghRelease, rule assetRule) (url, name string) {
	for _, a := range rel.Assets {
		lower := strings.ToLower(a.Name)
		if !strings.HasSuffix(lower, ".zip") && !strings.HasSuffix(lower, ".tar.gz") {
			continue
		}
		ok := true
		for _, part := range rule.include {
			if !strings.Contains(lower, part) {
				ok = false
				break
			}
		}
		for _, part := range rule.exclude {
			if strings.Contains(lower, part) {
				ok = false
				break
			}
		}
		if ok {
			return a.BrowserDownloadURL, a.Name
		}
	}
	return "", ""
}

// sdcppPickAsset walks the platform rules in order and returns the first match.
func sdcppPickAsset(rel *ghRelease, hw *Hardware) (url, name string, err error) {
	for _, rule := range sdcppAssetRules(hw) {
		// Never pick the standalone CUDA runtime archive as the binary bundle.
		rule.exclude = append(rule.exclude, "cudart")
		if u, n := sdcppFindAsset(rel, rule); u != "" {
			return u, n, nil
		}
	}
	return "", "", fmt.Errorf("no compatible binary found in the release assets")
}

// sdcppAssetRules lists candidate asset patterns from best to fallback.
// Asset names look like: sd-master-<sha>-bin-win-cuda12-x64.zip,
// sd-master-<sha>-bin-Linux-Ubuntu-24.04-x86_64.zip,
// sd-master-<sha>-bin-Darwin-macOS-26.5.2-arm64.zip.
func sdcppAssetRules(hw *Hardware) []assetRule {
	goarch := runtime.GOARCH
	accel := []string{"cuda", "rocm", "vulkan"} // exclusions for the plain CPU build

	switch runtime.GOOS {
	case "windows":
		var rules []assetRule
		if goarch == "arm64" {
			return []assetRule{
				{include: []string{"win", "arm64"}},
				{include: []string{"win"}, exclude: accel},
			}
		}
		if hw != nil && hw.Backend == BackendCUDA {
			rules = append(rules, assetRule{include: []string{"win", "cuda", "x64"}})
		}
		if hw != nil && hw.Backend == BackendROCm {
			rules = append(rules, assetRule{include: []string{"win", "rocm", "x64"}})
		}
		rules = append(rules,
			assetRule{include: []string{"win", "cpu", "x64"}},
			assetRule{include: []string{"win", "x64"}, exclude: accel},
			assetRule{include: []string{"win", "vulkan", "x64"}},
		)
		return rules

	case "darwin":
		arch := "x86_64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		return []assetRule{
			{include: []string{"darwin", arch}},
			{include: []string{"macos", arch}},
			{include: []string{"osx", arch}},
			{include: []string{"darwin"}},
			{include: []string{"macos"}},
		}

	default: // linux
		var rules []assetRule
		if hw != nil && hw.Backend == BackendCUDA {
			rules = append(rules, assetRule{include: []string{"linux", "cuda"}})
		}
		if hw != nil && hw.Backend == BackendROCm {
			rules = append(rules, assetRule{include: []string{"linux", "rocm"}})
		}
		arch := "x86_64"
		if goarch == "arm64" {
			arch = "arm64"
		}
		rules = append(rules,
			assetRule{include: []string{"linux", arch}, exclude: accel},
			assetRule{include: []string{"linux", "x64"}, exclude: accel},
			assetRule{include: []string{"linux", "vulkan"}},
		)
		return rules
	}
}
