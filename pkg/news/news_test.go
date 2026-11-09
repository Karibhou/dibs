package news

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/dibs/pkg/history"
	"github.com/hivecommons/dibs/pkg/match"
)

type fakeFetcher struct {
	prs   []history.MergedPullRequest
	calls int
}

func (f *fakeFetcher) FetchMergedPullRequests(context.Context, string) ([]history.MergedPullRequest, error) {
	f.calls++
	return append([]history.MergedPullRequest(nil), f.prs...), nil
}

type truncatedFetcher struct {
	fakeFetcher
	coveredSince time.Time
}

func (f *truncatedFetcher) FetchMergedPullRequestsSince(ctx context.Context, repoID string) ([]history.MergedPullRequest, time.Time, error) {
	prs, err := f.FetchMergedPullRequests(ctx, repoID)
	return prs, f.coveredSince, err
}

func newsNow() time.Time {
	return time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
}

func pr(title, at, author string) history.MergedPullRequest {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic(err)
	}
	return history.MergedPullRequest{Title: title, MergedAt: t, Author: author}
}

func TestFallbackTLDRFormatting(t *testing.T) {
	got := FallbackTLDR([]history.MergedPullRequest{
		{Title: "add quiet news cards"},
		{Title: "backfill merged pull request titles"},
		{Title: "tighten market chart spacing"},
		{Title: "wire cache invalidation"},
		{Title: "document API shape"},
	})
	if !strings.HasPrefix(got, "Merged add quiet news cards; backfill merged pull request titles") {
		t.Fatalf("fallback prefix = %q", got)
	}
	if !strings.Contains(got, "and 1 more") || !strings.HasSuffix(got, ".") {
		t.Fatalf("fallback should mention extra PRs and end with period: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("fallback must be one line: %q", got)
	}
}

func TestFallbackTLDRTruncationWithinCap(t *testing.T) {
	// Four 90-rune titles joined with "; " and prefixed with "Merged " exceeds 280 runes.
	// When truncated, result must stay within 280 runes and not end with "….".
	longTitle := strings.Repeat("a", 90)
	got := FallbackTLDR([]history.MergedPullRequest{
		{Title: longTitle},
		{Title: longTitle},
		{Title: longTitle},
		{Title: longTitle},
	})
	runeCount := utf8.RuneCountInString(got)
	if runeCount > 280 {
		t.Fatalf("truncated TLDR is %d runes, exceeds 280-rune cap", runeCount)
	}
	if strings.HasSuffix(got, "….") {
		t.Fatalf("truncated TLDR ends with '….' (ellipsis + period): %q", got)
	}
	if runeCount != 280 {
		t.Fatalf("truncated TLDR is %d runes, expected exactly 280", runeCount)
	}
}

func TestRefreshReusesCacheUntilDayFingerprintChanges(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ff := &fakeFetcher{prs: []history.MergedPullRequest{
		pr("first merge", "2026-08-19T10:00:00Z", "alice"),
	}}
	g := &Generator{Store: st, Fetcher: ff, Now: newsNow}
	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	first := st.Get("org/repo")
	if len(first) != 1 || first[0].TLDR != "Merged first merge." || first[0].Source != "digest" {
		t.Fatalf("first items = %+v", first)
	}

	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	second := st.Get("org/repo")
	if len(second) != 1 || second[0].TLDR != first[0].TLDR {
		t.Fatalf("same fingerprint should reuse cache: first=%+v second=%+v", first, second)
	}

	ff.prs = append(ff.prs, pr("second merge", "2026-08-19T11:00:00Z", "bob"))
	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("third Refresh: %v", err)
	}
	third := st.Get("org/repo")
	if len(third) != 1 || third[0].PRCount != 2 || !strings.Contains(third[0].TLDR, "second merge") {
		t.Fatalf("changed fingerprint should regenerate day: %+v", third)
	}
}

func TestRefreshLLMPath(t *testing.T) {
	var userPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				userPrompt = msg.Content
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "Auth and chart fixes shipped; cadence is rising across UI work."}}},
		})
	}))
	defer srv.Close()

	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	g := &Generator{
		Store: st,
		Fetcher: &fakeFetcher{prs: []history.MergedPullRequest{
			pr("fix auth refresh", "2026-08-19T10:00:00Z", "alice"),
			pr("add chart cards", "2026-08-19T11:00:00Z", "bob"),
		}},
		LLM: &match.LLM{BaseURL: srv.URL, Model: "test", Client: srv.Client()},
		Now: newsNow,
	}
	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	items := st.Get("org/repo")
	if len(items) != 1 || items[0].Source != "llm" || items[0].PRCount != 2 {
		t.Fatalf("items = %+v", items)
	}
	if !strings.Contains(userPrompt, "source PR count: 2") || !strings.Contains(userPrompt, "fix auth refresh") {
		t.Fatalf("prompt missing PR context:\n%s", userPrompt)
	}
}

func TestNewStoreErrors(t *testing.T) {
	t.Run("data dir path is an existing file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore(file); err == nil || !strings.Contains(err.Error(), "creating data dir") {
			t.Fatalf("err = %v, want creating data dir error", err)
		}
	})

	t.Run("repo-news.json unreadable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "repo-news.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore(dir); err == nil || !strings.Contains(err.Error(), "reading repo-news.json") {
			t.Fatalf("err = %v, want reading error", err)
		}
	})

	t.Run("repo-news.json corrupt", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "repo-news.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewStore(dir); err == nil || !strings.Contains(err.Error(), "corrupt repo-news.json") {
			t.Fatalf("err = %v, want corrupt error", err)
		}
	})
}

func TestNewStoreLoadsAndNormalizesExistingFile(t *testing.T) {
	dir := t.TempDir()
	repos := []repoNews{{
		RepoID: "org/repo",
		Items: []cachedItem{
			{Item: Item{Date: "2026-08-01", TLDR: "older", PRCount: 1, Source: "llm"}},
			{Item: Item{Date: "", TLDR: "no date", PRCount: 1}},        // dropped
			{Item: Item{Date: "2026-08-03", TLDR: " ", PRCount: 1}},    // blank TLDR dropped
			{Item: Item{Date: "2026-08-02", TLDR: "zero", PRCount: 0}}, // non-positive dropped
			{Item: Item{Date: "2026-08-04", TLDR: "newest", PRCount: 2, Source: "llm"}},
		},
	}}
	raw, err := json.Marshal(repos)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo-news.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	items := st.Get("org/repo")
	if len(items) != 2 {
		t.Fatalf("Get = %+v", items)
	}
	if items[0].Date != "2026-08-04" || items[0].TLDR != "newest" || items[1].Date != "2026-08-01" {
		t.Fatalf("items not newest-first/filtered: %+v", items)
	}
}

func TestUpsertRollsBackWhenPersistFails(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item := func(fp string) []cachedItem {
		// normalizeRepoNews drops items without PRCount/TLDR, so the fixture must pass that filter.
		return []cachedItem{{Item: Item{Date: "2026-08-18", PRCount: 1, TLDR: "x"}, Fingerprint: fp}}
	}
	if err := s.upsert("org/old", item("a"), newsNow()); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(t.TempDir(), "missing", "repo-news.json")

	if err := s.upsert("org/old", item("b"), newsNow()); err == nil {
		t.Fatal("expected persist error")
	}
	if err := s.upsert("org/new", item("c"), newsNow()); err == nil {
		t.Fatal("expected persist error")
	}
	if got := s.getCached("org/old")["2026-08-18"].Fingerprint; got != "a" {
		t.Fatalf("fingerprint = %q, want a", got)
	}
	if got := s.Get("org/new"); len(got) != 0 {
		t.Fatalf("new entry served after failed persist: %+v", got)
	}
}

func TestRefreshRetriesFallbackCardAfterLLMRecovers(t *testing.T) {
	const tldr = "Auth and chart fixes shipped; cadence is rising across UI work."
	var healthy atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !healthy.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": tldr}}},
		})
	}))
	defer srv.Close()

	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	g := &Generator{
		Store: st,
		Fetcher: &fakeFetcher{prs: []history.MergedPullRequest{
			pr("fix auth refresh", "2026-08-19T10:00:00Z", "alice"),
			pr("add chart cards", "2026-08-19T11:00:00Z", "bob"),
		}},
		LLM: &match.LLM{BaseURL: srv.URL, Model: "test", Client: srv.Client()},
		Now: newsNow,
	}
	ctx := context.Background()
	if err := g.Refresh(ctx, "org/repo"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if items := st.Get("org/repo"); len(items) != 1 || items[0].Source != "digest" {
		t.Fatalf("after failure items = %+v", items)
	}

	healthy.Store(true)
	if err := g.Refresh(ctx, "org/repo"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	items := st.Get("org/repo")
	if len(items) != 1 || items[0].Source != "llm" || items[0].TLDR != tldr {
		t.Fatalf("after recovery items = %+v", items)
	}

	before := hits.Load()
	if err := g.Refresh(ctx, "org/repo"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := hits.Load(); got != before {
		t.Fatalf("llm card regenerated: hits %d -> %d", before, got)
	}
	if items := st.Get("org/repo"); len(items) != 1 || items[0].Source != "llm" {
		t.Fatalf("after reuse items = %+v", items)
	}
}

func TestRefreshKeepsCachedCardsBeforeTruncationBoundary(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	tf := &truncatedFetcher{}
	tf.prs = []history.MergedPullRequest{
		pr("old merge", "2026-08-12T10:00:00Z", "alice"),
		pr("boundary merge", "2026-08-17T10:00:00Z", "alice"),
		pr("new merge", "2026-08-19T10:00:00Z", "alice"),
	}
	g := &Generator{Store: st, Fetcher: tf, Now: newsNow}
	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	tf.coveredSince = time.Date(2026, 8, 17, 6, 0, 0, 0, time.UTC)
	tf.prs = []history.MergedPullRequest{
		pr("boundary merge", "2026-08-17T10:00:00Z", "alice"),
		pr("boundary extra", "2026-08-17T11:00:00Z", "bob"),
		pr("new merge", "2026-08-19T10:00:00Z", "alice"),
	}
	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	byDate := map[string]Item{}
	for _, it := range st.Get("org/repo") {
		byDate[it.Date] = it
	}
	if _, ok := byDate["2026-08-12"]; !ok {
		t.Fatalf("card before boundary was dropped: %+v", byDate)
	}
	if it := byDate["2026-08-17"]; it.PRCount != 1 {
		t.Fatalf("boundary card re-digested from partial data: %+v", it)
	}
	if _, ok := byDate["2026-08-19"]; !ok {
		t.Fatalf("covered card missing: %+v", byDate)
	}
}

func TestRefreshCarriesCachedBoundaryCardWhenNoBoundaryMergeFetched(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	tf := &truncatedFetcher{}
	tf.prs = []history.MergedPullRequest{
		pr("boundary merge", "2026-08-17T10:00:00Z", "alice"),
		pr("new merge", "2026-08-19T10:00:00Z", "alice"),
	}
	g := &Generator{Store: st, Fetcher: tf, Now: newsNow}
	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	// The boundary PR was closed-unmerged, so the truncated fetch yields no
	// merged PR on the boundary day at all.
	tf.coveredSince = time.Date(2026, 8, 17, 6, 0, 0, 0, time.UTC)
	tf.prs = []history.MergedPullRequest{
		pr("new merge", "2026-08-19T10:00:00Z", "alice"),
	}
	if err := g.Refresh(context.Background(), "org/repo"); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	items := st.Get("org/repo")
	byDate := map[string]int{}
	for _, it := range items {
		byDate[it.Date]++
	}
	if byDate["2026-08-17"] != 1 {
		t.Fatalf("cached boundary card count = %d, want 1: %+v", byDate["2026-08-17"], items)
	}
	if byDate["2026-08-19"] != 1 {
		t.Fatalf("covered card count = %d, want 1: %+v", byDate["2026-08-19"], items)
	}
}
