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

// Windows process creation flags. Launching the updater with these fully
// detaches it from the terminal's console, so the update keeps running (and the
// terminal does NOT close) after `vortelio update` exits.
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

func StartDetached(restartGUI bool) (StartResult, error) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return StartResult{}, errors.New("uv non trovato nel PATH. Installa uv e riprova")
	}
	return startWindowsUpdater(uv, os.Getpid(), restartGUI, filepath.Join(os.TempDir(), "vortelio-update.log"))
}

func startWindowsUpdater(uv string, pid int, restartGUI bool, logPath string) (StartResult, error) {
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("vortelio-update-%d.ps1", pid))
	restartLine := ""
	if restartGUI {
		restartLine = "if ($code -eq 0) { Start-Process -FilePath 'vortelio' -ArgumentList 'gui' -WindowStyle Hidden }\n"
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Continue'
$log = %q
"Vortelio update started at $(Get-Date -Format o)" | Out-File -FilePath $log -Encoding utf8
try { Wait-Process -Id %d -ErrorAction SilentlyContinue } catch {}
Start-Sleep -Milliseconds 800
# Stop every running Vortelio process (the background server locks the install
# files; otherwise uv fails with "Access denied" and corrupts the install).
Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -like '*vortelio*' } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 800
$code = 1
for ($i = 0; $i -lt 3 -and $code -ne 0; $i++) {
  & %q tool install --reinstall --refresh %q *>> $log
  $code = $LASTEXITCODE
  if ($code -ne 0) { Start-Sleep -Seconds 2 }
}
"Exit code: $code" | Out-File -FilePath $log -Append -Encoding utf8
%sexit $code
`, logPath, pid, uv, RepoInstallSpec, restartLine)
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		return StartResult{}, err
	}

	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden", "-File", scriptPath)
	// Detach from the parent console: the updater outlives `vortelio update`
	// and the terminal stays open (it used to die with the shared console,
	// which is why the update never completed).
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
	if err := cmd.Start(); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "powershell") {
			return StartResult{}, fmt.Errorf("impossibile avviare PowerShell per l'aggiornamento: %w", err)
		}
		return StartResult{}, err
	}
	return StartResult{
		Started: true,
		Message: "Aggiornamento avviato. Vortelio si chiudera' e uv installera' la nuova versione.",
		LogPath: logPath,
	}, nil
}
