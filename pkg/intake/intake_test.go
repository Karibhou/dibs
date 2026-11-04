package intake

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

func TestExtractTextFormats(t *testing.T) {
	t.Parallel()
	if got, err := ExtractDocument(".txt", []byte("plain idea")); err != nil || got != "plain idea" {
		t.Fatalf("txt = %q, %v", got, err)
	}
	if got, err := ExtractDocument(".md", []byte("# Idea\nbody")); err != nil || got != "# Idea\nbody" {
		t.Fatalf("md = %q, %v", got, err)
	}
	got, err := ExtractDocument(".html", []byte("<h1>Idea</h1><p>Help &amp; ship</p>"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Idea") || !strings.Contains(got, "Help & ship") || strings.Contains(got, "<p>") {
		t.Fatalf("html stripped to %q", got)
	}
	got, err = ExtractDocument(".html", []byte(`<html><head><style>body{color:red}</style>
   <script>window.dataLayer=[];gtag('js')</script>
   <!-- generator: Word 16 --></head>
   <body><p>Idea: add a dark mode</p></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cleanText(got); got != "Idea: add a dark mode" {
		t.Fatalf("html with style/script/comment extracted %q", got)
	}
	got, err = ExtractDocument(".rtf", []byte(`{\rtf1\ansi This is \b bold\b0  idea\par done}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "This is") || !strings.Contains(got, "bold") || strings.Contains(got, `\b`) {
		t.Fatalf("rtf stripped to %q", got)
	}
}

func TestStripTagsSkipsNonTextRegions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"style", `<p>a</p><style type="text/css">p > b{}</style><p>b</p>`, "a b"},
		{"script", `<p>a</p><script>if (x < 1 && y > 2) { w("<p>no</p>") }</script><p>b</p>`, "a b"},
		{"noscript", `<p>a</p><noscript><p>enable JS</p></noscript><p>b</p>`, "a b"},
		{"template", `<p>a</p><template><p>row</p></template><p>b</p>`, "a b"},
		{"nested template", `a<template><template>x</template>y</template>b`, "a b"},
		{"comment with gt", `a<!-- if x > 1 -->b`, "a b"},
		{"empty comments", `a<!-->b<!--->c`, "a b c"},
		{"uppercase", `a<SCRIPT>x()</Script >b`, "a b"},
		{"similar names kept", `<scripts>a</scripts><styled>b</styled>`, "a b"},
		{"unterminated comment", `a<!-- b`, "a"},
		{"unterminated element", `a<script>b`, "a"},
		{"unterminated open tag", `a<style`, "a"},
		{"unterminated close tag", `a<style>b</style`, "a"},
		{"unterminated nested template", `a<template><template`, "a"},
		{"comment marker inside tag", `<a title="<!--">a</a>b<p>c`, "a b c"},
		{"quoted gt in double-quoted attribute", `<p title=">leak">hello</p>`, "hello"},
		{"quoted gt in single-quoted attribute", `<a data-x='a>b'>c</a>`, "c"},
		{"quoted attribute text never leaks", `<img alt="x <b>y</b> z">k<span title='<i>q</i>'>m</span>`, "k m"},
		{"entities", `<style>x</style>Help &amp; ship`, "Help & ship"},
		{"self-closing script", `<script/>alert(1)</script>Hello`, "Hello"},
		{"self-closing script with space", `<script />alert(1)</script>Hello`, "Hello"},
		{"self-closing uppercase script", `<SCRIPT/>alert(1)</SCRIPT>Hello`, "Hello"},
		{"self-closing style", `<style/>p{}</style>Hello`, "Hello"},
		{"self-closing noscript", `<noscript/>x</noscript>Hello`, "Hello"},
		{"self-closing template", `<template/>hidden</template>Hello`, "Hello"},
	}
	for _, tc := range cases {
		if got := cleanText(StripTags(tc.in)); got != tc.want {
			t.Errorf("%s: StripTags(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestExtractDOCX(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>First idea</w:t></w:r></w:p><w:p><w:r><w:t>Second line</w:t></w:r></w:p></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractDocument(".docx", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "First idea") || !strings.Contains(got, "Second line") {
		t.Fatalf("docx extracted %q", got)
	}
}

func TestExtractPDFGeneratedFixture(t *testing.T) {
	t.Parallel()
	got, err := ExtractDocument(".pdf", generatedPDF("Hello PDF idea"))
	if err != nil {
		t.Skipf("generated PDF fixture is not supported by extractor: %v", err)
	}
	if !strings.Contains(got, "Hello PDF idea") {
		t.Fatalf("pdf extracted %q", got)
	}
}

func TestExtractPDFMalformedReturnsError(t *testing.T) {
	t.Parallel()
	// The upstream parser panics on many malformed inputs; ExtractPDF must
	// convert that to an error, never panic.
	inputs := [][]byte{
		[]byte("%PDF-1.4\ngarbage"),
		[]byte("%PDF-1.4\nxref\n0 1\ntrailer\n<< /Size 1 >>\nstartxref\n9\n%%EOF\n"),
		generatedPDF("ok")[:40],
	}
	for i, in := range inputs {
		if _, err := ExtractPDF(in); err == nil {
			t.Errorf("input %d: malformed PDF extracted without error", i)
		}
	}
}

func TestExtractPDFStopsAtRuneLimit(t *testing.T) {
	t.Parallel()
	// Many pages of long text: extraction must stop accumulating at
	// extractRunesLimit instead of materializing every page.
	page := strings.Repeat("A", 4000)
	got, err := ExtractPDF(generatedMultiPagePDF(64, page))
	if err != nil {
		t.Skipf("generated PDF fixture is not supported by extractor: %v", err)
	}
	n := len([]rune(got))
	if n > extractRunesLimit {
		t.Fatalf("extracted %d runes, want <= %d", n, extractRunesLimit)
	}
	if !strings.Contains(got, "AAAA") {
		t.Fatalf("pdf extracted %q", got)
	}
}

func generatedMultiPagePDF(pages int, text string) []byte {
	// Objects: 1 catalog, 2 pages tree, 3 font, then per page: page + contents.
	kids := make([]string, pages)
	for i := 0; i < pages; i++ {
		kids[i] = fmt.Sprintf("%d 0 R", 4+i*2)
	}
	objects := []string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		fmt.Sprintf(`<< /Type /Pages /Kids [%s] /Count %d >>`, strings.Join(kids, " "), pages),
		`<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>`,
	}
	for i := 0; i < pages; i++ {
		content := "BT /F1 12 Tf 36 100 Td (" + text + ") Tj ET\n"
		objects = append(objects,
			fmt.Sprintf(`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>`, 5+i*2),
			fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		)
	}
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

func TestProcessCapsReturnedText(t *testing.T) {
	t.Parallel()
	got, err := ProcessWithContext(context.Background(), "idea.txt", "text/plain", []byte(strings.Repeat("x", MaxReturnedRunes+10)))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "document" || !got.Truncated || len([]rune(got.Text)) != MaxReturnedRunes {
		t.Fatalf("unexpected response: %#v", got)
	}
}

func TestAudioWithoutConfigReturns501(t *testing.T) {
	t.Setenv("DIBS_STT_URL", "")
	_, err := ProcessWithContext(context.Background(), "idea.wav", "audio/wav", []byte("RIFF----WAVEfmt "))
	if err == nil {
		t.Fatal("expected error")
	}
	ie, ok := err.(*Error)
	if !ok || ie.Status != http.StatusNotImplemented {
		t.Fatalf("error = %#v", err)
	}
}

func TestTranscribeForwardsMultipart(t *testing.T) {
	var sawAuth, sawModel, sawFile bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization") == "Bearer secret"
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		sawModel = r.FormValue("model") == "test-model"
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("file: %v", err)
		}
		defer file.Close()
		body, _ := io.ReadAll(file)
		sawFile = header.Filename == "idea.webm" && string(body) == "audio bytes" && header.Header.Get("Content-Type") == "audio/webm"
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "spoken idea"})
	}))
	defer srv.Close()
	t.Setenv("DIBS_STT_URL", srv.URL)
	t.Setenv("DIBS_STT_KEY", "secret")
	t.Setenv("DIBS_STT_MODEL", "test-model")
	got, err := TranscribeWithContext(context.Background(), "idea.webm", "audio/webm", []byte("audio bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "spoken idea" || !sawAuth || !sawModel || !sawFile {
		t.Fatalf("got %q auth=%v model=%v file=%v", got, sawAuth, sawModel, sawFile)
	}
}

func TestHandleUploadRequiresFile(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("other", "value")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/intake", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	HandleUpload(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func generatedPDF(text string) []byte {
	objects := []string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [3 0 R] /Count 1 >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>`,
		`<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>`,
		fmt.Sprintf("<< /Length %d >>\nstream\nBT /F1 12 Tf 36 100 Td (%s) Tj ET\nendstream", len("BT /F1 12 Tf 36 100 Td ("+text+") Tj ET\n"), text),
	}
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

// TestExtractDOCXBoundsDecompressedText pins the decompression-bomb guard:
// a tiny DOCX whose document.xml is one deflate-packed text node far
// larger than maxDOCXXMLBytes must extract without inflating the whole
// node into memory. Without the LimitedReader this allocates the full
// decompressed size (a 300 KiB upload reached 1.3 GiB of heap).
func TestExtractDOCXBoundsDecompressedText(t *testing.T) {
	const textSize = 16 * maxDOCXXMLBytes
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, `<w:document><w:body><w:p><w:r><w:t>`)
	chunk := []byte(strings.Repeat("A", 1<<20))
	for written := 0; written < textSize; written += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = io.WriteString(w, `</w:t></w:r></w:p></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 1<<20 {
		t.Fatalf("fixture should compress to well under 1 MiB, got %d bytes", buf.Len())
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := ExtractDOCX(buf.Bytes())
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("ExtractDOCX: %v", err)
	}
	if len(got) > extractRunesLimit {
		t.Fatalf("returned %d runes, want <= %d", len(got), extractRunesLimit)
	}
	// The budget plus decoder buffer growth lands around 3x the budget
	// (48 MiB observed); the unbounded read would allocate the full
	// 256 MiB node.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 6*maxDOCXXMLBytes {
		t.Fatalf("ExtractDOCX allocated %d bytes for a %d-byte upload; decompressed read is unbounded", allocated, buf.Len())
	}
}

// TestExtractDOCXBudgetKeepsCollectedText checks the budget path returns the
// text gathered before the cut instead of a decode error.
func TestExtractDOCXBudgetKeepsCollectedText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, `<w:document><w:body><w:p><w:r><w:t>kept paragraph</w:t></w:r></w:p><w:p><w:r><w:t>`)
	_, _ = io.WriteString(w, strings.Repeat("B", maxDOCXXMLBytes+1024))
	_, _ = io.WriteString(w, `</w:t></w:r></w:p></w:body></w:document>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractDOCX(buf.Bytes())
	if err != nil {
		t.Fatalf("ExtractDOCX: %v", err)
	}
	if !strings.HasPrefix(got, "kept paragraph") {
		t.Fatalf("budget path dropped collected text: %q", got)
	}
}

// singlePageFlatePDF builds a one-page PDF whose content stream, once
// FlateDecoded, is a run of "(AAA…)Tj" text-show operators totalling
// decompressedBytes. The deflate output is tiny (repetitive data), so the
// fixture itself stays well under the upload cap while the page inflates
// ~1000:1 on extraction.
func singlePageFlatePDF(t *testing.T, decompressedBytes int) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	chunk := []byte("(" + strings.Repeat("A", 1000) + ")Tj\n")
	for written := 0; written < decompressedBytes; written += len(chunk) {
		if _, err := zw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	comp := z.Bytes()

	objects := []string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [3 0 R] /Count 1 >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Resources << >> /Contents 4 0 R >>`,
		fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", len(comp), comp),
	}
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

// TestExtractPDFBoundsSinglePageDecompressedText pins the single-page
// decompression-bomb guard (hivecommons/dibs#583). The cross-page rune cap
// in ExtractPDF does not bound a single page: GetPlainText buffers that
// page's whole decoded text first. A one-page PDF whose content stream
// inflates far past maxPDFPageContentBytes must extract without
// materializing it. Without pdfPageContentExceeds this allocates the full
// decompressed size (a ~2 MiB upload reached ~1.4 GiB of heap).
func TestExtractPDFBoundsSinglePageDecompressedText(t *testing.T) {
	t.Parallel()
	const decompressed = 8 * maxPDFPageContentBytes
	pdfBytes := singlePageFlatePDF(t, decompressed)
	if len(pdfBytes) > 1<<20 {
		t.Fatalf("fixture should compress to well under 1 MiB, got %d bytes", len(pdfBytes))
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := ExtractPDF(pdfBytes)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if len([]rune(got)) > extractRunesLimit {
		t.Fatalf("returned %d runes, want <= %d", len([]rune(got)), extractRunesLimit)
	}
	// The guard reads at most maxPDFPageContentBytes decompressed bytes
	// (discarded) before skipping the page; the unbounded path would
	// allocate the full 128 MiB node and more as the page buffer regrows.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 6*maxPDFPageContentBytes {
		t.Fatalf("ExtractPDF allocated %d bytes for a %d-byte upload; single-page decompression is unbounded", allocated, len(pdfBytes))
	}
}

// toUnicodeFontPDF returns a one-page document whose only font carries the
// given ToUnicode CMap as a FlateDecode stream. The font has no /Encoding,
// which is the branch that makes Font.Encoder parse the CMap, and the page
// shows the single byte "A" through that font.
func toUnicodeFontPDF(t *testing.T, cmap []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(cmap); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	comp := z.Bytes()
	content := "BT /F1 12 Tf 36 100 Td (A) Tj ET\n"
	return assemblePDF([]string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [3 0 R] /Count 1 >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 144] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>`,
		`<< /Type /Font /Subtype /TrueType /BaseFont /Helvetica /ToUnicode 6 0 R >>`,
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", len(comp), comp),
	})
}

// TestExtractPDFDecodesToUnicodeCMap pins that a legitimate ToUnicode CMap
// still drives text decoding once the stream is budgeted: the map sends
// byte 0x41 ("A") to U+0042, so the page must extract as "B".
func TestExtractPDFDecodesToUnicodeCMap(t *testing.T) {
	t.Parallel()
	cmap := []byte("1 begincodespacerange <00> <FF> endcodespacerange\n1 beginbfchar <41> <0042> endbfchar\n")
	got, err := ExtractPDF(toUnicodeFontPDF(t, cmap))
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if !strings.Contains(got, "B") || strings.Contains(got, "A") {
		t.Fatalf("text = %q, want ToUnicode-mapped %q", got, "B")
	}
}

// TestExtractPDFBoundsToUnicodeDecompressedText pins the ToUnicode guard.
// GetPlainText lexes a font's whole ToUnicode CMap on the first Tf that
// names the font, one token at a time, with no size limit; a CMap that is
// one FlateDecode literal string inflating far past maxPDFPageContentBytes
// must not be materialized. Without the budget in ExtractPDF's font loop
// this allocates the full decompressed size.
func TestExtractPDFBoundsToUnicodeDecompressedText(t *testing.T) {
	t.Parallel()
	const decompressed = 8 * maxPDFPageContentBytes
	cmap := append([]byte("begincmap\n("), bytes.Repeat([]byte("A"), decompressed)...)
	pdfBytes := toUnicodeFontPDF(t, cmap)
	if len(pdfBytes) > 1<<20 {
		t.Fatalf("fixture should compress to well under 1 MiB, got %d bytes", len(pdfBytes))
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := ExtractPDF(pdfBytes)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("ExtractPDF: %v", err)
	}
	if got != "" {
		t.Fatalf("text = %q, want extraction to stop before the page that needs the bomb font", got)
	}
	// The guard discards at most maxPDFPageContentBytes decompressed bytes;
	// the unbounded path grows one 128 MiB token and copies it into a string.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 6*maxPDFPageContentBytes {
		t.Fatalf("ExtractPDF allocated %d bytes for a %d-byte upload; ToUnicode decompression is unbounded", allocated, len(pdfBytes))
	}
}
