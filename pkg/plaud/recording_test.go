package plaud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/dibs/pkg/store"
)

// fakeTools is an in-memory Plaud MCP session. Payloads are bare JSON
// unless a test wraps them; transcriptPages, when set for an id, is served
// page by page through next_cursor instead of transcripts.
type fakeTools struct {
	pages           []string // list_files text per page; later pages are "[]"
	listErr         error
	notes           map[string]string
	transcripts     map[string]string
	transcriptPages map[string][]string
	listArgs        []map[string]any
	transcriptArgs  []map[string]any
	closed          bool
}

func (f *fakeTools) CallTool(_ context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "list_files":
		f.listArgs = append(f.listArgs, args)
		if f.listErr != nil {
			return "", f.listErr
		}
		page := args["page"].(int)
		if page > len(f.pages) {
			return "[]", nil
		}
		return f.pages[page-1], nil
	case "get_note", "get_transcript":
		m := f.notes
		if name == "get_transcript" {
			f.transcriptArgs = append(f.transcriptArgs, args)
			m = f.transcripts
			if pages, ok := f.transcriptPages[args["file_id"].(string)]; ok {
				i := 0
				if c, ok := args["cursor"].(string); ok {
					fmt.Sscan(c, &i)
				}
				return wrapUntrusted(transcriptPage(args["file_id"].(string), pages, i)), nil
			}
		}
		v, ok := m[args["file_id"].(string)]
		if !ok {
			return "", fmt.Errorf("%s: not found", name)
		}
		return v, nil
	}
	return "", fmt.Errorf("unknown tool %s", name)
}

func (f *fakeTools) Close() error {
	f.closed = true
	return nil
}

func filesJSON(t *testing.T, files ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUnwrapUntrusted(t *testing.T) {
	payload, wrapped := unwrapUntrusted(wrapUntrusted(`{"data":[]}`) + "\n\nNote: trailer text")
	if !wrapped || payload != `{"data":[]}` {
		t.Fatalf("wrapped = %v, payload = %q", wrapped, payload)
	}
	// A nested closing tag with a different name does not end the block.
	inner := "{\"x\":\"</untrusted-user-data-ffff>\"}"
	if payload, wrapped := unwrapUntrusted(wrapUntrusted(inner)); !wrapped || payload != inner {
		t.Fatalf("nested tag: wrapped = %v, payload = %q", wrapped, payload)
	}
	if payload, wrapped := unwrapUntrusted("  bare  "); wrapped || payload != "bare" {
		t.Fatalf("bare: wrapped = %v, payload = %q", wrapped, payload)
	}
}

func TestDecodeRecordingsShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ids  []string
	}{
		{"empty", "  ", nil},
		{"array", `[{"id":"a"},"junk",{"name":"no id"}]`, []string{"a"}},
		{"api object", `{"data":[{"id":"c"}],"page":1,"has_next":false}`, []string{"c"}},
		{"numeric id", `[{"id":42}]`, []string{"42"}},
		{"envelope", wrapUntrusted(`{"data":[{"id":"e"}]}`), []string{"e"}},
		{"envelope with trailer", wrapUntrusted(`{"data":[{"id":"f"}]}`) + "\n\nNote: something", []string{"f"}},
		{"unknown object", `{"count":0}`, nil},
	}
	for _, tc := range cases {
		recs, err := decodeRecordings(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var ids []string
		for _, r := range recs {
			ids = append(ids, r.ID)
		}
		if fmt.Sprint(ids) != fmt.Sprint(tc.ids) {
			t.Errorf("%s: ids = %v, want %v", tc.name, ids, tc.ids)
		}
	}
	recs, err := decodeRecordings(wrapUntrusted(`{"data":[{"id":"t","name":" Standup ","created_at":"2026-11-03T10:00:00","start_at":"2026-11-04","duration":1500},{"id":"v","created_at":"2026-11-05T01:02:03.5Z","start_at":-1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := recs[0]; r.Name != "Standup" || r.CreatedAt.Hour() != 10 || r.StartAt.Day() != 4 || r.DurationMS != 1500 {
		t.Errorf("zone-less times = %+v", r)
	}
	if r := recs[1]; r.CreatedAt.Second() != 3 || !r.StartAt.IsZero() || !r.When().Equal(r.CreatedAt) {
		t.Errorf("RFC3339 time / When fallback = %+v", r)
	}
	for _, in := range []string{"not json at all", wrapUntrusted("not json either"), "Found 1 file:\n[{\"id\":\"e\"}]"} {
		if _, err := decodeRecordings(in); err == nil {
			t.Fatalf("%q: want error", in)
		}
	}
}

func TestRenderText(t *testing.T) {
	cases := map[string]string{
		// Unwrapped prose is a server status message: not ready.
		`Block "transaction" has no content for this recording yet.`:                       "",
		`Block "transaction" not available for this recording. Available blocks: outline.`: "",
		"  ":            "",
		`42`:            "",
		`{"unknown":1}`: "",
		// get_note payload (bare, as fakes emit it, and enveloped).
		`[{"data_type":"auto_sum_note","data_content":"Sum"},{"data_type":"high_light","data_content":""}]`: "Sum",
		wrapUntrusted(`[{"data_type":"auto_sum_note","data_content":"# Title\n\nBody"}]`):                   "# Title\n\nBody",
		// get_transcript page.
		wrapUntrusted(`{"next_cursor":null,"segments":[{"speaker":"A","content":"hi","start_time":0},{"speaker":"","content":"yo"},{"other":1}]}`): "A: hi\nyo",
		`{"marks":[{"content":"flagged"}]}`: "flagged",
		// Wrapped non-JSON block content is used verbatim.
		wrapUntrusted("plain block text"): "plain block text",
		wrapUntrusted("[]"):               "",
	}
	for in, want := range cases {
		if got := renderText(in); got != want {
			t.Errorf("renderText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdeaTitle(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		r    Recording
		want string
	}{
		{Recording{ID: "x", Name: "Named"}, "Named"},
		{Recording{ID: "x", StartAt: at}, "Plaud recording 2026-11-10 09:30 UTC"},
		{Recording{ID: "x"}, "Plaud recording x"},
	}
	for _, tc := range cases {
		if got := ideaTitle(tc.r); got != tc.want {
			t.Errorf("ideaTitle(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
	long := ideaTitle(Recording{ID: "x", Name: strings.Repeat("é", store.MaxTitleLen+5)})
	if utf8.RuneCountInString(long) != store.MaxTitleLen {
		t.Fatalf("long title has %d runes, want %d", utf8.RuneCountInString(long), store.MaxTitleLen)
	}
}

func TestIdeaDescription(t *testing.T) {
	r := Recording{ID: "rec-9", StartAt: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), DurationMS: 61400}
	got := ideaDescription(r, "Summary text", "Transcript text")
	for _, want := range []string{"recorded 2026-11-10T09:00:00Z", "(1m1s)", "Plaud file rec-9", "## Summary\n\nSummary text", "## Transcript\n\nTranscript text"} {
		if !strings.Contains(got, want) {
			t.Errorf("description missing %q:\n%s", want, got)
		}
	}
	bare := ideaDescription(Recording{ID: "z"}, "", "only transcript")
	if strings.Contains(bare, "recorded") || strings.Contains(bare, "## Summary") || !strings.Contains(bare, "only transcript") {
		t.Errorf("bare description = %q", bare)
	}
	huge := ideaDescription(r, "", strings.Repeat("ü", store.MaxBodyBytes))
	if len(huge) > store.MaxBodyBytes || !utf8.ValidString(huge) || !strings.HasSuffix(huge, truncatedMarker) {
		t.Fatalf("huge description: %d bytes, valid=%v", len(huge), utf8.ValidString(huge))
	}
}

func TestListRecordingsPaginates(t *testing.T) {
	var page1, page2 []map[string]any
	for i := 0; i < listPageSize; i++ {
		page1 = append(page1, map[string]any{"id": fmt.Sprintf("p1-%d", i), "start_at": "2026-11-10T00:00:00Z"})
	}
	page2 = append(page2,
		map[string]any{"id": "p1-0"},
		map[string]any{"id": "old", "start_at": "2026-10-01T00:00:00Z"},
		map[string]any{"id": "p2-new"},
	)
	tools := &fakeTools{pages: []string{filesJSON(t, page1...), filesJSON(t, page2...)}}
	since := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	recs, err := listRecordings(context.Background(), tools, since, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != listPageSize+1 || recs[len(recs)-1].ID != "p2-new" {
		t.Fatalf("got %d recordings (last %q), want page 1 plus p2-new", len(recs), recs[len(recs)-1].ID)
	}
	if len(tools.listArgs) != 2 || tools.listArgs[0]["date_from"] != "2026-11-09" {
		t.Fatalf("list args = %v", tools.listArgs)
	}

	capped, err := listRecordings(context.Background(), &fakeTools{pages: []string{filesJSON(t, page1...)}}, time.Time{}, 3)
	if err != nil || len(capped) != 3 {
		t.Fatalf("capped = %d, %v; want 3", len(capped), err)
	}
}

func TestListRecordingsErrors(t *testing.T) {
	if _, err := listRecordings(context.Background(), &fakeTools{listErr: errors.New("401")}, time.Time{}, 10); err == nil {
		t.Fatal("list error: want error")
	}
	if _, err := listRecordings(context.Background(), &fakeTools{pages: []string{"garbage"}}, time.Time{}, 10); err == nil {
		t.Fatal("decode error: want error")
	}
}

func TestBuildIdea(t *testing.T) {
	r := Recording{ID: "a", Name: "Idea"}
	tools := &fakeTools{notes: map[string]string{"a": `[{"data_content":"note"}]`, "empty": " "}, transcripts: map[string]string{"empty": "[]"}}
	idea, err := buildIdea(context.Background(), tools, r, []string{"plaud"})
	if err != nil || idea.Title != "Idea" || !strings.Contains(idea.Description, "note") || strings.Contains(idea.Description, "## Transcript") || idea.Tags[0] != "plaud" {
		t.Fatalf("note only: %+v, %v", idea, err)
	}
	_, err = buildIdea(context.Background(), tools, Recording{ID: "missing"}, nil)
	if err == nil || errors.Is(err, errNotReady) || !strings.Contains(err.Error(), "get_note") || !strings.Contains(err.Error(), "get_transcript") {
		t.Fatalf("both calls failing: err = %v, want both tool errors", err)
	}
	if _, err := buildIdea(context.Background(), tools, Recording{ID: "empty"}, nil); !errors.Is(err, errNotReady) {
		t.Fatalf("empty note and transcript: err = %v, want errNotReady", err)
	}
}

func TestBuildIdeaNotReadyPlaceholder(t *testing.T) {
	// The real server answers with unwrapped prose while a block is still
	// being produced; that must not be submitted as the transcript.
	tools := &fakeTools{
		notes:       map[string]string{"p": wrapUntrusted("[]")},
		transcripts: map[string]string{"p": `Block "transaction" has no content for this recording yet.`},
	}
	if _, err := buildIdea(context.Background(), tools, Recording{ID: "p"}, nil); !errors.Is(err, errNotReady) {
		t.Fatalf("placeholder: err = %v, want errNotReady", err)
	}
}

func TestFetchTranscriptPaginates(t *testing.T) {
	tools := &fakeTools{transcriptPages: map[string][]string{"m": {"one", "two", "three"}}}
	got, err := fetchTranscript(context.Background(), tools, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Me: one\nMe: two\nMe: three" {
		t.Fatalf("transcript = %q", got)
	}
	if len(tools.transcriptArgs) != 3 || tools.transcriptArgs[0]["cursor"] != nil || tools.transcriptArgs[1]["cursor"] != "1" || tools.transcriptArgs[2]["cursor"] != "2" {
		t.Fatalf("transcript args = %v", tools.transcriptArgs)
	}

	// Stops once the body cap is exceeded even if pages remain.
	var huge []string
	for i := 0; i < 10; i++ {
		huge = append(huge, strings.Repeat("x", store.MaxBodyBytes/4))
	}
	tools = &fakeTools{transcriptPages: map[string][]string{"h": huge}}
	if _, err := fetchTranscript(context.Background(), tools, "h"); err != nil {
		t.Fatal(err)
	}
	if n := len(tools.transcriptArgs); n < 4 || n >= 10 {
		t.Fatalf("made %d transcript calls, want to stop soon after the body cap", n)
	}

	if _, err := fetchTranscript(context.Background(), &fakeTools{}, "none"); err == nil {
		t.Fatal("missing transcript: want error")
	}
}
