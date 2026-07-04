package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/vortelio/vortelio/internal/updater"
)

type UpdateCommand struct{}

func NewUpdateCommand() *UpdateCommand { return &UpdateCommand{} }
func (c *UpdateCommand) Name() string  { return "update" }

func (c *UpdateCommand) Run(args []string) error {
	checkOnly := hasArg(args, "--check")
	force := hasArg(args, "--force")

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

	// Run the install in the foreground so the user sees uv's progress and
	// knows exactly when it finishes. (The detached updater is only for the
	// GUI, where the running server has to shut down and restart itself.)
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
