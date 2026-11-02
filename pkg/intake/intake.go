// Package intake extracts idea text from uploaded documents or transcribed audio.
package intake

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/dibs/pkg/envutil"
	"github.com/ledongthuc/pdf"
	nethtml "golang.org/x/net/html"
	"golang.org/x/text/encoding/charmap"
)

const (
	DefaultMaxUploadMB = 25
	MaxReturnedRunes   = 20000
	extractRunesLimit  = MaxReturnedRunes + 1
	// maxDOCXXMLBytes caps the DECOMPRESSED word/document.xml bytes fed to
	// the XML decoder (see ExtractDOCX). Far above any real document that
	// yields MaxReturnedRunes of text, far below the pod memory limit.
	maxDOCXXMLBytes = 16 << 20
	sttTimeout      = 60 * time.Second
)

var (
	DocumentExts = map[string]bool{".txt": true, ".md": true, ".html": true, ".htm": true, ".pdf": true, ".docx": true, ".rtf": true}
	AudioExts    = map[string]bool{".mp3": true, ".m4a": true, ".wav": true, ".ogg": true, ".webm": true, ".flac": true}
)

type Response struct {
	Text      string `json:"text"`
	Kind      string `json:"kind"`
	Truncated bool   `json:"truncated"`
}

type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func MaxUploadBytes() int64 {
	mb := DefaultMaxUploadMB
	if raw := strings.TrimSpace(envutil.Get("DIBS_INTAKE_MAX_MB")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			mb = n
		}
	}
	return int64(mb) * 1024 * 1024
}

func AudioEnabled() bool { return strings.TrimSpace(envutil.Get("DIBS_STT_URL")) != "" }

func HandleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"audio": AudioEnabled()})
}

func HandleUpload(w http.ResponseWriter, r *http.Request) {
	limit := MaxUploadBytes()
	r.Body = http.MaxBytesReader(w, r.Body, limit+1024*1024)
	if err := r.ParseMultipartForm(limit); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "intake upload is too large")
		} else {
			writeError(w, http.StatusBadRequest, "malformed multipart request")
		}
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, `multipart field "file" is required`)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading uploaded file")
		return
	}
	if int64(len(data)) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "intake upload is too large")
		return
	}
	resp, err := ProcessWithContext(r.Context(), header.Filename, header.Header.Get("Content-Type"), data)
	if err != nil {
		var ie *Error
		if errors.As(err, &ie) {
			writeError(w, ie.Status, ie.Msg)
		} else {
			writeError(w, http.StatusInternalServerError, "intake failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func ProcessWithContext(ctx context.Context, filename, declaredType string, data []byte) (Response, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	sniff := http.DetectContentType(firstBytes(data, 512))
	if DocumentExts[ext] {
		if !sniffMatchesDocument(ext, sniff, declaredType) {
			return Response{}, &Error{Status: http.StatusUnsupportedMediaType, Msg: "unsupported or mismatched document type"}
		}
		text, err := ExtractDocument(ext, data)
		if err != nil {
			return Response{}, &Error{Status: http.StatusBadRequest, Msg: "could not extract text from document"}
		}
		text, truncated := capRunes(cleanText(text), MaxReturnedRunes)
		return Response{Text: text, Kind: "document", Truncated: truncated}, nil
	}
	if AudioExts[ext] {
		if !sniffMatchesAudio(ext, sniff, declaredType) {
			return Response{}, &Error{Status: http.StatusUnsupportedMediaType, Msg: "unsupported or mismatched audio type"}
		}
		text, err := TranscribeWithContext(ctx, filename, declaredType, data)
		if err != nil {
			return Response{}, err
		}
		text, truncated := capRunes(cleanText(text), MaxReturnedRunes)
		return Response{Text: text, Kind: "audio", Truncated: truncated}, nil
	}
	return Response{}, &Error{Status: http.StatusUnsupportedMediaType, Msg: "unsupported intake file type"}
}

func ExtractDocument(ext string, data []byte) (string, error) {
	switch ext {
	case ".txt", ".md":
		return string(data), nil
	case ".html", ".htm":
		return StripTags(string(data)), nil
	case ".pdf":
		return ExtractPDF(data)
	case ".docx":
		return ExtractDOCX(data)
	case ".rtf":
		return StripRTF(string(data)), nil
	default:
		return "", fmt.Errorf("unsupported extension %s", ext)
	}
}

// ExtractPDF pulls plain text out of a PDF page by page, stopping as soon
// as extractRunesLimit runes have been collected. Reader.GetPlainText would
// materialize every page's decompressed text in one buffer, so a crafted
// PDF whose FlateDecode streams expand ~1000:1 could turn a within-cap
// upload into multi-GB allocations; the caller only ever keeps
// MaxReturnedRunes anyway. The parser also panics on malformed input
// (reader-level entry points have no recover), so panics are converted to
// ordinary errors here.
func ExtractPDF(data []byte) (text string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			text, err = "", fmt.Errorf("malformed PDF: %v", rec)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	fonts := make(map[string]*pdf.Font)
	var b strings.Builder
	runes := 0
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		for _, name := range p.Fonts() {
			if _, ok := fonts[name]; !ok {
				f := p.Font(name)
				fonts[name] = &f
			}
		}
		pageText, err := p.GetPlainText(fonts)
		if err != nil {
			return "", err
		}
		runes += writeLimitedRunes(&b, pageText, extractRunesLimit-runes)
		if runes >= extractRunesLimit {
			break
		}
	}
	return b.String(), nil
}

func ExtractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		// The rune cap below bounds what is KEPT, not what the decoder must
		// hold: xml.Decoder.Token materializes a whole CharData node before
		// returning it, and the zip entry's decompressed size is unchecked,
		// so one deflate-packed text node would inflate ~1000:1 into the
		// heap before the cap is ever consulted. Budget the decompressed
		// bytes instead; the decoder then sees EOF at the budget and the
		// text gathered so far is returned, as with the rune cap.
		lr := &io.LimitedReader{R: rc, N: maxDOCXXMLBytes}
		dec := xml.NewDecoder(lr)
		var b strings.Builder
		runes := 0
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				if lr.N <= 0 {
					return b.String(), nil
				}
				return "", err
			}
			switch t := tok.(type) {
			case xml.CharData:
				runes += writeLimitedRunes(&b, string(t), extractRunesLimit-runes)
			case xml.EndElement:
				if t.Name.Local == "p" || t.Name.Local == "br" || t.Name.Local == "tab" {
					runes += writeLimitedRunes(&b, "\n", extractRunesLimit-runes)
				}
			}
			if runes >= extractRunesLimit {
				return b.String(), nil
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("word/document.xml not found")
}

// StripTags reduces HTML to its text. Comments and the contents of
// htmlSkippedElements are dropped as a whole, since they are not rendered
// text and a "Save as HTML" page is mostly inline CSS and JS.
func StripTags(s string) string {
	var b strings.Builder
	z := nethtml.NewTokenizer(strings.NewReader(s))
	spacePending := false
	skip := ""
	depth := 0
	for {
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			return b.String()
		}
		switch tt {
		case nethtml.StartTagToken, nethtml.EndTagToken, nethtml.SelfClosingTagToken:
			name, _ := z.TagName()
			n := string(name)
			spacePending = true
			if skip == "" {
				if tt != nethtml.EndTagToken && slices.Contains(htmlSkippedElements, n) {
					skip, depth = n, 1
				}
				continue
			}
			if n != skip {
				continue
			}
			// Only template can nest; the others are raw text.
			switch tt {
			case nethtml.StartTagToken:
				depth++
			case nethtml.EndTagToken:
				depth--
			}
			if depth == 0 {
				skip = ""
			}
		case nethtml.TextToken:
			if skip != "" {
				continue
			}
			if spacePending {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				spacePending = false
			}
			b.Write(z.Text())
		case nethtml.CommentToken, nethtml.DoctypeToken:
			spacePending = true
		}
	}
}

// htmlSkippedElements hold no rendered document text.
var htmlSkippedElements = []string{"script", "style", "noscript", "template"}

func StripRTF(s string) string {
	var b strings.Builder
	// Word, WordPad and LibreOffice always emit exactly one fallback unit
	// after \uN, so group-scoped \uc tracking is unnecessary.
	ucSkip := 1
	var highSurrogate rune
	decodeByte := windows1252Rune
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '{':
			if rtfSkippedDestination(s, i+1) {
				i = skipRTFGroup(s, i)
				continue
			}
			i++
		case '}':
			i++
		case '\\':
			i++
			if i >= len(s) {
				break
			}
			if s[i] == '\\' || s[i] == '{' || s[i] == '}' {
				b.WriteByte(s[i])
				i++
				continue
			}
			if s[i] == '\'' && i+2 < len(s) {
				if v, ok := parseHexByte(s[i+1], s[i+2]); ok {
					if v >= 0x80 {
						b.WriteRune(decodeByte(v))
					} else {
						b.WriteByte(v)
					}
				}
				i += 3
				continue
			}
			wordStart := i
			for i < len(s) && ((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z')) {
				i++
			}
			word := s[wordStart:i]
			hasParam, neg, n := false, false, 0
			if i < len(s) && (s[i] == '-' || (s[i] >= '0' && s[i] <= '9')) {
				if s[i] == '-' {
					neg = true
				} else {
					hasParam, n = true, int(s[i]-'0')
				}
				i++
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					hasParam = true
					if n < 1<<20 {
						n = n*10 + int(s[i]-'0')
					}
					i++
				}
			}
			if neg {
				n = -n
			}
			if i < len(s) && s[i] == ' ' {
				i++
			}
			switch word {
			case "uc":
				if hasParam && n >= 0 {
					ucSkip = n
				}
			case "ansicpg":
				if hasParam {
					decodeByte = rtfCodepageDecoder(n)
				}
			case "u":
				if !hasParam {
					b.WriteByte(' ')
					break
				}
				if n < 0 {
					n += 65536
				}
				r := rune(n)
				switch {
				case r >= 0xD800 && r <= 0xDBFF:
					highSurrogate = r
				case r >= 0xDC00 && r <= 0xDFFF && highSurrogate != 0:
					b.WriteRune(0x10000 + (highSurrogate-0xD800)<<10 + (r - 0xDC00))
					highSurrogate = 0
				default:
					highSurrogate = 0
					b.WriteRune(r)
				}
				for k := 0; k < ucSkip && i < len(s); k++ {
					if s[i] == '\\' && i+3 < len(s) && s[i+1] == '\'' {
						i += 4
					} else if s[i] != '\\' && s[i] != '{' && s[i] != '}' {
						i++
					} else {
						break
					}
				}
			case "par", "line", "sect", "page":
				b.WriteByte('\n')
			default:
				b.WriteByte(' ')
			}
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			b.WriteRune(r)
			i += size
		}
	}
	return b.String()
}

var windows1252High = [32]rune{
	0x20AC, 0x81, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x8D, 0x017D, 0x8F,
	0x90, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x9D, 0x017E, 0x0178,
}

// windows1252Rune decodes a byte >= 0x80 from Windows-1252.
func windows1252Rune(v byte) rune {
	if v < 0xA0 {
		return windows1252High[v-0x80]
	}
	return rune(v)
}

// rtfCodepages are the single-byte code pages an RTF \ansicpgN may name,
// besides 1252, which windows1252Rune handles.
var rtfCodepages = map[int]*charmap.Charmap{
	437: charmap.CodePage437, 850: charmap.CodePage850, 852: charmap.CodePage852,
	866: charmap.CodePage866, 874: charmap.Windows874,
	1250: charmap.Windows1250, 1251: charmap.Windows1251, 1253: charmap.Windows1253,
	1254: charmap.Windows1254, 1255: charmap.Windows1255, 1256: charmap.Windows1256,
	1257: charmap.Windows1257, 1258: charmap.Windows1258,
	10000: charmap.Macintosh, 10007: charmap.MacintoshCyrillic,
	20866: charmap.KOI8R, 21866: charmap.KOI8U,
}

// rtfCodepageDecoder returns the \'xx decoder for an \ansicpgN document. A
// code page it cannot decode, such as the double-byte 932/936/949/950,
// yields U+FFFD rather than another code page's letters.
func rtfCodepageDecoder(cp int) func(byte) rune {
	if cp == 1252 {
		return windows1252Rune
	}
	if cm, ok := rtfCodepages[cp]; ok {
		return cm.DecodeByte
	}
	return func(byte) rune { return utf8.RuneError }
}

var rtfSkippedDestinations = map[string]bool{
	"fonttbl": true, "colortbl": true, "stylesheet": true, "listtable": true,
	"listoverridetable": true, "info": true, "pict": true, "object": true,
	"header": true, "headerl": true, "headerr": true, "headerf": true,
	"footer": true, "footerl": true, "footerr": true, "footerf": true,
	"fldinst": true, "datastore": true, "themedata": true,
	"colorschememapping": true, "latentstyles": true, "xmlnstbl": true,
	"rsidtbl": true, "generator": true,
}

// rtfSkippedDestination reports whether the group whose first byte after the
// opening brace is at i is a destination that holds no document text.
func rtfSkippedDestination(s string, i int) bool {
	if i+1 >= len(s) || s[i] != '\\' {
		return false
	}
	if s[i+1] == '*' {
		return true
	}
	j := i + 1
	for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
		j++
	}
	return rtfSkippedDestinations[s[i+1:j]]
}

// skipRTFGroup returns the index just past the group opened at s[i],
// honouring escaped braces and nested groups.
func skipRTFGroup(s string, i int) int {
	depth := 0
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return len(s)
}

func TranscribeWithContext(ctx context.Context, filename, contentType string, data []byte) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(envutil.Get("DIBS_STT_URL")), "/")
	if baseURL == "" {
		return "", &Error{Status: http.StatusNotImplemented, Msg: "audio transcription is not configured"}
	}
	key := strings.TrimSpace(envutil.Get("DIBS_STT_KEY"))
	model := strings.TrimSpace(envutil.Get("DIBS_STT_MODEL"))
	if model == "" {
		model = "whisper-1"
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", model)
	// Quiet mics get fully discarded by server-side voice-activity detection;
	// let whisper decide what is speech.
	_ = mw.WriteField("vad_filter", "false")
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+escapeQuotes(filename)+`"`)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: sttTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Text  string `json:"text"`
		Error any    `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &Error{Status: http.StatusBadGateway, Msg: "audio transcription failed"}
	}
	return out.Text, nil
}

func sniffMatchesDocument(ext, sniff, declared string) bool {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	sniff = strings.ToLower(sniff)
	switch ext {
	case ".pdf":
		return sniff == "application/pdf" || declared == "application/pdf"
	case ".docx":
		return sniff == "application/zip" || sniff == "application/octet-stream" || strings.Contains(declared, "wordprocessingml.document") || declared == "application/zip"
	case ".html", ".htm":
		return strings.HasPrefix(sniff, "text/") || declared == "text/html"
	case ".txt", ".md", ".rtf":
		return strings.HasPrefix(sniff, "text/") || sniff == "application/octet-stream" || strings.HasPrefix(declared, "text/") || declared == "application/rtf"
	default:
		return false
	}
}

func sniffMatchesAudio(ext, sniff, declared string) bool {
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	sniff = strings.ToLower(sniff)
	if strings.HasPrefix(declared, "audio/") || declared == "video/webm" || declared == "application/ogg" {
		return true
	}
	switch ext {
	case ".wav":
		return strings.Contains(sniff, "wave") || strings.Contains(sniff, "wav") || sniff == "application/octet-stream"
	case ".mp3":
		return sniff == "audio/mpeg" || sniff == "application/octet-stream"
	case ".ogg", ".flac", ".m4a", ".webm":
		return strings.HasPrefix(sniff, "audio/") || strings.HasPrefix(sniff, "video/") || sniff == "application/octet-stream" || sniff == "application/ogg"
	default:
		return false
	}
}

func capRunes(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= max {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String(), true
}

func writeLimitedRunes(b *strings.Builder, s string, remaining int) int {
	written := 0
	for _, r := range s {
		if remaining <= 0 {
			return written
		}
		b.WriteRune(r)
		remaining--
		written++
	}
	return written
}

func cleanText(s string) string {
	var b strings.Builder
	lastSpace := false
	newlines := 0
	for _, r := range strings.TrimSpace(s) {
		if r == '\r' {
			continue
		}
		if r == '\n' {
			if newlines < 2 {
				b.WriteByte('\n')
			}
			newlines++
			lastSpace = false
			continue
		}
		newlines = 0
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func firstBytes(data []byte, n int) []byte {
	if len(data) < n {
		return data
	}
	return data[:n]
}

func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, `\"`).Replace(s)
}

func parseHexByte(a, b byte) (byte, bool) {
	hi, ok := hexVal(a)
	if !ok {
		return 0, false
	}
	lo, ok := hexVal(b)
	if !ok {
		return 0, false
	}
	return hi<<4 | lo, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
