package runtime

import (
	"context"
	"os"
	"testing"
)

func relOf(names ...string) *ghTagRelease {
	r := &ghTagRelease{}
	for _, n := range names {
		r.Assets = append(r.Assets, ghAsset{Name: n})
	}
	return r
}

func TestPickEngineAsset(t *testing.T) {
	sd := relOf(
		"sd-master-968-8cd731e-ubdb47d1-bin-win-cpu-x64.zip",
		"sd-master-968-8cd731e-ubdb47d1-bin-win-cuda12-x64.zip",
		"sd-master-968-8cd731e-ubdb47d1-bin-win-cuda13-x64.zip",
		"sd-master-968-8cd731e-ubdb47d1-bin-win-vulkan-x64.zip",
		"sd-master-968-8cd731e-ubdb47d1-bin-Linux-Ubuntu-22.04-x86_64.zip",
		"sd-master-968-8cd731e-ubdb47d1-bin-Linux-Ubuntu-22.04-x86_64-cuda12.zip",
	)
	audio := relOf(
		"audio-v0.9.2-x-bin-windows-x64-cpu-portable.zip",
		"audio-v0.9.2-x-bin-windows-x64-cpu.zip",
		"audio-v0.9.2-x-bin-windows-x64-cuda12.zip",
		"audio-v0.9.2-x-cudart-windows-x64-cuda12.zip",
		"audio-v0.9.2-x-bin-ubuntu-x64-cpu.tar.gz",
	)
	wh := relOf("whisper-bin-x64.zip", "whisper-blas-bin-x64.zip", "whisper-bin-win-cuda-12.4.0-x64.zip", "whisper-bin-ubuntu-x64.tar.gz")
	cases := []struct {
		id, goos string
		b        HardwareBackend
		rel      *ghTagRelease
		want     string
		accel    string
	}{
		{"sdcpp", "windows", BackendCUDA, sd, "sd-master-968-8cd731e-ubdb47d1-bin-win-cuda12-x64.zip", "cuda"},
		{"sdcpp", "windows", BackendCPU, sd, "sd-master-968-8cd731e-ubdb47d1-bin-win-cpu-x64.zip", "cpu"},
		{"sdcpp", "linux", BackendCUDA, sd, "sd-master-968-8cd731e-ubdb47d1-bin-Linux-Ubuntu-22.04-x86_64-cuda12.zip", "cuda"},
		{"sdcpp", "linux", BackendCPU, sd, "sd-master-968-8cd731e-ubdb47d1-bin-Linux-Ubuntu-22.04-x86_64.zip", "cpu"},
		{"audiocpp", "windows", BackendCUDA, audio, "audio-v0.9.2-x-bin-windows-x64-cuda12.zip", "cuda"},
		{"audiocpp", "windows", BackendCPU, audio, "audio-v0.9.2-x-bin-windows-x64-cpu.zip", "cpu"},
		{"audiocpp", "linux", BackendCPU, audio, "audio-v0.9.2-x-bin-ubuntu-x64-cpu.tar.gz", "cpu"},
		{"whisper", "windows", BackendCUDA, wh, "whisper-bin-win-cuda-12.4.0-x64.zip", "cuda"},
		{"whisper", "windows", BackendCPU, wh, "whisper-blas-bin-x64.zip", "cpu"},
		{"whisper", "linux", BackendCPU, wh, "whisper-bin-ubuntu-x64.tar.gz", "cpu"},
	}
	for _, c := range cases {
		a, accel := pickEngineAsset(EngineByID(c.id), c.rel, c.goos, "amd64", c.b)
		if a == nil || a.Name != c.want || accel != c.accel {
			got := "<nil>"
			if a != nil {
				got = a.Name
			}
			t.Errorf("%s %s %v: got %s/%s want %s/%s", c.id, c.goos, c.b, got, accel, c.want, c.accel)
		}
	}
}

func TestCleanMember(t *testing.T) {
	if r := commonRoot([]string{"Release/a.exe", "Release/b.dll"}); r != "Release" {
		t.Fatal(r)
	}
	if r := commonRoot([]string{"a.exe", "model_specs/x.json"}); r != "" {
		t.Fatal(r)
	}
	if m := cleanMember("../../evil.dll", ""); m != "evil.dll" {
		t.Fatalf("traversal not neutralised: %q", m)
	}
	if m := cleanMember("Release/x/y.json", "Release"); m != "x"+string(os.PathSeparator)+"y.json" {
		t.Fatal(m)
	}
}

// Real install from GitHub: VORTELIO_ENGINE_IT=sdcpp,audiocpp,whisper go test -run TestEngineInstallLive
func TestEngineInstallLive(t *testing.T) {
	ids := os.Getenv("VORTELIO_ENGINE_IT")
	if ids == "" {
		t.Skip("set VORTELIO_ENGINE_IT")
	}
	hw := DetectHardware()
	for _, id := range splitComma(ids) {
		p := &logProgress{t: t, id: id}
		if err := InstallEngine(context.Background(), id, hw, p); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		st := GetEngineStatus(id)
		t.Logf("%s: %+v", id, st)
		if !st.Installed {
			t.Fatalf("%s not installed", id)
		}
	}
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s + "," {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	return out
}

type logProgress struct {
	t      *testing.T
	id     string
	total  int64
	lastPc int64
}

func (l *logProgress) Phase(s string)   { l.t.Logf("[%s] %s", l.id, s) }
func (l *logProgress) SetTotal(n int64) { l.total, l.lastPc = n, -1 }
func (l *logProgress) SetDone(n int64) {
	if l.total > 0 {
		pc := n * 10 / l.total
		if pc != l.lastPc {
			l.lastPc = pc
			l.t.Logf("[%s] %d%% (%d MB)", l.id, pc*10, n>>20)
		}
	}
}
