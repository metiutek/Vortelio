package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// stable-diffusion.cpp is a managed engine (see engines.go, id "sdcpp"): a
// pinned prebuilt release installed into ~/.vortelio/engines/sdcpp/current.
// Older Vortelio versions dropped an unpinned build into ~/.vortelio/bin/sdcpp,
// which is still used when the managed engine is absent.

var sdcppBinNames = func() []string {
	if runtime.GOOS == "windows" {
		return []string{"sd-cli.exe", "sd.exe"}
	}
	return []string{"sd-cli", "sd"}
}()

// SDCppDir is the legacy install directory (pre engine manager).
func SDCppDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".vortelio", "bin", "sdcpp")
}

// SDCppBin returns the path to the stable-diffusion.cpp CLI, or "" if not installed.
func SDCppBin() string {
	if p := EngineBin("sdcpp", "sd-cli"); p != "" {
		return p
	}
	var dirs []string
	if d := SDCppDir(); d != "" {
		dirs = append(dirs, d, filepath.Dir(d))
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

// InstallSDCpp installs the pinned stable-diffusion.cpp engine (shown in the
// Downloads panel) and waits for it.
func InstallSDCpp(hw *Hardware) error {
	_, err := EnsureEngine(context.Background(), "sdcpp", hw)
	return err
}
