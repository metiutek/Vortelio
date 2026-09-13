package runtime

import (
	"runtime"
	"testing"
)

// realAssets mirrors the asset list of a stable-diffusion.cpp release
// (master-813-bfbef5b). The old matcher looked for "avx2"/"osx" substrings that
// no release ever had, so it silently fell back to the CUDA runtime archive —
// which contains no sd binary at all.
func realAssets() *ghRelease {
	names := []string{
		"cudart-sd-bin-win-cu12-x64.zip",
		"sd-master-bfbef5b-bin-Darwin-macOS-26.5.2-arm64.zip",
		"sd-master-bfbef5b-bin-Linux-Ubuntu-24.04-x86_64-rocm-7.14.0.zip",
		"sd-master-bfbef5b-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip",
		"sd-master-bfbef5b-bin-Linux-Ubuntu-24.04-x86_64.zip",
		"sd-master-bfbef5b-bin-win-cpu-x64.zip",
		"sd-master-bfbef5b-bin-win-cuda12-x64.zip",
		"sd-master-bfbef5b-bin-win-rocm-7.14.0-x64.zip",
		"sd-master-bfbef5b-bin-win-vulkan-x64.zip",
	}
	rel := &ghRelease{}
	for _, n := range names {
		rel.Assets = append(rel.Assets, struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{Name: n, BrowserDownloadURL: "https://example.invalid/" + n})
	}
	return rel
}

func TestPickAssetPerPlatform(t *testing.T) {
	rel := realAssets()

	cases := []struct {
		name    string
		hw      *Hardware
		wantFor map[string]string // GOOS → expected asset (only the current GOOS is checked)
	}{
		{
			name: "cuda",
			hw:   &Hardware{Backend: BackendCUDA},
			wantFor: map[string]string{
				"windows": "sd-master-bfbef5b-bin-win-cuda12-x64.zip",
				"linux":   "sd-master-bfbef5b-bin-Linux-Ubuntu-24.04-x86_64.zip",
				"darwin":  "sd-master-bfbef5b-bin-Darwin-macOS-26.5.2-arm64.zip",
			},
		},
		{
			name: "cpu",
			hw:   &Hardware{Backend: BackendCPU},
			wantFor: map[string]string{
				"windows": "sd-master-bfbef5b-bin-win-cpu-x64.zip",
				"linux":   "sd-master-bfbef5b-bin-Linux-Ubuntu-24.04-x86_64.zip",
				"darwin":  "sd-master-bfbef5b-bin-Darwin-macOS-26.5.2-arm64.zip",
			},
		},
		{
			name: "rocm",
			hw:   &Hardware{Backend: BackendROCm},
			wantFor: map[string]string{
				"windows": "sd-master-bfbef5b-bin-win-rocm-7.14.0-x64.zip",
				"linux":   "sd-master-bfbef5b-bin-Linux-Ubuntu-24.04-x86_64-rocm-7.14.0.zip",
				"darwin":  "sd-master-bfbef5b-bin-Darwin-macOS-26.5.2-arm64.zip",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, ok := tc.wantFor[runtime.GOOS]
			if !ok {
				t.Skipf("no expectation for GOOS=%s", runtime.GOOS)
			}
			// darwin/linux expectations assume the arch of the published assets
			if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
				t.Skip("release only ships an arm64 macOS build")
			}
			if runtime.GOOS != "darwin" && runtime.GOARCH != "amd64" {
				t.Skipf("no expectation for GOARCH=%s", runtime.GOARCH)
			}
			_, got, err := sdcppPickAsset(rel, tc.hw)
			if err != nil {
				t.Fatalf("sdcppPickAsset: %v", err)
			}
			if got != want {
				t.Fatalf("picked %q, want %q", got, want)
			}
		})
	}
}

// The CUDA runtime archive holds no sd binary; picking it made the install
// fail with "sd binary not found in zip archive".
func TestPickAssetNeverPicksCudart(t *testing.T) {
	rel := realAssets()
	for _, hw := range []*Hardware{{Backend: BackendCPU}, {Backend: BackendCUDA}, {Backend: BackendROCm}, {Backend: BackendMetal}} {
		if _, name, err := sdcppPickAsset(rel, hw); err == nil && name == "cudart-sd-bin-win-cu12-x64.zip" {
			t.Fatalf("backend %v picked the CUDA runtime archive", hw.Backend)
		}
	}
}

func TestSamplingProgressParsing(t *testing.T) {
	tests := []struct {
		line        string
		wantCur     int
		wantTotal   int
		wantNoMatch bool
	}{
		{line: "  |=========>      | 2/4 - 5.59s/it", wantCur: 2, wantTotal: 4},
		{line: "|==================| 20/20 - 1.20it/s", wantCur: 20, wantTotal: 20},
		// Tensor loading bars use MB/s and must not be read as sampling steps.
		{line: "  |##################| 196/196 - 801.09MB/s", wantNoMatch: true},
		{line: "[INFO ] stable-diffusion.cpp:5592 - generate_image 256x256", wantNoMatch: true},
	}
	for _, tt := range tests {
		m := sdSamplingRe.FindStringSubmatch(tt.line)
		if tt.wantNoMatch {
			if m != nil {
				t.Errorf("line %q matched but should not", tt.line)
			}
			continue
		}
		if m == nil {
			t.Errorf("line %q did not match", tt.line)
			continue
		}
		if m[1] != itoa(tt.wantCur) || m[2] != itoa(tt.wantTotal) {
			t.Errorf("line %q → %s/%s, want %d/%d", tt.line, m[1], m[2], tt.wantCur, tt.wantTotal)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
