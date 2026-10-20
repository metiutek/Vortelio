package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vortelio/vortelio/internal/config"
)

// ─────────────────────────────────────────────────────────────────────────────
// Audit log + Prometheus metrics
// ─────────────────────────────────────────────────────────────────────────────

type AuditEntry struct {
	Timestamp string `json:"timestamp"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	DurMS     int64  `json:"duration_ms"`
	RemoteIP  string `json:"remote_ip,omitempty"`
}

var (
	auditMu      sync.Mutex
	auditRingBuf = make([]AuditEntry, 0, 512)
	auditRingCap = 512
	auditFileMu  sync.Mutex

	// Prometheus counters
	mReqTotal     sync.Map // map[pathStatusKey]*int64
	mReqDurSumMS  int64
	mReqInFlight  int64
	mGenTokensIn  int64
	mGenTokensOut int64
)

func auditFilePath() string {
	return filepath.Join(config.HomeDir(), "audit.log")
}

func appendAudit(e AuditEntry) {
	auditMu.Lock()
	if len(auditRingBuf) >= auditRingCap {
		auditRingBuf = auditRingBuf[1:]
	}
	auditRingBuf = append(auditRingBuf, e)
	auditMu.Unlock()

	// async file write — non-blocking
	go func() {
		auditFileMu.Lock()
		defer auditFileMu.Unlock()
		f, err := os.OpenFile(auditFilePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		line, _ := json.Marshal(e)
		f.Write(line)
		f.Write([]byte("\n"))
	}()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.status = c
	s.ResponseWriter.WriteHeader(c)
}

// Flush keeps streaming responses (chat tokens, SSE progress) flowing: without
// it every `w.(http.Flusher)` check behind this wrapper fails and output is
// held back in 4 KB chunks.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// withObservability wraps a handler to record audit entries + prometheus metrics.
func withObservability(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&mReqInFlight, 1)
		defer atomic.AddInt64(&mReqInFlight, -1)

		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: 200}
		h(sr, r)
		dur := time.Since(start)

		status := sr.status
		if status == 0 {
			status = 200
		}

		// Prometheus counter
		key := r.URL.Path + "|" + strconv.Itoa(status)
		v, _ := mReqTotal.LoadOrStore(key, new(int64))
		atomic.AddInt64(v.(*int64), 1)
		atomic.AddInt64(&mReqDurSumMS, dur.Milliseconds())

		// Audit (skip /metrics + /api/status to avoid noise)
		if r.URL.Path == "/metrics" || r.URL.Path == "/api/status" {
			return
		}
		ip := r.RemoteAddr
		if i := strings.LastIndex(ip, ":"); i > 0 {
			ip = ip[:i]
		}
		appendAudit(AuditEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    status,
			DurMS:     dur.Milliseconds(),
			RemoteIP:  ip,
		})
	}
}

// /api/audit — GET recent audit entries
func handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, 405, "GET only")
		return
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	auditMu.Lock()
	n := len(auditRingBuf)
	if limit > n {
		limit = n
	}
	out := make([]AuditEntry, limit)
	copy(out, auditRingBuf[n-limit:])
	auditMu.Unlock()
	respond(w, 200, map[string]interface{}{
		"count":   len(out),
		"entries": out,
	})
}

// /metrics — Prometheus text exposition format
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	fmt.Fprintln(w, "# HELP vortelio_requests_total Total HTTP requests by path and status.")
	fmt.Fprintln(w, "# TYPE vortelio_requests_total counter")
	mReqTotal.Range(func(k, v interface{}) bool {
		parts := strings.SplitN(k.(string), "|", 2)
		fmt.Fprintf(w, "vortelio_requests_total{path=%q,status=%q} %d\n", parts[0], parts[1], atomic.LoadInt64(v.(*int64)))
		return true
	})

	fmt.Fprintln(w, "# HELP vortelio_request_duration_ms_sum Total request duration in ms.")
	fmt.Fprintln(w, "# TYPE vortelio_request_duration_ms_sum counter")
	fmt.Fprintf(w, "vortelio_request_duration_ms_sum %d\n", atomic.LoadInt64(&mReqDurSumMS))

	fmt.Fprintln(w, "# HELP vortelio_requests_in_flight In-flight requests.")
	fmt.Fprintln(w, "# TYPE vortelio_requests_in_flight gauge")
	fmt.Fprintf(w, "vortelio_requests_in_flight %d\n", atomic.LoadInt64(&mReqInFlight))

	fmt.Fprintln(w, "# HELP vortelio_generation_tokens_total Total generation tokens.")
	fmt.Fprintln(w, "# TYPE vortelio_generation_tokens_total counter")
	fmt.Fprintf(w, "vortelio_generation_tokens_total{direction=\"in\"} %d\n", atomic.LoadInt64(&mGenTokensIn))
	fmt.Fprintf(w, "vortelio_generation_tokens_total{direction=\"out\"} %d\n", atomic.LoadInt64(&mGenTokensOut))

	// Loaded models count from existing /api/ps logic — count via runtime if exposed
	// (skipped here — no runtime API to count loaded models without import cycle risk)
}

// MetricsRecordTokens — called from generate handlers to count tokens.
func MetricsRecordTokens(in, out int64) {
	atomic.AddInt64(&mGenTokensIn, in)
	atomic.AddInt64(&mGenTokensOut, out)
}
