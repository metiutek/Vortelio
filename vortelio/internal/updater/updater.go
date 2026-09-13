package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vortelio/vortelio/internal/version"
)

const (
	RepoInstallSpec = "git+https://github.com/metiu1/Vortelio#subdirectory=vortelio-pip"
	LatestAPIURL    = "https://api.github.com/repos/metiu1/Vortelio/releases/latest"
	// MainVersionURL is the canonical version on the default branch. Installs come
	// from main (git), which is usually ahead of the latest GitHub release, so we
	// compare against this to detect available updates.
	MainVersionURL = "https://raw.githubusercontent.com/metiu1/Vortelio/main/vortelio/internal/version/version.go"
)

var mainVersionRE = regexp.MustCompile(`Version\s*=\s*"([0-9]+(?:\.[0-9]+){1,3})"`)

type Info struct {
	Current        string `json:"current"`
	Latest         string `json:"latest"`
	Available      bool   `json:"available"`
	InstallCommand string `json:"install_command"`
	Source         string `json:"source"`
}

type StartResult struct {
	Started bool   `json:"started"`
	Message string `json:"message"`
	LogPath string `json:"log_path,omitempty"`
}

// Check results are cached: a successful lookup is reused for checkTTL, a
// failed one for checkErrTTL, so callers (GUI status poll, TUI menu) never hit
// GitHub in a tight loop or hang for the full timeout when offline.
const (
	checkTTL    = 15 * time.Minute
	checkErrTTL = 2 * time.Minute
)

var (
	checkMu   sync.Mutex
	checkInfo Info
	checkErr  error
	checkAt   time.Time
)

func Check(ctx context.Context) (Info, error) {
	checkMu.Lock()
	if !checkAt.IsZero() {
		ttl := checkTTL
		if checkErr != nil {
			ttl = checkErrTTL
		}
		if time.Since(checkAt) < ttl {
			info, err := checkInfo, checkErr
			checkMu.Unlock()
			return info, err
		}
	}
	checkMu.Unlock()

	info, err := checkUncached(ctx)

	checkMu.Lock()
	// Never let a transient failure evict a good result within its TTL.
	if err == nil || checkErr != nil || time.Since(checkAt) >= checkTTL {
		checkInfo, checkErr, checkAt = info, err, time.Now()
	}
	checkMu.Unlock()
	return info, err
}

func checkUncached(ctx context.Context) (Info, error) {
	info := Info{
		Current:        version.Version,
		InstallCommand: "uv tool install --reinstall --refresh \"" + RepoInstallSpec + "\"",
		Source:         MainVersionURL,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, MainVersionURL, nil)
	if err != nil {
		return info, err
	}
	req.Header.Set("User-Agent", "vortelio-updater/"+version.Version)
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return info, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return info, err
	}
	m := mainVersionRE.FindStringSubmatch(string(body))
	if len(m) < 2 {
		return info, errors.New("could not read version from main")
	}
	info.Latest = m[1]
	info.Available = compareVersions(info.Latest, version.Version) > 0
	return info, nil
}

func CheckWithTimeout(timeout time.Duration) (Info, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return Check(ctx)
}

// InstallForeground runs `uv tool install --reinstall --refresh <spec>` in the
// foreground, streaming uv's output to the terminal and blocking until it
// finishes. The CLI uses this so the user watches real progress and knows when
// the update is actually done — unlike the detached GUI updater, which returns
// immediately and made updates look like they "did nothing".
func InstallForeground() error {
	uv, err := exec.LookPath("uv")
	if err != nil {
		return errors.New("uv non trovato nel PATH. Installa uv con `pip install uv` e riprova")
	}
	cmd := exec.Command(uv, "tool", "install", "--reinstall", "--refresh", RepoInstallSpec)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func cleanVersion(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "V"), "v")
	re := regexp.MustCompile(`\d+(?:\.\d+){1,3}`)
	if m := re.FindString(s); m != "" {
		return m
	}
	return ""
}

func compareVersions(a, b string) int {
	ap := versionParts(a)
	bp := versionParts(b)
	max := len(ap)
	if len(bp) > max {
		max = len(bp)
	}
	for i := 0; i < max; i++ {
		var av, bv int
		if i < len(ap) {
			av = ap[i]
		}
		if i < len(bp) {
			bv = bp[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = cleanVersion(v)
	if v == "" {
		return nil
	}
	raw := strings.Split(v, ".")
	out := make([]int, 0, len(raw))
	for _, part := range raw {
		n, _ := strconv.Atoi(part)
		out = append(out, n)
	}
	return out
}
