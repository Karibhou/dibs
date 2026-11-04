package intake

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
)

// pdfPageObjects returns a one-page document (catalog, pages tree, page,
// font, contents) whose contents stream carries the given dictionary
// extras, so a test can swap in a filter or other stream attribute.
func pdfPageObjects(text, streamExtras string) []string {
	content := "BT /F1 12 Tf 36 100 Td (" + text + ") Tj ET\n"
	return []string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [3 0 R] /Count 1 >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>`,
		`<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>`,
		fmt.Sprintf("<< /Length %d %s>>\nstream\n%sendstream", len(content), streamExtras, content),
	}
}

// assemblePDF serialises numbered objects with a valid xref table and
// trailer, so pdf.NewReader accepts the file and any malformation under
// test is reached only while walking the page tree.
func assemblePDF(objects []string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

// TestExtractPDFRecoversParserPanic pins the recover in ExtractPDF. The
// fixtures in TestExtractPDFMalformedReturnsError are all rejected by
// pdf.NewReader before any page is read, so they never exercise the panic
// path; this document has a well-formed header, xref and trailer, and only
// the page object itself is syntactically broken, which makes the upstream
// parser panic while the page tree is walked.
func TestExtractPDFRecoversParserPanic(t *testing.T) {
	t.Parallel()
	doc := assemblePDF(pdfPageObjects("hi", ""))
	broken := bytes.Replace(doc, []byte("3 0 obj\n<< /Type /Page"), []byte("3 0 obj\n<< /Type )Page"), 1)
	if bytes.Equal(broken, doc) {
		t.Fatal("fixture did not corrupt the page object")
	}
	got, err := ExtractPDF(broken)
	if err == nil || !strings.HasPrefix(err.Error(), "malformed PDF:") {
		t.Fatalf("err = %v, want recovered parser panic reported as malformed PDF", err)
	}
	if got != "" {
		t.Fatalf("text = %q, want empty on recovered panic", got)
	}
}

// TestExtractPDFSkipsNullPages covers the page-tree walk when /Count
// overstates /Kids: the reader hands back a null page for the surplus
// index, which must be skipped rather than aborting the extraction or
// dropping the text of the real pages.
func TestExtractPDFSkipsNullPages(t *testing.T) {
	t.Parallel()
	objects := pdfPageObjects("real page", "")
	objects[1] = `<< /Type /Pages /Kids [3 0 R] /Count 3 >>`
	got, err := ExtractPDF(assemblePDF(objects))
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if !strings.Contains(got, "real page") {
		t.Fatalf("text = %q, want the real page's text despite null surplus pages", got)
	}
}

// TestExtractPDFReturnsPageTextError covers the GetPlainText error return:
// a content stream declaring a filter the decoder does not implement is an
// ordinary error from the page reader (not a panic), and ExtractPDF must
// surface it instead of returning partial or empty text as success.
func TestExtractPDFReturnsPageTextError(t *testing.T) {
	t.Parallel()
	got, err := ExtractPDF(assemblePDF(pdfPageObjects("hi", "/Filter /NoSuchDecode ")))
	if err == nil || !strings.Contains(err.Error(), "NoSuchDecode") {
		t.Fatalf("err = %v, want unknown-filter error from the page reader", err)
	}
	if strings.HasPrefix(err.Error(), "malformed PDF:") {
		t.Fatalf("err = %v, want a plain page error, not a recovered panic", err)
	}
	if got != "" {
		t.Fatalf("text = %q, want empty on page error", got)
	}
}

// TestPDFPageContentExceedsUndecodableStream pins the recover inside
// pdfPageContentExceeds: Value.Reader panics on a filter it cannot decode,
// and the budget probe must report "not exceeded" (rather than propagate
// the panic to ExtractPDF's recover) so GetPlainText surfaces the ordinary
// unknown-filter error.
func TestPDFPageContentExceedsUndecodableStream(t *testing.T) {
	t.Parallel()
	doc := assemblePDF(pdfPageObjects("hi", "/Filter /NoSuchDecode "))
	r, err := pdf.NewReader(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if pdfPageContentExceeds(r.Page(1), 1) {
		t.Fatal("pdfPageContentExceeds = true for an undecodable stream, want false so the page error is reported")
	}
}
