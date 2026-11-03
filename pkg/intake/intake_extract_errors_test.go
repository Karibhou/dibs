package intake

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestExtractDOCXMalformedXML covers the decode-error arm that still has
// budget left: a truncated document.xml is a parse error, not partial text.
func TestExtractDOCXMalformedXML(t *testing.T) {
	t.Parallel()
	data := zipWithEntries(t, map[string]string{
		"word/document.xml": `<w:document><w:body><w:p><w:r><w:t>unclosed`,
	})
	got, err := ExtractDOCX(data)
	if err == nil {
		t.Fatalf("ExtractDOCX accepted truncated XML, returned %q", got)
	}
	if got != "" {
		t.Fatalf("ExtractDOCX returned text %q alongside error %v", got, err)
	}
}

// TestExtractDOCXBudgetCutInsideMarkup pins the budget arm when the cut
// lands inside a token rather than between text nodes: the decoder reports
// an unexpected EOF, and because the byte budget is exhausted the text
// gathered so far is returned instead of that error.
func TestExtractDOCXBudgetCutInsideMarkup(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, `<w:document><w:body><w:p><w:r><w:t>kept</w:t></w:r></w:p><!--`)
	// A single comment token larger than the budget keeps the collected rune
	// count tiny while guaranteeing the cut happens mid-token.
	_, _ = io.WriteString(w, strings.Repeat("x", maxDOCXXMLBytes))
	_, _ = io.WriteString(w, `--></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractDOCX(buf.Bytes())
	if err != nil {
		t.Fatalf("ExtractDOCX returned decode error on exhausted budget: %v", err)
	}
	if got != "kept\n" {
		t.Fatalf("ExtractDOCX = %q, want %q", got, "kept\n")
	}
}

// TestExtractDOCXUnsupportedCompression covers the entry-open error arm: a
// document.xml stored with a compression method the reader cannot decode.
func TestExtractDOCXUnsupportedCompression(t *testing.T) {
	t.Parallel()
	const bogusMethod uint16 = 99
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(bogusMethod, func(w io.Writer) (io.WriteCloser, error) {
		return nopWriteCloser{w}, nil
	})
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "word/document.xml", Method: bogusMethod})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, `<w:document/>`); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractDOCX(buf.Bytes())
	if err == nil {
		t.Fatalf("ExtractDOCX opened an entry with unsupported compression, returned %q", got)
	}
	if !strings.Contains(err.Error(), "unsupported compression") {
		t.Fatalf("error = %v, want unsupported compression algorithm", err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// TestStripRTFEscapesAndParams covers the control-symbol and control-word
// arms the basic fixture never reaches: escaped braces and backslashes,
// multi-digit and negative numeric parameters, a trailing backslash, and the
// \~ \- \_ and escaped-newline control symbols.
func TestStripRTFEscapesAndParams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"escaped backslash", `a\\b`, `a\b`},
		{"escaped braces", `\{x\}`, `{x}`},
		{"multi-digit param", `\fs24 big`, " big"},
		{"negative param", `\li-120 indent`, " indent"},
		{"param without delimiter", `\fs24big`, " big"},
		{"trailing backslash", `tail\`, "tail"},
		{"group braces dropped", `{\rtf1 {\b bold}}`, "  bold"},
		{"optional hyphen is invisible", `hy\-phen`, "hyphen"},
		{"optional hyphen keeps following digits", `page\-12 end`, "page12 end"},
		{"non-breaking space", `10\~kg`, "10\u00a0kg"},
		{"non-breaking hyphen", `e\_mail`, "e\u2011mail"},
		{"control symbol keeps following space", `a\~ b`, "a\u00a0 b"},
		{"escaped newline is a paragraph", "a\\\nb", "a\nb"},
		{"escaped CRLF is one paragraph", "a\\\r\nb", "a\nb"},
		{"other control symbols dropped", `a\:b\|c\*d`, "abcd"},
		{"truncated hex escape", `x\'e`, "xe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripRTF(tc.in); got != tc.want {
				t.Fatalf("StripRTF(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
