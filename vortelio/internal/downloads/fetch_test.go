package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// rangeServer serves data with Range support; when cut > 0 the first
// `drops` responses close the connection after `cut` bytes.
func rangeServer(t *testing.T, data []byte, cut int, drops int32) (*httptest.Server, *int32) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		start := 0
		if rg := r.Header.Get("Range"); rg != "" {
			s := strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-")
			start, _ = strconv.Atoi(s)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		}
		body := data[start:]
		if cut > 0 && n <= drops && len(body) > cut {
			w.Write(body[:cut])
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func fastRetries(t *testing.T) {
	ob := backoffBase
	backoffBase = time.Millisecond
	t.Cleanup(func() { backoffBase = ob })
}

func TestFetchResumesAfterDrops(t *testing.T) {
	fastRetries(t)
	data := payload(3 << 20)
	srv, calls := rangeServer(t, data, 700_000, 3)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	var last int64
	err := Fetch(context.Background(), Spec{URL: srv.URL, Dest: dest, Size: int64(len(data)), SHA256: digest(data)},
		func(h int64) { last = h })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if digest(got) != digest(data) {
		t.Fatal("content mismatch")
	}
	if last != int64(len(data)) {
		t.Fatalf("progress ended at %d", last)
	}
	if atomic.LoadInt32(calls) < 4 {
		t.Fatalf("expected resumed requests, got %d", *calls)
	}
	if _, err := os.Stat(PartPath(dest)); !os.IsNotExist(err) {
		t.Fatal(".part left behind")
	}
}

func TestFetchContinuesExistingPart(t *testing.T) {
	data := payload(1 << 20)
	srv, _ := rangeServer(t, data, 0, 0)
	dest := filepath.Join(t.TempDir(), "m.bin")
	os.WriteFile(PartPath(dest), data[:400_000], 0o644)
	var first int64 = -1
	err := Fetch(context.Background(), Spec{URL: srv.URL, Dest: dest, Size: int64(len(data))}, func(h int64) {
		if first < 0 {
			first = h
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if first <= 400_000 {
		t.Fatalf("did not resume: first progress %d", first)
	}
}

func TestFetchChecksumMismatchIsPermanent(t *testing.T) {
	data := payload(200_000)
	srv, calls := rangeServer(t, data, 0, 0)
	dest := filepath.Join(t.TempDir(), "m.bin")
	err := Fetch(context.Background(), Spec{URL: srv.URL, Dest: dest, Size: int64(len(data)), SHA256: strings.Repeat("0", 64)}, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if *calls != 1 {
		t.Fatalf("checksum failure must not retry, calls=%d", *calls)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("corrupt file published")
	}
}

func TestFetch404Permanent(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	err := Fetch(context.Background(), Spec{URL: srv.URL, Dest: filepath.Join(t.TempDir(), "x")}, nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v", err)
	}
}

func TestFetchStallRetries(t *testing.T) {
	fastRetries(t)
	oldStall := StallTimeout
	StallTimeout = 300 * time.Millisecond
	defer func() { StallTimeout = oldStall }()
	data := payload(500_000)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:1000])
			w.(http.Flusher).Flush()
			<-r.Context().Done() // hang until the client gives up
			return
		}
		start := 0
		if rg := r.Header.Get("Range"); rg != "" {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write(data[start:])
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "s.bin")
	if err := Fetch(context.Background(), Spec{URL: srv.URL, Dest: dest, Size: int64(len(data)), SHA256: digest(data)}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFetchCancelKeepsPart(t *testing.T) {
	data := payload(4 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data[:1<<20])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "c.bin")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for PartialBytes(dest) < 1<<20 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	err := Fetch(ctx, Spec{URL: srv.URL, Dest: dest, Size: int64(len(data))}, nil)
	if err != ErrCancelled {
		t.Fatalf("got %v", err)
	}
	if PartialBytes(dest) != 1<<20 {
		t.Fatalf("part has %d bytes", PartialBytes(dest))
	}
}

func TestManagerJoinCancelRetry(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "d.json"))
	release := make(chan struct{})
	var runs int32
	task := func(ctx context.Context, p *Progress) error {
		atomic.AddInt32(&runs, 1)
		p.SetTotal(10)
		select {
		case <-ctx.Done():
			return ErrCancelled
		case <-release:
		}
		p.SetDone(10)
		return nil
	}
	j, joined := m.Start("model", "llm/x:1", "x", nil, task)
	if joined {
		t.Fatal("first start joined")
	}
	j2, joined := m.Start("model", "llm/x:1", "x", nil, task)
	if !joined || j2.ID != j.ID {
		t.Fatal("second start must join")
	}
	m.Cancel(j.ID)
	fin, _ := m.Wait(context.Background(), j.ID)
	if fin.State != Cancelled {
		t.Fatalf("state %s", fin.State)
	}
	close(release)
	if err := m.Retry(j.ID); err != nil {
		t.Fatal(err)
	}
	fin, _ = m.Wait(context.Background(), j.ID)
	if fin.State != Done || fin.Done != 10 {
		t.Fatalf("after retry: %+v", fin)
	}
}

func TestManagerInterruptedResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	m := NewManager(path)
	block := make(chan struct{})
	j, _ := m.Start("model", "llm/y:1", "y", map[string]string{"a": "b"}, func(ctx context.Context, p *Progress) error {
		<-block
		return nil
	})
	time.Sleep(50 * time.Millisecond)
	m.scheduleSave(true)

	m2 := NewManager(path) // simulated restart
	got, ok := m2.Get(j.ID)
	if !ok || got.State != Interrupted {
		t.Fatalf("want interrupted, got %+v", got)
	}
	resumed := make(chan string, 1)
	m2.RegisterKind("model", func(key string, meta map[string]string) (Task, error) {
		return func(ctx context.Context, p *Progress) error { resumed <- key + meta["a"]; return nil }, nil
	})
	select {
	case k := <-resumed:
		if k != "llm/y:1b" {
			t.Fatal(k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not resumed")
	}
	close(block)
	// Both managers save d.json when their job ends: wait for it, or the
	// write races the TempDir cleanup ("directory is not empty" on Windows).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.Wait(ctx, j.ID)
	m2.Wait(ctx, j.ID)
}
