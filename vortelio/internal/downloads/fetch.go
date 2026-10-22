// Package downloads moves large files onto disk reliably: every transfer goes
// to a "<dest>.part" file that survives cancellation, crashes and restarts, is
// continued with an HTTP Range request, retried with backoff when the
// connection drops or stalls, checked against the expected size and SHA-256,
// and only then renamed into place. A file at dest is therefore always whole.
package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Spec describes one file to fetch.
type Spec struct {
	URL    string
	Dest   string
	Size   int64  // expected size in bytes; 0 = unknown
	SHA256 string // expected hex digest; "" = not checked
	Header http.Header
}

// Tunables (variables so tests can shorten them).
var (
	// StallTimeout aborts an attempt when no byte arrives for this long.
	StallTimeout = 60 * time.Second
	// MaxAttempts bounds consecutive failed attempts that made no progress.
	MaxAttempts = 8
	backoffBase = 2 * time.Second
	backoffMax  = 60 * time.Second
)

// ErrCancelled is returned when the context is cancelled. The .part file is
// kept so the next Fetch continues from where this one stopped.
var ErrCancelled = errors.New("download cancelled")

// PermanentError marks a failure retrying cannot fix (404, 401, bad checksum).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func permanent(format string, a ...any) error {
	return &PermanentError{Err: fmt.Errorf(format, a...)}
}

// PartPath is where the in-progress bytes of dest are kept.
func PartPath(dest string) string { return dest + ".part" }

// Complete reports whether dest already holds the expected file. With a known
// size the size must match; without one any non-empty file counts.
func Complete(dest string, size int64) bool {
	st, err := os.Stat(dest)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return false
	}
	return size <= 0 || st.Size() == size
}

// PartialBytes is how much of dest is already on disk (whole or partial).
func PartialBytes(dest string) int64 {
	if st, err := os.Stat(dest); err == nil && !st.IsDir() {
		return st.Size()
	}
	if st, err := os.Stat(PartPath(dest)); err == nil {
		return st.Size()
	}
	return 0
}

// Fetch downloads s.URL to s.Dest. onBytes receives the number of bytes of
// this file present on disk after every write (resumed bytes included) and may
// be nil. A file already complete at Dest is not fetched again.
func Fetch(ctx context.Context, s Spec, onBytes func(have int64)) error {
	if onBytes == nil {
		onBytes = func(int64) {}
	}
	if Complete(s.Dest, s.Size) {
		onBytes(s.Size)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Dest), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	part := PartPath(s.Dest)
	failures := 0
	for {
		if ctx.Err() != nil {
			return ErrCancelled
		}
		before := fileSize(part)
		if s.Size > 0 && before > s.Size {
			os.Remove(part) // larger than the file: corrupt, start over
			before = 0
		}
		var err error
		if s.Size > 0 && before == s.Size {
			err = nil
		} else {
			err = attempt(ctx, s, part, onBytes)
		}
		if err == nil {
			if verr := finish(s, part); verr != nil {
				return verr
			}
			onBytes(fileSize(s.Dest))
			return nil
		}
		if ctx.Err() != nil {
			return ErrCancelled
		}
		var perm *PermanentError
		if errors.As(err, &perm) {
			return err
		}
		// Only failures that made no progress count towards giving up, so a
		// flaky connection that keeps moving forward is never abandoned.
		if fileSize(part) > before {
			failures = 0
		} else {
			failures++
		}
		if failures >= MaxAttempts {
			return fmt.Errorf("download failed after %d attempts: %w", failures, err)
		}
		wait := backoffBase << min(failures, 5)
		if wait > backoffMax {
			wait = backoffMax
		}
		select {
		case <-ctx.Done():
			return ErrCancelled
		case <-time.After(wait):
		}
	}
}

func fileSize(p string) int64 {
	if st, err := os.Stat(p); err == nil {
		return st.Size()
	}
	return 0
}

var client = &http.Client{
	Timeout: 0, // body reads are guarded by the stall watchdog instead
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 15 {
			return fmt.Errorf("too many redirects")
		}
		// HF signs CDN redirects itself; never forward the token off-site.
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	},
}

// attempt performs one HTTP request, appending to part from its current size.
func attempt(ctx context.Context, s Spec, part string, onBytes func(int64)) error {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()

	have := fileSize(part)
	req, err := http.NewRequestWithContext(actx, http.MethodGet, s.URL, nil)
	if err != nil {
		return permanent("bad URL: %v", err)
	}
	for k, v := range s.Header {
		req.Header[k] = v
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "vortelio")
	}
	req.Header.Set("Accept-Encoding", "identity")
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}

	// Watchdog: cancel the attempt when the connection goes silent.
	// StallTimeout is read here, not in the goroutine, which can outlive the attempt.
	stall := StallTimeout
	tick := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(stall)
		defer t.Stop()
		for {
			select {
			case <-actx.Done():
				return
			case <-tick:
				if !t.Stop() {
					<-t.C
				}
				t.Reset(stall)
			case <-t.C:
				cancel()
				return
			}
		}
	}()
	stalled := func() bool { return actx.Err() != nil && ctx.Err() == nil }

	resp, err := client.Do(req)
	if err != nil {
		if stalled() {
			return errors.New("connection stalled")
		}
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		// Server ignored Range: rewrite from zero instead of duplicating bytes.
		flags |= os.O_TRUNC
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		// Usually means the part already holds every byte.
		if s.Size > 0 && have >= s.Size {
			return nil
		}
		os.Remove(part)
		return errors.New("server rejected resume; restarting")
	case http.StatusUnauthorized, http.StatusForbidden:
		return permanent("access denied (HTTP %d) — the model may be gated: accept its terms on huggingface.co and set a token", resp.StatusCode)
	case http.StatusNotFound:
		return permanent("file not found (HTTP 404): %s", s.URL)
	default:
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("server busy (HTTP %d)", resp.StatusCode)
		}
		return permanent("server returned HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return permanent("cannot write %s: %v", part, err)
	}
	buf := make([]byte, 1<<20)
	var werr error
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr = f.Write(buf[:n]); werr != nil {
				break
			}
			have += int64(n)
			select {
			case tick <- struct{}{}:
			default:
			}
			onBytes(have)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			werr = rerr
			break
		}
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		if stalled() {
			return errors.New("connection stalled")
		}
		if isDiskFull(werr) {
			return permanent("disk full while writing %s", filepath.Base(s.Dest))
		}
		return fmt.Errorf("transfer interrupted: %w", werr)
	}
	if s.Size > 0 && have < s.Size {
		return fmt.Errorf("connection closed early (%d of %d bytes)", have, s.Size)
	}
	return nil
}

func isDiskFull(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no space") || strings.Contains(m, "not enough space") || strings.Contains(m, "disk full")
}

// finish verifies part and moves it to dest.
func finish(s Spec, part string) error {
	st, err := os.Stat(part)
	if err != nil {
		return fmt.Errorf("download vanished: %w", err)
	}
	if st.Size() == 0 {
		os.Remove(part)
		return errors.New("server sent an empty file")
	}
	if s.Size > 0 && st.Size() != s.Size {
		os.Remove(part)
		return fmt.Errorf("size mismatch: got %d bytes, expected %d", st.Size(), s.Size)
	}
	if s.SHA256 != "" {
		got, err := FileSHA256(part)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, strings.TrimPrefix(s.SHA256, "sha256:")) {
			os.Remove(part)
			return permanent("checksum mismatch for %s — the file was corrupted in transit, retry the download", filepath.Base(s.Dest))
		}
	}
	return renameRetry(part, s.Dest)
}

// FileSHA256 returns the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// renameRetry replaces dst with src; on Windows an antivirus scan or a process
// that just exited can hold dst for a moment.
func renameRetry(src, dst string) error {
	var err error
	for i := 0; i < 20; i++ {
		os.Remove(dst)
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("finalize %s: %w", filepath.Base(dst), err)
}
