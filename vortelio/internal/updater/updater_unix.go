//go:build !windows

package updater

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// shellQuote wraps s in single quotes for POSIX sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func StartDetached(restartGUI bool) (StartResult, error) {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return StartResult{}, errors.New("uv non trovato nel PATH. Installa uv e riprova")
	}
	return startUnixUpdater(uv, os.Getpid(), restartGUI, filepath.Join(os.TempDir(), "vortelio-update.log"))
}

func startUnixUpdater(uv string, pid int, restartGUI bool, logPath string) (StartResult, error) {
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("vortelio-update-%d.sh", pid))
	restartLine := ""
	if restartGUI {
		restartLine = "if [ \"$code\" -eq 0 ]; then nohup vortelio gui >/dev/null 2>&1 & fi\n"
	}
	script := fmt.Sprintf(`#!/bin/sh
log=%s
echo "Vortelio update started at $(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" > "$log"
while kill -0 %d 2>/dev/null; do sleep 0.2; done
sleep 1
%s tool install --reinstall --refresh %s >> "$log" 2>&1
code=$?
echo "Exit code: $code" >> "$log"
%sexit "$code"
`, shellQuote(logPath), pid, shellQuote(uv), shellQuote(RepoInstallSpec), restartLine)
	if err := os.WriteFile(scriptPath, []byte(script), 0700); err != nil {
		return StartResult{}, err
	}
	cmd := exec.Command("sh", "-c", "nohup "+shellQuote(scriptPath)+" >/dev/null 2>&1 &")
	if err := cmd.Start(); err != nil {
		return StartResult{}, err
	}
	return StartResult{
		Started: true,
		Message: "Aggiornamento avviato. Vortelio si chiudera' e uv installera' la nuova versione.",
		LogPath: logPath,
	}, nil
}
