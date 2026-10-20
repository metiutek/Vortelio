package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// llamaCaps records which flags the installed llama-server understands, read
// from its --help, so Vortelio only passes options the binary accepts (an
// unknown flag makes llama-server exit before loading anything).
type llamaCaps struct {
	Fit            bool // --fit on|off: size GPU layers to the free VRAM
	FitCtx         bool // --fit-ctx N
	FitTarget      bool // --fit-target MiB: free memory --fit leaves per device
	LogVerbosity   bool // --log-verbosity N (4 = load details: layers on GPU)
	FlashAttnValue bool // --flash-attn takes on|off|auto (older builds: bare flag)
	Jinja          bool // --jinja (native chat templates + tool calls)
	Embeddings     bool // --embeddings
	Pooling        bool // --pooling
	ReasoningFmt   bool // --reasoning-format
	NoMmprojAuto   bool // --no-mmproj-auto
}

var (
	llamaCapsMu    sync.Mutex
	llamaCapsCache = map[string]llamaCaps{}
)

// probeLlamaCaps returns the capabilities of bin, cached per path+mtime.
func probeLlamaCaps(bin string) llamaCaps {
	key := bin
	if st, err := os.Stat(bin); err == nil {
		key += "|" + st.ModTime().String()
	}
	llamaCapsMu.Lock()
	if c, ok := llamaCapsCache[key]; ok {
		llamaCapsMu.Unlock()
		return c
	}
	llamaCapsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--help")
	cmd.Dir = filepath.Dir(bin)
	HideWindow(cmd)
	out, _ := cmd.CombinedOutput()
	c := parseLlamaCaps(string(out))

	llamaCapsMu.Lock()
	llamaCapsCache[key] = c
	llamaCapsMu.Unlock()
	return c
}

func parseLlamaCaps(help string) llamaCaps {
	has := func(flag string) bool {
		return regexp.MustCompile(`(^|[\s,])` + regexp.QuoteMeta(flag) + `([\s,=]|$)`).MatchString(help)
	}
	c := llamaCaps{
		Fit:          has("--fit"),
		FitCtx:       has("--fit-ctx"),
		FitTarget:    has("--fit-target"),
		LogVerbosity: has("--log-verbosity"),
		Jinja:        has("--jinja"),
		Embeddings:   has("--embeddings") || has("--embedding"),
		Pooling:      has("--pooling"),
		ReasoningFmt: has("--reasoning-format"),
		NoMmprojAuto: has("--no-mmproj-auto"),
	}
	for _, line := range strings.Split(help, "\n") {
		if strings.Contains(line, "--flash-attn") && strings.Contains(line, "on|off") {
			c.FlashAttnValue = true
		}
	}
	return c
}
