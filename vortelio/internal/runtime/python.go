package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func FindPython() string {
	candidates := pythonCandidates()
	for _, p := range candidates {
		if isRealPython(p) {
			return p
		}
	}
	return ""
}

func isRealPython(path string) bool {
	if path == "" {
		return false
	}
	// Skip uv-managed / StabilityMatrix / conda base envs — can't pip install freely
	lower := strings.ToLower(path)
	for _, skip := range []string{"stabilitymatrix", "uv", "conda", "anaconda", "miniconda", "windowsapps"} {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	cmd := HideWindow(exec.Command(path, "--version"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "Python 3") {
		return false
	}
	// Make sure pip is usable (not externally managed without --break-system-packages)
	pipTest := HideWindow(exec.Command(path, "-m", "pip", "--version"))
	if err := pipTest.Run(); err != nil {
		return false
	}
	return true
}

func pythonCandidates() []string {
	var found []string
	for _, name := range []string{"python3", "python", "python3.12", "python3.11", "python3.10"} {
		if p, err := exec.LookPath(name); err == nil {
			if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(p), "windowsapps") {
				continue
			}
			found = append(found, p)
		}
	}
	if runtime.GOOS == "windows" {
		// py.exe launcher is the most reliable — check it FIRST
		for _, pyExe := range []string{"py", "python", "python3"} {
			if p, err := exec.LookPath(pyExe); err == nil {
				p = strings.ToLower(p)
				if !strings.Contains(p, "windowsapps") && !strings.Contains(p, "appdata\\local\\microsoft\\windowsapps") {
					found = append(found, p)
				}
			}
		}
		// Scan common install locations
		versions := []string{"314", "313", "312", "311", "310", "39", "38"}
		var winDirs []string
		for _, v := range versions {
			winDirs = append(winDirs,
				filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Python", "Python"+v),
				filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Python", "Python"+v+"-32"),
				filepath.Join("C:\\", "Python"+v),
				filepath.Join("C:\\", "Program Files", "Python"+v),
				filepath.Join("C:\\", "Program Files (x86)", "Python"+v),
				filepath.Join(os.Getenv("APPDATA"), "..", "Local", "Programs", "Python", "Python"+v),
			)
		}
		// Also scan user PATH manually for python*.exe
		for _, dir := range strings.Split(os.Getenv("PATH"), ";") {
			for _, name := range []string{"python.exe", "python3.exe"} {
				candidate := filepath.Join(strings.TrimSpace(dir), name)
				if _, err := os.Stat(candidate); err == nil {
					if !strings.Contains(strings.ToLower(candidate), "windowsapps") {
						found = append(found, candidate)
					}
				}
			}
		}
		for _, dir := range winDirs {
			candidate := filepath.Join(dir, "python.exe")
			if _, err := os.Stat(candidate); err == nil {
				found = append(found, candidate)
			}
		}
	} else if runtime.GOOS == "darwin" {
		found = append(found, "/opt/homebrew/bin/python3", "/usr/local/bin/python3", "/usr/bin/python3")
	} else {
		found = append(found, "/usr/bin/python3", "/usr/local/bin/python3")
	}
	return found
}

func InstallPythonPackage(pythonBin string, packages ...string) error {
	baseArgs := []string{"-m", "pip", "install", "--quiet", "--no-warn-conflicts"}
	// Try normal install first
	args := append(append([]string{}, baseArgs...), packages...)
	cmd := HideWindow(exec.Command(pythonBin, args...))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err == nil {
		return nil
	}
	// Try --break-system-packages (handles PEP 668 externally-managed envs)
	args2 := append(append([]string{}, baseArgs...), "--break-system-packages")
	args2 = append(args2, packages...)
	cmd2 := HideWindow(exec.Command(pythonBin, args2...))
	cmd2.Stdout = os.Stdout
	cmd2.Stderr = os.Stderr
	if err := cmd2.Run(); err == nil {
		return nil
	}
	// Try uv pip install (StabilityMatrix uses uv)
	uvPath, _ := exec.LookPath("uv")
	if uvPath == "" {
		uvPath, _ = exec.LookPath("uv.exe")
	}
	if uvPath != "" {
		uvArgs := append([]string{"pip", "install", "--quiet"}, packages...)
		cmd3 := HideWindow(exec.Command(uvPath, uvArgs...))
		cmd3.Stdout = os.Stdout
		cmd3.Stderr = os.Stderr
		if err := cmd3.Run(); err == nil {
			return nil
		}
	}
	// Final attempt with user install
	args3 := append(append([]string{}, baseArgs...), "--user")
	args3 = append(args3, packages...)
	return RunWithOutput(HideWindow(exec.Command(pythonBin, args3...)), os.Stdout, os.Stderr)
}

func CheckPythonPackage(pythonBin, pkg string) bool {
	cmd := HideWindow(exec.Command(pythonBin, "-c", fmt.Sprintf("import %s", pkg)))
	return cmd.Run() == nil
}

// escapePy makes a string safe to embed inside a Python literal delimited by
// """…""" or '''…'''. Both quote styles are escaped: a prompt containing ''' or
// """ would otherwise close the literal early and break (or inject into) the
// generated script.
func escapePy(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
