package match

import (
	"context"
	"fmt"
	"testing"

	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/store"
)

// TestPersistRematchResultsKeepsCachedMatches: a persisted rematch carrying
// fewer matches than the cache merges over it instead of evicting the rest,
// so the next read re-scores and re-notifies nothing.
func TestPersistRematchResultsKeepsCachedMatches(t *testing.T) {
	st, reg := newFixtures(t)
	const n = 6
	repos := make([]registry.RepoProfile, 0, n)
	for i := 0; i < n; i++ {
		repos = append(repos, registry.RepoProfile{
			RepoID:      fmt.Sprintf("org/repo-%02d", i),
			Owner:       "bob",
			Description: "kubernetes multicluster scheduling placement",
			Topics:      []string{"scheduling"},
		})
	}
	if err := reg.Merge(repos); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	srv, calls := fakeGateway(t, func(string) string { return `{"score": 80, "reason": "fits"}` })
	notes := &countingNotifier{}
	e := &Engine{Store: st, Registry: reg, Notifier: notes, LLM: &LLM{BaseURL: srv.URL + "/v1", Model: "test"}}
	idea := createIdea(t, st, "Kubernetes multicluster scheduling", "Schedule workloads across clusters with placement policies.")

	if _, err := e.MatchesForIdea(context.Background(), idea); err != nil {
		t.Fatalf("MatchesForIdea: %v", err)
	}
	cached, err := st.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(cached.Matches) != n {
		t.Fatalf("cached %d matches, want %d", len(cached.Matches), n)
	}
	for _, m := range cached.Matches {
		if m.Score < NotifyThreshold {
			t.Fatalf("%s scored %v, want >= %d", m.RepoID, m.Score, NotifyThreshold)
		}
	}
	scored, notified := calls.Load(), notes.n

	rp, err := reg.Get("org/repo-00")
	if err != nil {
		t.Fatalf("Get repo: %v", err)
	}
	top := []store.Match{{RepoID: rp.RepoID, RepoHash: RepoHash(rp), Score: 95, ByLLM: true}}
	if err := e.PersistRematchResults(cached.ID, cached.UpdatedAt, "tldr", top, nil, ""); err != nil {
		t.Fatalf("PersistRematchResults: %v", err)
	}
	if notes.n != notified {
		t.Fatalf("rematch re-notified %d already-cached repos", notes.n-notified)
	}
	saved, err := st.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(saved.Matches) != n {
		t.Fatalf("cache holds %d matches after rematch, want %d", len(saved.Matches), n)
	}
	var found bool
	for _, m := range saved.Matches {
		if m.RepoID == rp.RepoID {
			found = true
			if m.Score != 95 {
				t.Fatalf("rematched entry score = %v, want 95", m.Score)
			}
		}
	}
	if !found {
		t.Fatalf("rematched repo missing from cache")
	}

	if _, err := e.MatchesForIdea(context.Background(), saved); err != nil {
		t.Fatalf("MatchesForIdea after rematch: %v", err)
	}
	if calls.Load() != scored {
		t.Fatalf("%d extra score calls after rematch", calls.Load()-scored)
	}
	if notes.n != notified {
		t.Fatalf("%d extra notifications after rematch", notes.n-notified)
	}
}

// TestRematchIdeaColdCachePersistsAllScored: with no prior MatchesForIdea read,
// a persisted rematch returns only RematchHiveKeep matches but caches every
// scored repo.
func TestRematchIdeaColdCachePersistsAllScored(t *testing.T) {
	st, reg := newFixtures(t)
	const n = 6
	repos := make([]registry.RepoProfile, 0, n)
	for i := 0; i < n; i++ {
		repos = append(repos, registry.RepoProfile{
			RepoID:      fmt.Sprintf("org/repo-%02d", i),
			Owner:       "bob",
			Description: "kubernetes multicluster scheduling placement",
			Topics:      []string{"scheduling"},
		})
	}
	if err := reg.Merge(repos); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	e := &Engine{Store: st, Registry: reg}
	idea := createIdea(t, st, "Kubernetes multicluster scheduling", "Schedule workloads across clusters with placement policies.")

	_, hive, _, err := e.RematchIdea(context.Background(), idea, true, nil)
	if err != nil {
		t.Fatalf("RematchIdea: %v", err)
	}
	if len(hive) != RematchHiveKeep {
		t.Fatalf("returned %d matches, want %d", len(hive), RematchHiveKeep)
	}
	saved, err := st.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(saved.Matches) != n {
		t.Fatalf("cache holds %d matches after cold rematch, want %d", len(saved.Matches), n)
	}
}
