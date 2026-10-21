// Package docs extracts the text of documents people attach to a chat (PDF,
// Word, PowerPoint, Excel, OpenDocument, EPUB, RTF, HTML), so any model can
// read them — not only an agent with a file tool.
package docs

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ledongthuc/pdf"
)

// MaxChars caps what Extract returns (about 70k tokens).
const MaxChars = 280_000

// Result is the text of one document.
type Result struct {
	Text      string `json:"text"`
	Kind      string `json:"kind"`  // pdf, docx, …
	Pages     int    `json:"pages"` // PDF pages, slides, sheets (0 when not meaningful)
	Truncated bool   `json:"truncated"`
}

// Supported reports whether Extract knows the file's format.
func Supported(name string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "pdf", "docx", "pptx", "xlsx", "odt", "odp", "ods", "epub", "rtf", "html", "htm":
		return true
	}
	return false
}

// Extract reads the text of the document at p.
func Extract(p string) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil { // malformed files must not take the server down
			err = fmt.Errorf("cannot read this document: %v", r)
		}
	}()
	kind := strings.ToLower(strings.TrimPrefix(filepath.Ext(p), "."))
	res.Kind = kind
	switch kind {
	case "pdf":
		res.Text, res.Pages, err = extractPDF(p)
	case "docx":
		res.Text, err = zipXMLText(p, func(n string) bool { return n == "word/document.xml" }, "p")
	case "pptx":
		res.Text, res.Pages, err = pptxText(p)
	case "xlsx":
		res.Text, res.Pages, err = xlsxText(p)
	case "odt", "odp", "ods":
		res.Text, err = zipXMLText(p, func(n string) bool { return n == "content.xml" }, "p", "h", "table-row")
	case "epub":
		res.Text, err = zipXMLText(p, func(n string) bool {
			e := strings.ToLower(path.Ext(n))
			return e == ".xhtml" || e == ".html" || e == ".htm"
		}, "p", "h1", "h2", "h3", "li", "div")
	case "rtf":
		res.Text, err = rtfText(p)
	case "html", "htm":
		var b []byte
		if b, err = os.ReadFile(p); err == nil {
			res.Text = htmlText(string(b))
		}
	default:
		return res, fmt.Errorf("unsupported document type %q", kind)
	}
	if err != nil {
		return res, err
	}
	res.Text = tidy(res.Text)
	if r := []rune(res.Text); len(r) > MaxChars {
		res.Text, res.Truncated = string(r[:MaxChars]), true
	}
	if strings.TrimSpace(res.Text) == "" {
		return res, fmt.Errorf("no text found (a scanned document needs OCR)")
	}
	return res, nil
}

func extractPDF(p string) (string, int, error) {
	f, r, err := pdf.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	n := r.NumPage()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		pg := r.Page(i)
		if pg.V.IsNull() {
			continue
		}
		rows, err := pg.GetTextByRow()
		if err != nil {
			continue
		}
		for _, row := range rows {
			var line strings.Builder
			prevX := -1.0
			for _, w := range row.Content {
				if prevX >= 0 && w.X-prevX > w.FontSize*0.25 && !strings.HasSuffix(line.String(), " ") {
					line.WriteByte(' ')
				}
				line.WriteString(w.S)
				prevX = w.X + w.W
			}
			b.WriteString(strings.TrimSpace(line.String()))
			b.WriteByte('\n')
		}
		if b.Len() > MaxChars*4 {
			break
		}
		b.WriteString("\n")
	}
	return b.String(), n, nil
}

// zipXMLText concatenates the text of the XML parts accepted by want,
// starting a new line at each of the given element names.
func zipXMLText(p string, want func(string) bool, breaks ...string) (string, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return "", err
	}
	defer z.Close()
	files := append([]*zip.File(nil), z.File...)
	sort.SliceStable(files, func(i, j int) bool { return naturalLess(files[i].Name, files[j].Name) })
	var b strings.Builder
	found := false
	for _, f := range files {
		if !want(f.Name) {
			continue
		}
		found = true
		if err := xmlText(f, &b, breaks); err != nil {
			return "", err
		}
		b.WriteString("\n")
	}
	if !found {
		return "", fmt.Errorf("not a valid document")
	}
	return b.String(), nil
}

func xmlText(f *zip.File, b *strings.Builder, breaks []string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	d := xml.NewDecoder(io.LimitReader(rc, 64<<20))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	skip := 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return nil // keep what was read
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "script", "style", "head":
				skip++
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "script", "style", "head":
				if skip > 0 {
					skip--
				}
			}
			for _, br := range breaks {
				if t.Name.Local == br {
					b.WriteByte('\n')
				}
			}
		case xml.CharData:
			if skip == 0 {
				b.Write(t)
			}
		}
	}
}

var slideRe = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

func pptxText(p string) (string, int, error) {
	n := 0
	text, err := zipXMLText(p, func(name string) bool {
		if slideRe.MatchString(name) {
			n++
			return true
		}
		return false
	}, "p")
	return text, n, err
}

// xlsxText lists each sheet's rows as tab-separated values.
func xlsxText(p string) (string, int, error) {
	z, err := zip.OpenReader(p)
	if err != nil {
		return "", 0, err
	}
	defer z.Close()
	var shared []string
	for _, f := range z.File {
		if f.Name == "xl/sharedStrings.xml" {
			var b strings.Builder
			if err := xmlText(f, &b, []string{"si"}); err == nil {
				shared = strings.Split(b.String(), "\n")
			}
		}
	}
	var sheets []*zip.File
	for _, f := range z.File {
		if strings.HasPrefix(f.Name, "xl/worksheets/sheet") && strings.HasSuffix(f.Name, ".xml") {
			sheets = append(sheets, f)
		}
	}
	sort.Slice(sheets, func(i, j int) bool { return naturalLess(sheets[i].Name, sheets[j].Name) })
	var out strings.Builder
	for i, f := range sheets {
		fmt.Fprintf(&out, "## Sheet %d\n", i+1)
		rc, err := f.Open()
		if err != nil {
			continue
		}
		var ws struct {
			Rows []struct {
				Cells []struct {
					T string `xml:"t,attr"`
					V string `xml:"v"`
					I string `xml:"is>t"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		xml.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&ws)
		rc.Close()
		for _, r := range ws.Rows {
			vals := make([]string, 0, len(r.Cells))
			for _, c := range r.Cells {
				v := c.V
				switch c.T {
				case "s":
					if k, err := strconv.Atoi(c.V); err == nil && k < len(shared) {
						v = shared[k]
					}
				case "inlineStr":
					v = c.I
				}
				vals = append(vals, v)
			}
			out.WriteString(strings.Join(vals, "\t"))
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	}
	return out.String(), len(sheets), nil
}

var (
	rtfCtrl  = regexp.MustCompile(`\\[a-zA-Z]+-?\d* ?`)
	rtfHex   = regexp.MustCompile(`\\'([0-9a-fA-F]{2})`)
	rtfGroup = regexp.MustCompile(`\{\\\*[^{}]*\}`)
	tagRe    = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)
	blockRe  = regexp.MustCompile(`(?i)</?(p|div|br|li|h[1-6]|tr|section|article)[^>]*>`)
	spacesRe = regexp.MustCompile(`[ \x{00a0}]+`)
	blankRe  = regexp.MustCompile(`\n{3,}`)
)

func rtfText(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := string(b)
	s = rtfGroup.ReplaceAllString(s, "")
	s = strings.NewReplacer(`\par`, "\n", `\line`, "\n", `\tab`, "\t").Replace(s)
	s = rtfHex.ReplaceAllStringFunc(s, func(m string) string {
		v, _ := strconv.ParseUint(m[2:], 16, 8)
		return string(rune(v)) // Windows-1252 ≈ Latin-1 for text
	})
	s = rtfCtrl.ReplaceAllString(s, "")
	return strings.NewReplacer("{", "", "}", "").Replace(s), nil
}

func htmlText(s string) string {
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(spacesRe.ReplaceAllString(l, " "))
	}
	return strings.TrimSpace(blankRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// naturalLess orders "slide2" before "slide10".
func naturalLess(a, b string) bool {
	ma, mb := numRe.FindStringSubmatch(a), numRe.FindStringSubmatch(b)
	if ma != nil && mb != nil && ma[1] == mb[1] && ma[3] == mb[3] {
		x, _ := strconv.Atoi(ma[2])
		y, _ := strconv.Atoi(mb[2])
		return x < y
	}
	return a < b
}

var numRe = regexp.MustCompile(`^(.*?)(\d+)(\D*)$`)
