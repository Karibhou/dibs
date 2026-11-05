package intake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
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

// pdfContentArrayPageObjects returns a one-page document whose /Contents
// is an array of two separate streams, the form the reader concatenates
// before interpreting, so a page's text can be split across objects.
func pdfContentArrayPageObjects(first, second string) []string {
	return []string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [3 0 R] /Count 1 >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Resources << /Font << /F1 4 0 R >> >> /Contents [5 0 R 6 0 R] >>`,
		`<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>`,
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(first), first),
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(second), second),
	}
}

// TestPDFPageContentExceedsSumsContentArray pins the /Contents array branch
// of pdfStreamExceeds. The reader interprets an array of content streams as
// one concatenated stream, so the budget must apply to their SUM: a bomb
// split into streams that are each under maxPDFPageContentBytes would
// otherwise inflate in full. The probe must also stop at the first stream
// that alone exhausts the budget, and must not trip when the sum fits.
func TestPDFPageContentExceedsSumsContentArray(t *testing.T) {
	t.Parallel()
	first := "BT /F1 12 Tf 36 100 Td (first half) Tj ET\n"
	second := "BT /F1 12 Tf 36 80 Td (second half) Tj ET\n"
	doc := assemblePDF(pdfContentArrayPageObjects(first, second))
	r, err := pdf.NewReader(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	page := r.Page(1)
	if got := page.V.Key("Contents").Kind(); got != pdf.Array {
		t.Fatalf("fixture /Contents kind = %v, want Array", got)
	}
	sum := int64(len(first) + len(second))
	for _, tc := range []struct {
		name   string
		budget int64
		want   bool
	}{
		{"sum fits exactly", sum, false},
		{"sum exceeds by one byte", sum - 1, true},
		{"first stream alone exceeds", int64(len(first)) - 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pdfPageContentExceeds(page, tc.budget); got != tc.want {
				t.Fatalf("pdfPageContentExceeds(budget=%d) = %v, want %v", tc.budget, got, tc.want)
			}
		})
	}

	// A within-budget array page must still extract the text of every
	// stream, so the guard cannot be satisfied by measuring only the first.
	got, err := ExtractPDF(doc)
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if !strings.Contains(got, "first half") || !strings.Contains(got, "second half") {
		t.Fatalf("text = %q, want text from both content streams", got)
	}
}

func TestPDFDeclaredObjectsExceeds(t *testing.T) {
	t.Parallel()
	valid := assemblePDF(pdfPageObjects("hi", ""))
	huge := bytes.Replace(valid, []byte("xref\n0 6\n"), []byte("xref\n1000000000 5\n"), 1)
	if bytes.Equal(huge, valid) {
		t.Fatal("fixture did not rewrite the subsection header")
	}
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"valid document", valid, false},
		{"huge subsection start", huge, true},
		{"huge subsection count", []byte("xref\n0 1000000000\ntrailer\n"), true},
		{"subsection count overflows int64", []byte("xref\n0 99999999999999999999999999\ntrailer\n"), true},
		{"subsection start plus count reaches limit", []byte("xref\n600000 600000\ntrailer\n"), true},
		{"second xref section is huge", []byte("xref\n0 1\ntrailer\nxref\n1000000000 1\ntrailer\n"), true},
		{"subsection without trailer", []byte("xref\n0 5\n0000000000 65535 f \n"), false},
		{"huge subsection without trailer", []byte("xref\n1000000000 5\n"), true},
		{"xref word in text", []byte("(see the xref table) 7"), false},
		{"xref keyword at end", []byte("startxref"), false},
		{"index start huge", []byte("/Type /XRef /Index [1000000000 2] /W [1 2 1]"), true},
		{"index second pair huge", []byte("/Index [0 2 1000000000 2]"), true},
		{"index small", []byte("/Index [0 2 10 3]"), false},
		{"index odd trailing value ignored", []byte("/Index [0 2 99999999]"), false},
		{"size huge no index", []byte("/Type /XRef /Size 2000000000 /W [1 2 1]"), true},
		{"size small", []byte("/Type /XRef /Size 10"), false},
		{"size overflows int64", []byte("/Size 1234567890123456789012345"), true},
		// The reader tokenizes rather than reading lines: any of its
		// whitespace bytes separate tokens, % comments are skipped, names
		// may use #xx escapes and integers may carry a sign.
		{"subsection header split across lines", []byte("xref\n1000000000\n5\n0000000000 65535 f \ntrailer\n"), true},
		{"subsection header with CR line endings", []byte("xref\r1000000000 5\r0000000000 65535 f \rtrailer\r"), true},
		{"subsection header with NUL separators", []byte("xref\x001000000000\x005\x00trailer"), true},
		{"subsection header with form feed", []byte("xref\f1000000000\f5\ftrailer"), true},
		{"subsection header after comment", []byte("xref %c\n1000000000 5\ntrailer\n"), true},
		{"subsection header with comment between tokens", []byte("xref\n1000000000 %c\n5\ntrailer\n"), true},
		{"subsection start signed", []byte("xref\n+1000000000 5\ntrailer\n"), true},
		{"huge second subsection", []byte("xref\n0 1\n0000000000 65535 f \n1000000000 1\n0000000000 00000 n \ntrailer\n"), true},
		{"huge subsection without entries", []byte("xref\n0 1\n1000000000 1\ntrailer\n"), false},
		{"entry offsets above limit are not headers", []byte("xref\n0 2\n0000000000 65535 f \n0020000000 00000 n \ntrailer\n"), false},
		{"entry offsets above limit, one token per line", []byte("xref\n0\n1\n2000000\n0\nn\ntrailer\n"), false},
		{"xref keyword ended by comment delimiter", []byte("xref%c\n1000000000 5"), true},
		{"xref keyword followed by name is rejected by reader", []byte("xref/1000000000 5"), false},
		{"xref keyword inside startxref", []byte("startxref\n2000000\n%%EOF\n"), false},
		{"size with NUL separator", []byte("/Size\x002000000000"), true},
		{"size after comment", []byte("/Size %c\n2000000000"), true},
		{"size with escaped name", []byte("/Siz#65 2000000000"), true},
		{"size signed", []byte("/Size +2000000000"), true},
		{"size non-integer ignored", []byte("/Size /Foo"), false},
		{"size inside dictionary delimiters", []byte("<</Size 2000000000>>"), true},
		{"size with invalid escape is another name", []byte("/Size#zz 2000000000"), false},
		{"size with escape at end of name", []byte("/Size# 2000000000"), false},
		{"index with comment inside array", []byte("/Index [0 2 %c\n 1000000000 2]"), true},
		{"index with NUL separators", []byte("/Index\x00[0\x002\x001000000000\x002]"), true},
		{"index with escaped name", []byte("/Ind#65x [1000000000 2]"), true},
		{"index pair after non-integer ignored", []byte("/Index [0 2 /Foo 1 1000000000 2]"), false},
		{"index not an array ignored", []byte("/Index 1000000000 2"), false},
		{"w entry huge", []byte("<< /Type /XRef /W [1 2000000000000 1] >>"), true},
		{"w small", []byte("<< /Type /XRef /W [1 2 1] >>"), false},
		{"w sum exceeds", []byte("<< /Type /XRef /W [40 40 40] >>"), true},
		{"w overflow digits", []byte("<< /Type /XRef /W [1 99999999999999999999 1] >>"), true},
		{"w negative", []byte("<< /Type /XRef /W [1 -2 1] >>"), true},
		{"w not an array ignored", []byte("<< /Type /XRef /W 2000000000000 >>"), false},
		{"w before type", []byte("<< /W [1 2000000000000 1] /Type /XRef >>"), true},
		{"w after string value", []byte("<< /Type /XRef /ID (>>) /W [1 2000000000000 1] >>"), true},
		{"w after hex string value", []byte("<< /Type /XRef /ID <3e3e> /W [1 2000000000000 1] >>"), true},
		{"w after nested dictionary value", []byte("<< /Type /XRef /DecodeParms << /Columns 5 >> /W [1 2000000000000 1] >>"), true},
		{"w after nested array value", []byte("<< /Type /XRef /Index [0 [1]] /W [1 2000000000000 1] >>"), true},
		// CIDFont dictionaries use /W for glyph widths, which the reader never
		// sizes anything from.
		{"cidfont width ranges", []byte("<< /Type /Font /Subtype /CIDFontType2 /W [40 40 40] >>"), false},
		{"cidfont first cid above limit", []byte("<< /Type /Font /Subtype /CIDFontType0 /W [100 [500 600 700]] >>"), false},
		{"cidfont inside xref-typed parent", []byte("<< /Type /XRef /Font << /Type /Font /W [1 2000000000000 1] >> >>"), false},
		{"xref type in nested dictionary", []byte("<< /Foo << /Type /XRef >> /W [1 2000000000000 1] >>"), false},
		{"w without dictionary", []byte("/Type /XRef /W [1 2000000000000 1]"), false},
		{"w after non-name key ignored", []byte("<< /Type /XRef 5 /W [1 2000000000000 1] >>"), false},
		{"w in string ignored", []byte("<< /Type /XRef /ID (/W [1 2000000000000 1]) >>"), false},
		{"unterminated escape at eof", []byte("<< /Type /XRef /ID (\\"), false},
		{"bare unterminated escape at eof", []byte("(\\"), false},
		{"overlapping candidate sections exhaust budget", pdfOverlappingXrefs(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pdfDeclaredObjectsExceeds(tc.data, maxPDFObjects); got != tc.want {
				t.Fatalf("pdfDeclaredObjectsExceeds = %v, want %v", got, tc.want)
			}
		})
	}
}

// pdfOverlappingXrefs builds input whose every xref keyword, read as the
// reader would from that offset, parses the same long run of valid entries,
// so vetting it in full would take time quadratic in its size.
func pdfOverlappingXrefs() []byte {
	var b bytes.Buffer
	b.WriteString("xref 0 4000\n")
	for range 2000 {
		b.WriteString("%xref 0 4000\n")
	}
	for range 4000 {
		b.WriteString("0000000000 65535 f \n")
	}
	b.WriteString("trailer\n")
	return b.Bytes()
}

func TestExtractPDFValidStillExtracts(t *testing.T) {
	t.Parallel()
	got, err := ExtractPDF(assemblePDF(pdfPageObjects("hello", "")))
	if err != nil || !strings.Contains(got, "hello") {
		t.Fatalf("ExtractPDF = %q, %v; want text containing hello", got, err)
	}
}

func TestExtractPDFRejectsHugeDeclaredObjectCount(t *testing.T) {
	t.Parallel()
	doc := bytes.Replace(assemblePDF(pdfPageObjects("hi", "")), []byte("xref\n0 6\n"), []byte("xref\n1000000000 5\n"), 1)
	_, err := ExtractPDF(doc)
	if err == nil || !strings.Contains(err.Error(), "declares more than") {
		t.Fatalf("err = %v, want declares more than", err)
	}
	_, err = ProcessWithContext(context.Background(), "idea.pdf", "application/pdf", doc)
	var ie *Error
	if !errors.As(err, &ie) {
		t.Fatalf("err = %#v, want *Error", err)
	}
	if ie.Status != http.StatusBadRequest || ie.Msg != "could not extract text from document" {
		t.Fatalf("error = %d %q, want 400 could not extract text from document", ie.Status, ie.Msg)
	}
}
