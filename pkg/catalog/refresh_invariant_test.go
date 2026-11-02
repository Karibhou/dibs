package catalog

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

const alphaOnlyLandscapeYAML = `
landscape:
  - name: Orchestration
    subcategories:
      - name: Scheduling
        items:
          - name: alpha
            description: landscape says alpha
            repo_url: https://github.com/cncf/alpha
            project: graduated
`

// TestRefreshNeverDegradesCatalog is the gate for Refresh: a degraded fetch
// must never overwrite a populated catalog, and FetchedAt only advances for
// projects that were actually enriched. Extend the table when changing Refresh.
func TestRefreshNeverDegradesCatalog(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	alphaAt := now.Add(-4 * 24 * time.Hour) // oldest, so enriched first
	betaAt := now.Add(-3 * 24 * time.Hour)

	githubOK := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/cncf/alpha":
			_ = json.NewEncoder(w).Encode(map[string]any{"description": "fresh alpha", "language": "Go"})
		case "/repos/cncf/alpha/readme":
			_, _ = w.Write([]byte("Fresh alpha readme.\n"))
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}
	serverError := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }

	tests := []struct {
		name      string
		landscape string
		github    http.HandlerFunc
		wantErr   error
		wantIDs   []string
		// wantStamp is the expected FetchedAt per surviving project.
		wantStamp map[string]time.Time
		// wantKept lists projects whose prior enrichment must survive.
		wantKept []string
	}{
		{
			name:      "empty landscape body",
			landscape: "",
			github:    serverError,
			wantErr:   errEmptyLandscape,
			wantIDs:   []string{"cncf/alpha", "cncf/beta"},
			wantStamp: map[string]time.Time{"cncf/alpha": alphaAt, "cncf/beta": betaAt},
			wantKept:  []string{"cncf/alpha", "cncf/beta"},
		},
		{
			name:      "renamed upstream keys parse to nothing",
			landscape: "categories:\n  - name: x\n",
			github:    serverError,
			wantErr:   errEmptyLandscape,
			wantIDs:   []string{"cncf/alpha", "cncf/beta"},
			wantStamp: map[string]time.Time{"cncf/alpha": alphaAt, "cncf/beta": betaAt},
			wantKept:  []string{"cncf/alpha", "cncf/beta"},
		},
		{
			name:      "subset churn drops removed projects and keeps survivor enrichment",
			landscape: alphaOnlyLandscapeYAML,
			github:    serverError,
			wantIDs:   []string{"cncf/alpha"},
			wantStamp: map[string]time.Time{"cncf/alpha": alphaAt},
			wantKept:  []string{"cncf/alpha"},
		},
		{
			name:      "every enrich call fails",
			landscape: refreshLandscapeYAML,
			github:    serverError,
			wantIDs:   []string{"cncf/alpha", "cncf/beta"},
			wantStamp: map[string]time.Time{"cncf/alpha": alphaAt, "cncf/beta": betaAt},
			wantKept:  []string{"cncf/alpha", "cncf/beta"},
		},
		{
			name:      "rate limited after the first project",
			landscape: refreshLandscapeYAML,
			github:    githubOK,
			wantIDs:   []string{"cncf/alpha", "cncf/beta"},
			wantStamp: map[string]time.Time{"cncf/alpha": now, "cncf/beta": betaAt},
			wantKept:  []string{"cncf/beta"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gh := httptest.NewServer(tc.github)
			defer gh.Close()
			s := newRefreshStore(t, gh, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.landscape))
			})
			s.Now = func() time.Time { return now }
			s.setProjects([]Project{
				{Name: "alpha", RepoID: "cncf/alpha", Description: "cached alpha", Topics: []string{"a"}, Language: "Rust", Readme: "cached alpha readme", FetchedAt: alphaAt},
				{Name: "beta", RepoID: "cncf/beta", Description: "cached beta", Topics: []string{"b"}, Language: "Rust", Readme: "cached beta readme", FetchedAt: betaAt},
			})

			err := s.Refresh(t.Context())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Refresh error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if _, statErr := os.Stat(s.path); !os.IsNotExist(statErr) {
					t.Fatalf("cache file written despite failed refresh (stat err %v)", statErr)
				}
			}
			got := map[string]Project{}
			for _, p := range s.List() {
				got[p.RepoID] = p
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("catalog has %d projects, want %v", len(got), tc.wantIDs)
			}
			for _, id := range tc.wantIDs {
				p, ok := got[id]
				if !ok {
					t.Fatalf("project %s missing after Refresh", id)
				}
				if !p.FetchedAt.Equal(tc.wantStamp[id]) {
					t.Errorf("%s FetchedAt = %v, want %v", id, p.FetchedAt, tc.wantStamp[id])
				}
			}
			for _, id := range tc.wantKept {
				p := got[id]
				if p.Readme == "" || p.Language != "Rust" || len(p.Topics) != 1 || p.Description == "" {
					t.Errorf("%s lost prior enrichment: %+v", id, p)
				}
			}
		})
	}
}
