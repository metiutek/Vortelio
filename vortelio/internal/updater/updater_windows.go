//go:build windows

package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Windows process creation flags.
const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
)

func StartDetached(restartGUI bool) (StartResult, error) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return StartResult{}, errors.New("uv non trovato nel PATH. Installa uv e riprova")
	}
	return startWindowsUpdater(uv, os.Getpid(), restartGUI, filepath.Join(os.TempDir(), "vortelio-update.log"))
}

// psQuote returns s as a PowerShell single-quoted literal (no escapes, no
// variable expansion; embedded quotes are doubled).
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func powershellExe() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "powershell.exe"
}

func startWindowsUpdater(uv string, pid int, restartGUI bool, logPath string) (StartResult, error) {
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("vortelio-update-%d.ps1", pid))
	restartLine := ""
	if restartGUI {
		// Absolute path: the updater runs outside our process tree (see below)
		// and may not see the same PATH.
		exe := "vortelio"
		if p, err := exec.LookPath("vortelio"); err == nil {
			exe = p
		}
		restartLine = fmt.Sprintf("if ($code -eq 0) { Start-Process -FilePath %s -ArgumentList 'gui' -WindowStyle Hidden }\n", psQuote(exe))
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Continue'
$log = %s
"Vortelio update started at $(Get-Date -Format o)" | Out-File -FilePath $log -Encoding utf8
try { Wait-Process -Id %d -Timeout 30 -ErrorAction SilentlyContinue } catch {}
Start-Sleep -Milliseconds 800
# Stop every running Vortelio process (the background server locks the install
# files; otherwise uv fails with "Access denied" and corrupts the install).
Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -like '*vortelio*' } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 800
$code = 1
for ($i = 0; $i -lt 3 -and $code -ne 0; $i++) {
  & %s tool install --reinstall --refresh %s 2>&1 | ForEach-Object { "$_" } | Out-File -FilePath $log -Append -Encoding utf8
  $code = $LASTEXITCODE
  if ($code -ne 0) { Start-Sleep -Seconds 2 }
}
"Exit code: $code" | Out-File -FilePath $log -Append -Encoding utf8
%sexit $code
`, psQuote(logPath), pid, psQuote(uv), psQuote(RepoInstallSpec), restartLine)
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		return StartResult{}, err
	}

	ps := powershellExe()
	cmdLine := fmt.Sprintf(`"%s" -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "%s"`, ps, scriptPath)

	// Vortelio runs inside the Windows job object created by uv's tool
	// launcher (vortelio.exe → python → binary). Every child we spawn inherits
	// that job, and the job is torn down when Vortelio exits — which killed the
	// updater before it even wrote its log, so updates silently did nothing.
	// Win32_Process.Create spawns the updater from the WMI service, outside our
	// job and process tree, so it survives our exit.
	if err := startViaWMI(ps, cmdLine); err == nil {
		return started(logPath), nil
	}

	// Fallback: ask to break away from the job (allowed only if the job permits
	// it), then plain detached as a last resort.
	for _, flags := range []uint32{
		detachedProcess | createNewProcessGroup | createBreakawayFromJob,
		detachedProcess | createNewProcessGroup,
	} {
		cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden", "-File", scriptPath)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		if err := cmd.Start(); err == nil {
			return started(logPath), nil
		} else if flags&createBreakawayFromJob == 0 {
			return StartResult{}, fmt.Errorf("impossibile avviare PowerShell per l'aggiornamento: %w", err)
		}
	}
	return StartResult{}, errors.New("impossibile avviare l'aggiornamento")
}

// startViaWMI launches cmdLine through Win32_Process.Create and waits only for
// the launch itself (not for the launched process).
func startViaWMI(ps, cmdLine string) error {
	launcher := `$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=` + psQuote(cmdLine) + `}; exit [int]$r.ReturnValue`
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", launcher)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("WMI launch failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func started(logPath string) StartResult {
	return StartResult{
		Started: true,
		Message: "Aggiornamento avviato. Vortelio si chiudera' e uv installera' la nuova versione.",
		LogPath: logPath,
	}
}
