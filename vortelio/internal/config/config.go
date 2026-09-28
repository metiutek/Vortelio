package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Config contiene tutte le opzioni configurabili a runtime.
type Config struct {
	Port                  int      `json:"port"`                    // porta server HTTP (default 11500)
	BindAddr              string   `json:"bind_addr"`               // indirizzo bind (default "127.0.0.1"; usa "0.0.0.0" per accesso remoto)
	APIKey                string   `json:"api_key"`                 // chiave API opzionale per autenticazione (vuota = nessuna auth)
	AllowOrigins          []string `json:"allow_origins"`           // origini CORS aggiuntive ("*" = tutte)
	OllamaPort            int      `json:"ollama_port"`             // porta Ollama locale (default 11434)
	CloudTimeoutSec       int      `json:"cloud_timeout_sec"`       // timeout chiamate cloud in secondi (default 120)
	PythonBin             string   `json:"python_bin"`              // percorso Python custom (default: auto-detect)
	OpenRouterKey         string   `json:"openrouter_key"`          // master OpenRouter API key per proxy cloud premium
	StripeSecretKey       string   `json:"stripe_secret_key"`       // Stripe secret key (sk_live_... o sk_test_...)
	StripeWebhookSec      string   `json:"stripe_webhook_secret"`   // Stripe webhook signing secret (whsec_...)
	StripePricePro        string   `json:"stripe_price_pro"`        // Stripe Price ID per piano Pro (€9.99/mese)
	StripePriceBusiness   string   `json:"stripe_price_business"`   // Stripe Price ID per piano Business (€20/mese)
	StripePriceEnterprise string   `json:"stripe_price_enterprise"` // Stripe Price ID per piano Enterprise (€150/mese)
	AutoUpdate            bool     `json:"auto_update"`             // installa da solo le nuove versioni di Vortelio (server idle)
}

var (
	once     sync.Once
	instance *Config
)

// Load carica la configurazione. Chiamare una volta all'avvio; poi usare Get().
func Load() *Config {
	once.Do(func() {
		instance = defaults()
		path := filepath.Join(HomeDir(), "config.json")
		if data, err := os.ReadFile(path); err == nil {
			json.Unmarshal(data, instance) // valori file sovrascrivono default
		}
	})
	return instance
}

// Save scrive la configurazione corrente in config.json.
func Save() error {
	data, err := json.MarshalIndent(Get(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(HomeDir(), 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(HomeDir(), "config.json"), data, 0644)
}

// Get restituisce la configurazione già caricata (o i default se Load non chiamato).
func Get() *Config {
	if instance != nil {
		return instance
	}
	return Load()
}

// HomeDir restituisce la home directory di Vortelio.
// Priorità: env VORTELIO_HOME > ~/.vortelio
func HomeDir() string {
	if h := os.Getenv("VORTELIO_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".vortelio")
}

func defaults() *Config {
	return &Config{
		Port:            11500,
		BindAddr:        "127.0.0.1",
		OllamaPort:      11434,
		CloudTimeoutSec: 120,
	}
}
