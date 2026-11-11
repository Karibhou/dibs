package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRefreshRateLimitKeepsPriorEnrichmentAndStaleStamps(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-3 * 24 * time.Hour)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/cncf/alpha":
			_ = json.NewEncoder(w).Encode(map[string]any{"description": "fresh alpha", "language": "Go"})
		case "/repos/cncf/alpha/readme":
			_, _ = w.Write([]byte("Alpha readme.\n"))
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer gh.Close()

	s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rateLimitLandscapeYAML))
	})
	s.Now = func() time.Time { return now }
	s.setProjects([]Project{{
		Name: "beta", RepoID: "cncf/beta", Description: "cached beta",
		Topics: []string{"cached"}, Language: "Rust", Readme: "cached readme", FetchedAt: cachedAt,
	}})

	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := map[string]Project{}
	for _, p := range s.List() {
		got[p.RepoID] = p
	}
	if a := got["cncf/alpha"]; a.Readme == "" || !a.FetchedAt.Equal(now) {
		t.Fatalf("alpha = %+v, want enriched and stamped now", a)
	}
	b := got["cncf/beta"]
	if b.Readme != "cached readme" || b.Language != "Rust" || len(b.Topics) != 1 || b.Description != "cached beta" {
		t.Fatalf("beta lost prior enrichment: %+v", b)
	}
	if !b.FetchedAt.Equal(cachedAt) {
		t.Fatalf("beta FetchedAt = %v, want cached %v", b.FetchedAt, cachedAt)
	}
	g := got["cncf/gamma"]
	if !g.FetchedAt.IsZero() || g.Readme != "" {
		t.Fatalf("gamma (never enriched) = %+v, want zero FetchedAt and empty readme", g)
	}
	if !s.needsRefresh() {
		t.Fatal("needsRefresh() = false with unenriched projects, want true")
	}
}

func TestNeedsRefreshAnyZeroFetchedAt(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	s, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Now = func() time.Time { return now }
	s.setProjects([]Project{
		{RepoID: "a/a", FetchedAt: now.Add(-time.Hour)},
		{RepoID: "b/b"},
	})
	if !s.needsRefresh() {
		t.Fatal("needsRefresh() = false with a zero FetchedAt project, want true")
	}
}

func TestRefreshReadmeRateLimitKeepsCachedReadmeAndStamp(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-3 * 24 * time.Hour)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/cncf/beta":
			_ = json.NewEncoder(w).Encode(map[string]any{"description": "fresh beta", "language": "Go"})
		default:
			// The readme call is the one that trips the limiter.
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer gh.Close()

	s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rateLimitLandscapeYAML))
	})
	s.Now = func() time.Time { return now }
	s.setProjects([]Project{{
		Name: "beta", RepoID: "cncf/beta", Description: "cached beta",
		Topics: []string{"cached"}, Language: "Rust", Readme: "cached readme", FetchedAt: cachedAt,
	}})

	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	var b Project
	for _, p := range s.List() {
		if p.RepoID == "cncf/beta" {
			b = p
		}
	}
	if b.Readme != "cached readme" {
		t.Fatalf("beta readme = %q, want cached readme kept after readme rate limit", b.Readme)
	}
	if !b.FetchedAt.Equal(cachedAt) {
		t.Fatalf("beta FetchedAt = %v, want stale %v so it is retried", b.FetchedAt, cachedAt)
	}
	if !s.limiter.WaitAt(now) {
		t.Fatal("limiter not tripped by readme rate limit")
	}
}

func TestRefreshAsyncSkipsWhileRateLimited(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer gh.Close()

	calls := make(chan string, 8)
	s := newRefreshStore(t, gh, func(w http.ResponseWriter, r *http.Request) {
		calls <- r.URL.Path
		_, _ = w.Write([]byte(rateLimitLandscapeYAML))
	})
	s.Now = func() time.Time { return now }
	refreshed := make(chan error, 1)
	s.OnRefresh = func(err error) { refreshed <- err }

	// First pass downloads the landscape and trips the limiter.
	s.RefreshAsync()
	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatalf("OnRefresh err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first RefreshAsync never completed")
	}
	<-calls
	if !s.limiter.WaitAt(now) {
		t.Fatal("limiter not tripped by first pass")
	}
	if !s.needsRefresh() {
		t.Fatal("needsRefresh() = false with unenriched projects, want true")
	}

	// Subsequent ticks while rate-limited must not refetch or rewrite.
	s.RefreshAsync()
	select {
	case p := <-calls:
		t.Fatalf("RefreshAsync fetched %s while rate-limited", p)
	case err := <-refreshed:
		t.Fatalf("OnRefresh(%v) called while rate-limited", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Once the back-off expires, the retry resumes.
	s.Now = func() time.Time { return now.Add(24 * time.Hour) }
	s.RefreshAsync()
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("RefreshAsync did not resume after the rate limit expired")
	}
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed refresh never completed")
	}
}

func TestRefreshEnrichesNeverFetchedAndOldestFirst(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	var (
		mu      sync.Mutex
		visited []string
	)
	// GitHub has quota for two projects this pass; everything after is 429.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/readme")
		mu.Lock()
		if !slices.Contains(visited, repoID) {
			visited = append(visited, repoID)
		}
		over := len(visited) > 2
		mu.Unlock()
		switch {
		case over:
			w.WriteHeader(http.StatusTooManyRequests)
		case strings.HasSuffix(r.URL.Path, "/readme"):
			_, _ = w.Write([]byte("fresh readme for " + repoID + "\n"))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"description": "fresh " + repoID, "language": "Go"})
		}
	}))
	defer gh.Close()

	s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rateLimitLandscapeYAML))
	})
	s.Now = func() time.Time { return now }
	// Landscape order is alpha, beta, gamma; staleness order is the reverse of
	// what alpha's position suggests: beta was never enriched, gamma is three
	// days old and alpha is the freshest.
	alphaAt, gammaAt := now.Add(-time.Hour), now.Add(-3*24*time.Hour)
	s.setProjects([]Project{
		{Name: "alpha", RepoID: "cncf/alpha", Description: "cached alpha", Readme: "cached alpha readme", FetchedAt: alphaAt},
		{Name: "beta", RepoID: "cncf/beta"},
		{Name: "gamma", RepoID: "cncf/gamma", Description: "cached gamma", Readme: "cached gamma readme", FetchedAt: gammaAt},
	})

	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	mu.Lock()
	order := append([]string(nil), visited...)
	mu.Unlock()
	if want := []string{"cncf/beta", "cncf/gamma", "cncf/alpha"}; !slices.Equal(order, want) {
		t.Fatalf("enrichment order = %v, want never-fetched then oldest first %v", order, want)
	}
	got := map[string]Project{}
	for _, p := range s.List() {
		got[p.RepoID] = p
	}
	for _, id := range []string{"cncf/beta", "cncf/gamma"} {
		// readmeIntro trims the trailing newline the fake GitHub server writes.
		if p := got[id]; p.Readme != "fresh readme for "+id || !p.FetchedAt.Equal(now) {
			t.Fatalf("%s = %+v, want enriched this pass and stamped now", id, p)
		}
	}
	if a := got["cncf/alpha"]; a.Readme != "cached alpha readme" || !a.FetchedAt.Equal(alphaAt) {
		t.Fatalf("alpha = %+v, want prior enrichment and stamp carried forward", a)
	}
	ids := make([]string, 0, 3)
	for _, p := range s.List() {
		ids = append(ids, p.RepoID)
	}
	if want := []string{"cncf/alpha", "cncf/beta", "cncf/gamma"}; !slices.Equal(ids, want) {
		t.Fatalf("persisted order = %v, want sorted by RepoID %v", ids, want)
	}
}

func TestRefreshEnrichFailureKeepsPriorEnrichmentAndStaleStamp(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-3 * 24 * time.Hour)
	cases := map[string]http.HandlerFunc{
		"repo 500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
		"readme 500": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/readme") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"description": "fresh beta", "language": "Go"})
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			gh := httptest.NewServer(handler)
			defer gh.Close()
			s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(rateLimitLandscapeYAML))
			})
			s.Now = func() time.Time { return now }
			s.setProjects([]Project{{
				Name: "beta", RepoID: "cncf/beta", Description: "cached beta",
				Topics: []string{"cached"}, Language: "Rust", Readme: "cached readme", FetchedAt: cachedAt,
			}})
			if err := s.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			var b Project
			for _, p := range s.List() {
				if p.RepoID == "cncf/beta" {
					b = p
				}
			}
			if b.Readme != "cached readme" || b.Language != "Rust" || len(b.Topics) != 1 || b.Description != "cached beta" {
				t.Fatalf("beta lost prior enrichment: %+v", b)
			}
			if !b.FetchedAt.Equal(cachedAt) {
				t.Fatalf("beta FetchedAt = %v, want stale %v", b.FetchedAt, cachedAt)
			}
			if !s.needsRefresh() {
				t.Fatal("needsRefresh() = false after enrich failure, want true")
			}
		})
	}
}

func TestRefreshPermanentClientErrorStampsButServerErrorDoesNot(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		status    int
		wantStamp bool
	}{
		"403 no rate-limit headers": {http.StatusForbidden, true},
		"451":                       {http.StatusUnavailableForLegalReasons, true},
		"500":                       {http.StatusInternalServerError, false},
	} {
		t.Run(name, func(t *testing.T) {
			gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer gh.Close()
			s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(rateLimitLandscapeYAML))
			})
			s.Now = func() time.Time { return now }
			if err := s.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if got := s.needsRefresh(); got == tc.wantStamp {
				t.Fatalf("needsRefresh() = %v, want %v", got, !tc.wantStamp)
			}
		})
	}
}

func TestRefreshSkipsEnrichAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	calls := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		cancel() // the deadline expires during the first project
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer gh.Close()
	s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rateLimitLandscapeYAML))
	})
	logs := 0
	s.Logf = func(string, ...any) { logs++ }
	if err := s.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if calls != 1 || logs > 1 {
		t.Fatalf("enrich calls = %d, log lines = %d, want 1 call and at most 1 line after the deadline", calls, logs)
	}
	if got := len(s.List()); got < 2 {
		t.Fatalf("List() = %d projects, want all persisted", got)
	}
}

func TestRefreshCarryForwardMatchesRepoIDCaseInsensitively(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	cachedAt := now.Add(-3 * 24 * time.Hour)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer gh.Close()

	s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rateLimitLandscapeYAML))
	})
	s.Now = func() time.Time { return now }
	s.setProjects([]Project{{
		Name: "beta", RepoID: "CNCF/Beta", Description: "cached beta",
		Topics: []string{"cached"}, Language: "Rust", Readme: "cached readme", FetchedAt: cachedAt,
	}})

	if err := s.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	for _, p := range s.List() {
		if p.RepoID != "cncf/beta" {
			continue
		}
		if p.Readme != "cached readme" || p.Description != "cached beta" || len(p.Topics) != 1 || !p.FetchedAt.Equal(cachedAt) {
			t.Fatalf("beta lost prior enrichment across case-differing RepoID: %+v", p)
		}
		return
	}
	t.Fatal("cncf/beta missing after Refresh")
}
