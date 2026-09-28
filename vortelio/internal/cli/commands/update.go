package commands

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/config"
	"github.com/vortelio/vortelio/internal/updater"
)

type UpdateCommand struct{}

func NewUpdateCommand() *UpdateCommand { return &UpdateCommand{} }
func (c *UpdateCommand) Name() string  { return "update" }

func (c *UpdateCommand) Run(args []string) error {
	checkOnly := hasArg(args, "--check")
	force := hasArg(args, "--force")

	// vortelio update --auto on|off — toggle automatic updates (server side).
	for i, arg := range args {
		if !strings.EqualFold(arg, "--auto") {
			continue
		}
		cfg := config.Get()
		if i+1 < len(args) {
			switch strings.ToLower(args[i+1]) {
			case "on", "true", "1":
				cfg.AutoUpdate = true
			case "off", "false", "0":
				cfg.AutoUpdate = false
			default:
				return fmt.Errorf("uso: vortelio update --auto on|off")
			}
			if err := config.Save(); err != nil {
				return fmt.Errorf("salvataggio config fallito: %w", err)
			}
		}
		if cfg.AutoUpdate {
			fmt.Println("Aggiornamento automatico: ATTIVO (il server installa le nuove versioni quando e' inattivo).")
		} else {
			fmt.Println("Aggiornamento automatico: DISATTIVO. Attivalo con: vortelio update --auto on")
		}
		return nil
	}

	info, err := updater.CheckWithTimeout(10 * time.Second)
	if err != nil {
		if !force {
			return fmt.Errorf("controllo aggiornamenti fallito: %w", err)
		}
		fmt.Printf("Controllo aggiornamenti fallito: %s\n", err)
		fmt.Println("Procedo comunque con la reinstallazione forzata.")
	}

	if info.Current != "" {
		fmt.Printf("Versione installata: %s\n", info.Current)
	}
	if info.Latest != "" {
		fmt.Printf("Ultima versione:     %s\n", info.Latest)
	}
	if checkOnly {
		if info.Available {
			fmt.Println("Aggiornamento disponibile. Esegui: vortelio update")
		} else {
			fmt.Println("Vortelio e' gia' aggiornato.")
		}
		return nil
	}
	if !force && !info.Available {
		fmt.Println("Vortelio e' gia' aggiornato. Usa --force per reinstallare.")
		return nil
	}

	// On Windows a foreground `uv tool install` always self-destructs: this
	// process is launched by the uv-venv python.exe (in ...\uv\tools\vortelio\
	// Scripts), which stays alive holding that dir open while we run. uv then
	// can't remove the old Scripts and aborts with "Access denied", leaving a
	// half-removed, corrupt install. Hand off to a detached updater that first
	// waits for this whole process tree to exit, then reinstalls with nothing
	// locking the venv. The caller exits right after, releasing the lock.
	if runtime.GOOS == "windows" {
		res, err := updater.StartDetached(false)
		if err != nil {
			return fmt.Errorf("aggiornamento fallito: %w", err)
		}
		fmt.Println(res.Message)
		if res.LogPath != "" {
			fmt.Printf("Log: %s\n", res.LogPath)
		}
		fmt.Println("Vortelio si chiude ora. Riaprilo tra qualche secondo con: vortelio")
		return nil
	}

	// Unix: no venv lock (uv can replace open files), so stream uv's progress
	// in the foreground and block until it finishes.
	fmt.Println("Aggiornamento in corso — non chiudere il terminale…")
	if err := updater.InstallForeground(); err != nil {
		return fmt.Errorf("aggiornamento fallito: %w", err)
	}
	fmt.Println()
	fmt.Println("✅ Vortelio aggiornato. Riavvialo con: vortelio")
	return nil
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if strings.EqualFold(arg, want) {
			return true
		}
	}
	return false
}
