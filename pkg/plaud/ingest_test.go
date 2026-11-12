package plaud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/dibs/pkg/store"
)

// fakeSink records submissions and can fail on a given call (1-based).
type fakeSink struct {
	ideas    []Idea
	failOn   int
	failWith error
	onSubmit func()
}

func (s *fakeSink) Submit(_ context.Context, idea Idea) error {
	if s.onSubmit != nil {
		s.onSubmit()
	}
	if s.failOn == len(s.ideas)+1 {
		return s.failWith
	}
	s.ideas = append(s.ideas, idea)
	return nil
}

var testNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func threeRecordings(t *testing.T) *fakeTools {
	t.Helper()
	return &fakeTools{
		pages: []string{filesJSON(t,
			map[string]any{"id": "r3", "name": "Third", "start_at": "2026-11-11T08:00:00Z"},
			map[string]any{"id": "r1", "name": "First", "start_at": "2026-11-11T06:00:00Z"},
			map[string]any{"id": "r2", "name": "Second", "start_at": "2026-11-11T07:00:00Z"},
		)},
		notes:       map[string]string{"r1": `[{"data_content":"n1"}]`, "r2": `[{"data_content":"n2"}]`, "r3": `[{"data_content":"n3"}]`},
		transcripts: map[string]string{"r1": `{"segments":[{"content":"t1"}]}`, "r2": `{"segments":[{"content":"t2"}]}`, "r3": `{"segments":[{"content":"t3"}]}`},
	}
}

func newIngester(t *testing.T, tools Tools, sink Sink) *Ingester {
	t.Helper()
	return &Ingester{
		Dial:       func(context.Context) (Tools, error) { return tools, nil },
		Sink:       sink,
		CursorPath: filepath.Join(t.TempDir(), "plaud-cursor.json"),
		Tags:       []string{"plaud"},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return testNow },
	}
}

func titles(ideas []Idea) string {
	var out []string
	for _, i := range ideas {
		out = append(out, i.Title)
	}
	return strings.Join(out, ",")
}

func TestRunOnceSubmitsOldestFirstAndDedupes(t *testing.T) {
	tools := threeRecordings(t)
	sink := &fakeSink{}
	in := newIngester(t, tools, sink)
	res, err := in.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Submitted != 3 || titles(sink.ideas) != "First,Second,Third" || !tools.closed {
		t.Fatalf("res=%+v titles=%q closed=%v", res, titles(sink.ideas), tools.closed)
	}
	if got := tools.listArgs[0]["date_from"]; got != "2026-11-04" {
		t.Fatalf("first run date_from = %v, want DefaultSinceDays back", got)
	}
	cur, err := LoadCursor(in.CursorPath)
	if err != nil || len(cur.Seen) != 3 || !cur.Newest.Equal(time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("cursor = %+v, %v", cur, err)
	}

	tools.listArgs = nil
	res, err = in.RunOnce(context.Background())
	if err != nil || res.Submitted != 0 || len(sink.ideas) != 3 {
		t.Fatalf("second run: res=%+v err=%v ideas=%d; want no resubmission", res, err, len(sink.ideas))
	}
	if got := tools.listArgs[0]["date_from"]; got != "2026-11-10" {
		t.Fatalf("second run date_from = %v, want newest minus overlap", got)
	}
}

func TestRunOnceSinceDays(t *testing.T) {
	tools := &fakeTools{}
	in := newIngester(t, tools, &fakeSink{})
	in.SinceDays = 1
	if _, err := in.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := tools.listArgs[0]["date_from"]; got != "2026-11-10" {
		t.Fatalf("date_from = %v", got)
	}
}

func TestRunOnceSkipsRecordingsWithoutContent(t *testing.T) {
	tools := threeRecordings(t)
	delete(tools.notes, "r1")
	delete(tools.transcripts, "r1")
	tools.notes["r2"] = ""
	tools.transcripts["r2"] = ""
	sink := &fakeSink{}
	in := newIngester(t, tools, sink)
	res, err := in.RunOnce(context.Background())
	if err != nil || res.Submitted != 1 || res.Skipped != 2 || titles(sink.ideas) != "Third" {
		t.Fatalf("res=%+v err=%v titles=%q", res, err, titles(sink.ideas))
	}
	cur, _ := LoadCursor(in.CursorPath)
	if _, ok := cur.Seen["r1"]; ok {
		t.Fatal("a skipped recording was marked seen; it must be retried")
	}
}

func TestRunOnceStopsAtLocalBudget(t *testing.T) {
	sink := &fakeSink{}
	in := newIngester(t, threeRecordings(t), sink)
	in.MaxPerHour = 2
	res, err := in.RunOnce(context.Background())
	if err != nil || res.Submitted != 2 || !res.BudgetExhausted || res.RateLimited {
		t.Fatalf("res=%+v err=%v; want 2 submitted then budget stop", res, err)
	}
}

func TestRunOnceStopsOn429(t *testing.T) {
	sink := &fakeSink{failOn: 2, failWith: ErrRateLimited}
	in := newIngester(t, threeRecordings(t), sink)
	res, err := in.RunOnce(context.Background())
	if err != nil || res.Submitted != 1 || !res.RateLimited || res.BudgetExhausted {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	cur, _ := LoadCursor(in.CursorPath)
	if _, ok := cur.Seen["r2"]; ok || len(cur.Seen) != 1 {
		t.Fatalf("seen = %v; the rate-limited recording must be retried", cur.Seen)
	}
}

func TestRunOnceErrors(t *testing.T) {
	boom := errors.New("boom")
	sink := &fakeSink{failOn: 1, failWith: boom}
	if _, err := newIngester(t, threeRecordings(t), sink).RunOnce(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("sink error = %v", err)
	}

	in := newIngester(t, nil, &fakeSink{})
	in.Dial = func(context.Context) (Tools, error) { return nil, boom }
	if _, err := in.RunOnce(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("dial error = %v", err)
	}

	in = newIngester(t, &fakeTools{listErr: boom}, &fakeSink{})
	if _, err := in.RunOnce(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("list error = %v", err)
	}

	in = newIngester(t, &fakeTools{}, &fakeSink{})
	in.CursorPath = t.TempDir()
	if _, err := in.RunOnce(context.Background()); err == nil {
		t.Fatal("unreadable cursor: want error")
	}

	in = newIngester(t, threeRecordings(t), nil)
	in.Sink = &fakeSink{onSubmit: func() { _ = os.MkdirAll(in.CursorPath, 0o700) }}
	if _, err := in.RunOnce(context.Background()); err == nil {
		t.Fatal("cursor save failure: want error")
	}
}

func TestRunOnceDefaults(t *testing.T) {
	in := newIngester(t, &fakeTools{}, &fakeSink{})
	in.Now, in.Log = nil, nil
	if in.now().IsZero() || in.log() == nil {
		t.Fatal("defaults not applied")
	}
	if _, err := in.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunOnceMode(t *testing.T) {
	in := newIngester(t, &fakeTools{}, &fakeSink{})
	if err := in.Run(context.Background(), 0); err != nil {
		t.Fatalf("Run once: %v", err)
	}
	boom := errors.New("boom")
	in.Dial = func(context.Context) (Tools, error) { return nil, boom }
	if err := in.Run(context.Background(), 0); !errors.Is(err, boom) {
		t.Fatalf("Run once error = %v", err)
	}
}

func TestRunLoopRetriesUntilCancelled(t *testing.T) {
	var buf bytes.Buffer
	in := newIngester(t, nil, &fakeSink{})
	in.Log = slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	in.Dial = func(context.Context) (Tools, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("plaud down")
		}
		cancel()
		return &fakeTools{}, nil
	}
	if err := in.Run(ctx, time.Millisecond); err != nil {
		t.Fatalf("Run loop: %v", err)
	}
	if calls != 2 || !strings.Contains(buf.String(), "plaud down") || !strings.Contains(buf.String(), "plaud ingest run done") {
		t.Fatalf("calls=%d log=%s", calls, buf.String())
	}
}

func TestDibsSinkSubmitsPrivateDraft(t *testing.T) {
	srv, st := dibsServer(t)
	sink := &DibsSink{Endpoint: srv.URL + "/mcp", Token: "hub-token"}
	if err := sink.Submit(context.Background(), Idea{Title: "Voice idea", Description: "From Plaud", Tags: []string{"plaud", "voice-note"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := sink.Submit(context.Background(), Idea{Title: "No tags", Description: "Body"}); err != nil {
		t.Fatalf("Submit without tags: %v", err)
	}
	ideas, err := st.ListByAuthor("alice")
	if err != nil || len(ideas) != 2 {
		t.Fatalf("ideas = %d, %v", len(ideas), err)
	}
	for _, idea := range ideas {
		if idea.Status != store.StatusDraft || idea.Visibility != store.VisibilityPrivate {
			t.Fatalf("idea %q status=%s visibility=%s; want private draft", idea.Title, idea.Status, idea.Visibility)
		}
		if idea.Title == "Voice idea" && strings.Join(idea.Tags, ",") != "plaud,voice-note" {
			t.Fatalf("tags = %v", idea.Tags)
		}
	}
}

func TestDibsSinkRejectsBadToken(t *testing.T) {
	srv, _ := dibsServer(t)
	err := (&DibsSink{Endpoint: srv.URL + "/mcp", Token: "bad-token-xyz"}).Submit(context.Background(), Idea{Title: "T", Description: "D"})
	if err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("bad token: err = %v", err)
	}
	if strings.Contains(err.Error(), "bad-token-xyz") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestDibsSinkReports429(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	err := (&DibsSink{Endpoint: srv.URL, Token: "hub-token"}).Submit(context.Background(), Idea{Title: "T", Description: "D"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if auth != "Bearer hub-token" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestDibsSinkOtherHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := (&DibsSink{Endpoint: srv.URL, Token: "t", Transport: http.DefaultTransport}).Submit(context.Background(), Idea{Title: "T", Description: "D"})
	if err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want a non-rate-limit error", err)
	}
}

// rateLimitAfterInit lets the handshake through, then answers 429.
type rateLimitAfterInit struct {
	next http.Handler
}

func (h *rateLimitAfterInit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if bytes.Contains(body, []byte(`"tools/call"`)) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	h.next.ServeHTTP(w, r)
}

func TestDibsSinkReports429OnToolCall(t *testing.T) {
	srv, _ := dibsServer(t)
	proxy := httptest.NewServer(&rateLimitAfterInit{next: srv.Config.Handler})
	defer proxy.Close()
	err := (&DibsSink{Endpoint: proxy.URL + "/mcp", Token: "hub-token"}).Submit(context.Background(), Idea{Title: "T", Description: "D"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}
