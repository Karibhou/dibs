package plaud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/dibs/pkg/store"
)

// listPageSize is the list_files page size requested.
const listPageSize = 50

// truncatedMarker ends a description cut to fit the idea body cap.
const truncatedMarker = "\n\n_[transcript truncated to fit the Dibs idea size limit]_\n"

// maxTranscriptPages bounds the get_transcript next_cursor walk per
// recording (50 utterances a page by default), on top of the body-size stop.
const maxTranscriptPages = 100

// untrustedOpen matches the opening tag of the @plaud-ai/mcp envelope. The
// prose preamble names the tag too, but bare (<tag>); only the real opening
// tag carries attributes (source="plaud-recording"). The closing tag repeats
// the same random name.
var untrustedOpen = regexp.MustCompile(`<(untrusted-user-data-[0-9a-fA-F]+)\s[^>]*>`)

// errNotReady marks a recording with neither a note nor a transcript yet;
// it is retried on the next run.
var errNotReady = errors.New("no note or transcript available yet")

// Recording is one Plaud recording as listed by list_files.
type Recording struct {
	ID         string
	Name       string
	CreatedAt  time.Time
	StartAt    time.Time
	DurationMS int64
}

// When is the best known recording time (zero if unknown).
func (r Recording) When() time.Time {
	if !r.StartAt.IsZero() {
		return r.StartAt
	}
	return r.CreatedAt
}

// Idea is what gets submitted to Dibs for one recording.
type Idea struct {
	Title       string
	Description string
	Tags        []string
}

// listRecordings pages through list_files from since until a page is short,
// adds nothing new, or limit recordings are collected. Recordings older than
// since are dropped client-side too, in case the server ignores date_from.
func listRecordings(ctx context.Context, tools Tools, since time.Time, limit int) ([]Recording, error) {
	seen := map[string]bool{}
	var out []Recording
	for page := 1; len(out) < limit; page++ {
		args := map[string]any{"page": page, "page_size": listPageSize}
		if !since.IsZero() {
			args["date_from"] = since.UTC().Format("2006-01-02")
		}
		text, err := tools.CallTool(ctx, "list_files", args)
		if err != nil {
			return nil, err
		}
		recs, err := decodeRecordings(text)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, r := range recs {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			if w := r.When(); !since.IsZero() && !w.IsZero() && w.Before(since) {
				continue
			}
			out = append(out, r)
			added++
			if len(out) >= limit {
				break
			}
		}
		if added == 0 || len(recs) < listPageSize {
			break
		}
	}
	return out, nil
}

// unwrapUntrusted strips the envelope @plaud-ai/mcp puts around every
// successful tool payload:
//
//	The block delimited by <untrusted-user-data-HEX> below contains ...
//	<untrusted-user-data-HEX source="plaud-recording">
//	PAYLOAD
//	</untrusted-user-data-HEX>
//	optional trailer notes
//
// It returns the payload and whether an envelope was found. Text without an
// envelope is returned trimmed, so a fake server may answer with bare JSON.
func unwrapUntrusted(text string) (string, bool) {
	loc := untrustedOpen.FindStringSubmatchIndex(text)
	if loc == nil {
		return strings.TrimSpace(text), false
	}
	body := text[loc[1]:]
	closing := "</" + text[loc[2]:loc[3]] + ">"
	if end := strings.LastIndex(body, closing); end >= 0 {
		body = body[:end]
	}
	return strings.TrimSpace(body), true
}

// decodeRecordings parses a list_files result from @plaud-ai/mcp (0.3.x).
// Inside the untrusted envelope the payload is the Plaud API response
// GET /open/third-party/files/, i.e.
//
//	{"data": [{"id": "...", "name": "...", "created_at": "2026-11-10T09:00:00",
//	           "start_at": "...", "duration": 65000, ...}, ...], ...}
//
// Timestamps are strings (RFC 3339, sometimes without a zone; UTC assumed);
// duration is milliseconds. A bare array is accepted too, for fakes.
func decodeRecordings(text string) ([]Recording, error) {
	payload, _ := unwrapUntrusted(text)
	if payload == "" {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, fmt.Errorf("plaud: list_files returned non-JSON output: %s", firstLine(payload))
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case map[string]any:
		items, _ = v["data"].([]any)
	}
	var out []Recording
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		r := Recording{
			ID:        stringField(m, "id"),
			Name:      stringField(m, "name"),
			CreatedAt: timeField(m, "created_at"),
			StartAt:   timeField(m, "start_at"),
		}
		if d, ok := m["duration"].(float64); ok {
			r.DurationMS = int64(d)
		}
		if r.ID != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
	}
	return ""
}

func timeField(m map[string]any, keys ...string) time.Time {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02"} {
				if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
					return t.UTC()
				}
			}
		case float64:
			if v > 1e12 {
				return time.UnixMilli(int64(v)).UTC()
			}
			if v > 0 {
				return time.Unix(int64(v), 0).UTC()
			}
		}
	}
	return time.Time{}
}

// renderText turns a get_note or get_transcript result into plain text.
// The JSON inside the envelope is rendered by renderValue. A result with
// no envelope is a server-side status message ("Block "transaction" has no
// content for this recording yet.", "... not available ...") rather than
// recording data, so it renders as "" (not ready), unless it is bare JSON
// as emitted by fakes.
func renderText(text string) string {
	payload, wrapped := unwrapUntrusted(text)
	var raw any
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		if wrapped {
			return payload
		}
		return ""
	}
	return strings.TrimSpace(renderValue(raw))
}

// renderValue renders the shapes @plaud-ai/mcp returns:
//
//   - get_note: an array of note blocks
//     [{"data_type": "auto_sum_note", "data_content": "<markdown>", ...}, ...]
//   - get_transcript: one page
//     {"segments": [{"speaker": "A", "content": "...", "start_time": 0, ...}],
//     "next_cursor": "..." | null, ...}
//     ("marks" for the mark_memo block, which is not requested here), or a
//     bare array of blocks when the recording has none yet.
//
// Blocks and segments are joined one per line; a segment is prefixed with
// its speaker.
func renderValue(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []any:
		lines := make([]string, 0, len(v))
		for _, it := range v {
			if line := strings.TrimSpace(renderValue(it)); line != "" {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	case map[string]any:
		for _, key := range []string{"data_content", "content", "segments", "marks"} {
			inner, ok := v[key]
			if !ok {
				continue
			}
			body := strings.TrimSpace(renderValue(inner))
			if body == "" {
				continue
			}
			if speaker := stringField(v, "speaker"); speaker != "" && key == "content" {
				return speaker + ": " + body
			}
			return body
		}
	}
	return ""
}

// nextCursor returns the next_cursor of a get_transcript page, or "" on the
// last page (or for anything that is not a paged result).
func nextCursor(text string) string {
	payload, _ := unwrapUntrusted(text)
	var page struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(payload), &page); err != nil {
		return ""
	}
	return page.NextCursor
}

// fetchTranscript walks get_transcript pages via next_cursor and joins the
// rendered utterances. It stops early once the text already exceeds the
// idea body cap, since ideaDescription truncates there anyway.
func fetchTranscript(ctx context.Context, tools Tools, id string) (string, error) {
	var b strings.Builder
	cursor := ""
	for page := 0; page < maxTranscriptPages; page++ {
		args := map[string]any{"file_id": id}
		if cursor != "" {
			args["cursor"] = cursor
		}
		text, err := tools.CallTool(ctx, "get_transcript", args)
		if err != nil {
			return "", err
		}
		if part := renderText(text); part != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part)
		}
		cursor = nextCursor(text)
		if cursor == "" || b.Len() > store.MaxBodyBytes {
			break
		}
	}
	return b.String(), nil
}

// buildIdea fetches the note and transcript for r and renders the idea.
// A recording whose note and transcript both fail or are empty is reported
// as an error so it is retried later rather than marked as seen.
func buildIdea(ctx context.Context, tools Tools, r Recording, tags []string) (Idea, error) {
	note, noteErr := tools.CallTool(ctx, "get_note", map[string]any{"file_id": r.ID})
	transcript, trErr := fetchTranscript(ctx, tools, r.ID)
	if noteErr != nil && trErr != nil {
		return Idea{}, errors.Join(noteErr, trErr)
	}
	note = renderText(note)
	if note == "" && transcript == "" {
		return Idea{}, errNotReady
	}
	return Idea{Title: ideaTitle(r), Description: ideaDescription(r, note, transcript), Tags: tags}, nil
}

func ideaTitle(r Recording) string {
	title := r.Name
	if title == "" {
		if w := r.When(); !w.IsZero() {
			title = "Plaud recording " + w.UTC().Format("2006-01-02 15:04 UTC")
		} else {
			title = "Plaud recording " + r.ID
		}
	}
	return capRunes(title, store.MaxTitleLen)
}

func ideaDescription(r Recording, note, transcript string) string {
	var b strings.Builder
	b.WriteString("_Voice note captured with Plaud")
	if w := r.When(); !w.IsZero() {
		fmt.Fprintf(&b, ", recorded %s", w.UTC().Format(time.RFC3339))
	}
	if r.DurationMS > 0 {
		fmt.Fprintf(&b, " (%s)", (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second))
	}
	fmt.Fprintf(&b, "; Plaud file %s._\n\n", r.ID)
	if note != "" {
		b.WriteString("## Summary\n\n" + note + "\n\n")
	}
	if transcript != "" {
		b.WriteString("## Transcript\n\n" + transcript + "\n")
	}
	return capBytes(b.String(), store.MaxBodyBytes)
}

// capBytes cuts s to at most limit bytes on a UTF-8 boundary, ending it with
// truncatedMarker when cut.
func capBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len(truncatedMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}

func capRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}
