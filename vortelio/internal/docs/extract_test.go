package docs

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeZip(t *testing.T, p string, files map[string]string) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, body := range files {
		w, _ := z.Create(name)
		w.Write([]byte(body))
	}
	z.Close()
	f.Close()
}

func TestExtractDocx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.docx")
	writeZip(t, p, map[string]string{"word/document.xml": `<?xml version="1.0"?><w:document xmlns:w="w"><w:body>` +
		`<w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:t xml:space="preserve"> world</w:t></w:r></w:p><w:p><w:r><w:t>Second paragraph &amp; more</w:t></w:r></w:p></w:body></w:document>`})
	r, err := Extract(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "Hello world\nSecond paragraph & more" {
		t.Errorf("got %q", r.Text)
	}
}

func TestExtractXlsx(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.xlsx")
	writeZip(t, p, map[string]string{
		"xl/sharedStrings.xml":     `<sst><si><t>Name</t></si><si><t>Price</t></si><si><t>Apple</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<worksheet><sheetData><row><c t="s"><v>0</v></c><c t="s"><v>1</v></c></row><row><c t="s"><v>2</v></c><c><v>1.5</v></c></row></sheetData></worksheet>`,
	})
	r, err := Extract(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Text, "Name\tPrice") || !strings.Contains(r.Text, "Apple\t1.5") || r.Pages != 1 {
		t.Errorf("got %q (%d sheets)", r.Text, r.Pages)
	}
}

func TestExtractBrokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.pdf")
	os.WriteFile(p, []byte("%PDF-1.4 not really"), 0o644)
	if _, err := Extract(p); err == nil {
		t.Error("expected an error")
	}
}

func TestNaturalLess(t *testing.T) {
	if !naturalLess("ppt/slides/slide2.xml", "ppt/slides/slide10.xml") {
		t.Error("slide2 should come before slide10")
	}
}
