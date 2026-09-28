package runtime

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Asset names of the real llama.cpp release b11223.
var b11223 = []string{
	"cudart-llama-b11223-bin-ubuntu-cuda-12.8-x64.tar.gz",
	"cudart-llama-b11223-bin-ubuntu-cuda-13.4-x64.tar.gz",
	"cudart-llama-bin-win-cuda-12.4-x64.zip",
	"cudart-llama-bin-win-cuda-13.4-x64.zip",
	"llama-b11223-bin-linux-arm64-snapdragon.tar.gz",
	"llama-b11223-bin-macos-arm64.tar.gz",
	"llama-b11223-bin-macos-x64.tar.gz",
	"llama-b11223-bin-ubuntu-arm64.tar.gz",
	"llama-b11223-bin-ubuntu-cuda-12.8-x64.tar.gz",
	"llama-b11223-bin-ubuntu-cuda-13.4-x64.tar.gz",
	"llama-b11223-bin-ubuntu-openvino-2026.4-x64.tar.gz",
	"llama-b11223-bin-ubuntu-rocm-10.0-x64.tar.gz",
	"llama-b11223-bin-ubuntu-vulkan-x64.tar.gz",
	"llama-b11223-bin-ubuntu-x64.tar.gz",
	"llama-b11223-bin-win-cpu-arm64.zip",
	"llama-b11223-bin-win-cpu-x64.zip",
	"llama-b11223-bin-win-cuda-12.4-x64.zip",
	"llama-b11223-bin-win-cuda-13.4-x64.zip",
	"llama-b11223-bin-win-vulkan-x64.zip",
	"llama-b11223-ui.tar.gz",
}

func TestPickLlamaAssets(t *testing.T) {
	rel := &llamaRelease{TagName: "b11223"}
	for _, n := range b11223 {
		rel.Assets = append(rel.Assets, llamaAsset{Name: n})
	}
	cases := []struct {
		goos, goarch string
		backend      HardwareBackend
		bin, cudart  string
	}{
		{"windows", "amd64", BackendCUDA, "llama-b11223-bin-win-cuda-12.4-x64.zip", "cudart-llama-bin-win-cuda-12.4-x64.zip"},
		{"windows", "amd64", BackendCPU, "llama-b11223-bin-win-cpu-x64.zip", ""},
		{"windows", "amd64", BackendROCm, "llama-b11223-bin-win-vulkan-x64.zip", ""},
		{"windows", "arm64", BackendCPU, "llama-b11223-bin-win-cpu-arm64.zip", ""},
		{"darwin", "arm64", BackendMetal, "llama-b11223-bin-macos-arm64.tar.gz", ""},
		{"darwin", "amd64", BackendCPU, "llama-b11223-bin-macos-x64.tar.gz", ""},
		{"linux", "amd64", BackendCPU, "llama-b11223-bin-ubuntu-x64.tar.gz", ""},
		{"linux", "amd64", BackendCUDA, "llama-b11223-bin-ubuntu-cuda-12.8-x64.tar.gz", "cudart-llama-b11223-bin-ubuntu-cuda-12.8-x64.tar.gz"},
		{"linux", "amd64", BackendROCm, "llama-b11223-bin-ubuntu-vulkan-x64.tar.gz", ""},
		{"linux", "arm64", BackendCPU, "llama-b11223-bin-ubuntu-arm64.tar.gz", ""},
	}
	for _, c := range cases {
		bin, cudart := pickLlamaAssets(rel, c.goos, c.goarch, c.backend)
		if bin == nil || bin.Name != c.bin {
			t.Errorf("%s/%s/%v: bin = %v, want %s", c.goos, c.goarch, c.backend, bin, c.bin)
		}
		got := ""
		if cudart != nil {
			got = cudart.Name
		}
		if got != c.cudart {
			t.Errorf("%s/%s/%v: cudart = %q, want %q", c.goos, c.goarch, c.backend, got, c.cudart)
		}
	}
}

// TestInstallLlamaCppLive downloads the real CPU build into a throwaway home and
// runs it. Network-heavy, so opt-in: VORTELIO_LIVE_TEST=1.
func TestInstallLlamaCppLive(t *testing.T) {
	if os.Getenv("VORTELIO_LIVE_TEST") == "" {
		t.Skip("set VORTELIO_LIVE_TEST=1 to run")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	build, err := InstallLlamaCpp(&Hardware{Backend: BackendCPU}, func(m string, p float64) {})
	if err != nil {
		t.Fatal(err)
	}
	bin := LlamaServerBin()
	if bin != filepath.Join(home, ".vortelio", "bin", llamaServerName()) {
		t.Fatalf("installed server not found in the managed dir: %q", bin)
	}
	if got := LlamaInstalledBuild(bin); got != build {
		t.Fatalf("llama-server --version reports build %d, installed %d", got, build)
	}
	t.Logf("installed and ran llama.cpp build %d", build)
}

func TestExtractTarGzFlatKeepsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows; tarballs are only used on unix")
	}
	dir := t.TempDir()
	arc := filepath.Join(dir, "a.tar.gz")
	f, _ := os.Create(arc)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("bin")
	tw.WriteHeader(&tar.Header{Name: "llama-b1/llama-server", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.WriteHeader(&tar.Header{Name: "llama-b1/libllama.so", Linkname: "libllama.so.0", Typeflag: tar.TypeSymlink})
	tw.Close()
	gz.Close()
	f.Close()

	out := filepath.Join(dir, "out")
	os.Mkdir(out, 0o755)
	if err := extractTarGzFlat(arc, out); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(out, "llama-server")); err != nil || st.Mode()&0o100 == 0 {
		t.Fatalf("llama-server missing or not executable: %v", err)
	}
	if l, err := os.Readlink(filepath.Join(out, "libllama.so")); err != nil || l != "libllama.so.0" {
		t.Fatalf("symlink = %q, %v", l, err)
	}
}
