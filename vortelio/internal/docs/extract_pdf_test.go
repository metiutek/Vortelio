package docs_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/vortelio/vortelio/internal/docs"
	"github.com/vortelio/vortelio/internal/runtime"
)

func TestExtractPDF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.pdf")
	if _, err := runtime.CreateDocument("pdf", p, "Report", "The quick brown fox jumps over the lazy dog."); err != nil {
		t.Fatal(err)
	}
	r, err := docs.Extract(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text, "quick brown fox") || r.Pages != 1 {
		t.Errorf("got %q (%d pages)", r.Text, r.Pages)
	}
}
