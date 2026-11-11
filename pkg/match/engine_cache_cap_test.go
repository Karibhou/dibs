package match

import (
	"context"
	"fmt"
	"testing"

	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/store"
)

type countingNotifier struct{ n int }

func (c *countingNotifier) NewMatch(string, string, *store.Idea, *registry.RepoProfile, float64) {
	c.n++
}

// TestMatchesForIdeaCachesFullSetAndCapsReturned: with MaxMatches+1 repos the
// persisted cache holds all of them, so a second call re-scores and
// re-notifies nothing and does not rewrite the idea.
func TestMatchesForIdeaCachesFullSetAndCapsReturned(t *testing.T) {
	st, reg := newFixtures(t)
	n := MaxMatches + 1
	repos := make([]registry.RepoProfile, 0, n)
	for i := 0; i < n; i++ {
		repos = append(repos, registry.RepoProfile{
			RepoID:      fmt.Sprintf("org/repo-%02d", i),
			Owner:       "bob",
			Description: "telemetry dashboards grafana",
			Topics:      []string{"observability"},
		})
	}
	repos[0].Description = "kubernetes multicluster scheduling placement policies clusters"
	repos[1].Description = "kubernetes multicluster scheduling"
	repos[2].Description = "kubernetes scheduling dashboards"
	if err := reg.Merge(repos); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	notes := &countingNotifier{}
	e := &Engine{Store: st, Registry: reg, Notifier: notes}
	idea := createIdea(t, st, "Kubernetes multicluster scheduling", "Schedule workloads across clusters with placement policies.")

	first, err := e.MatchesForIdea(context.Background(), idea)
	if err != nil {
		t.Fatalf("MatchesForIdea: %v", err)
	}
	if len(first) != MaxMatches {
		t.Fatalf("first call returned %d, want %d", len(first), MaxMatches)
	}
	saved, err := st.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(saved.Matches) != n {
		t.Fatalf("persisted %d matches, want %d", len(saved.Matches), n)
	}
	notified := notes.n

	second, err := e.MatchesForIdea(context.Background(), saved)
	if err != nil {
		t.Fatalf("second MatchesForIdea: %v", err)
	}
	if len(second) != MaxMatches {
		t.Fatalf("second call returned %d, want %d", len(second), MaxMatches)
	}
	for i := range first {
		if first[i].RepoID != second[i].RepoID {
			t.Fatalf("entry %d changed: %s then %s", i, first[i].RepoID, second[i].RepoID)
		}
	}
	again, err := st.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !again.MatchesUpdatedAt.Equal(saved.MatchesUpdatedAt) {
		t.Fatalf("MatchesUpdatedAt bumped on a fully cached call")
	}
	if notes.n != notified {
		t.Fatalf("notifier fired %d more times on second call", notes.n-notified)
	}
}

// TestPersistRematchResultsMixedCaseRepoID: a stored match spelled Foo/Bar and
// a new match spelled foo/bar are the same repo, so it is replaced (not
// duplicated) and not notified again.
func TestPersistRematchResultsMixedCaseRepoID(t *testing.T) {
	st, reg := newFixtures(t)
	if err := reg.Merge([]registry.RepoProfile{{RepoID: "foo/bar", Owner: "bob", Description: "kubernetes scheduling"}}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	rp, err := reg.Get("foo/bar")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	idea := createIdea(t, st, "Kubernetes scheduling", "Schedule workloads.")
	if _, err := st.Mutate(idea.ID, false, func(i *store.Idea) error {
		i.Matches = []store.Match{{RepoID: "Foo/Bar", Score: NotifyThreshold + 10}}
		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	saved, err := st.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get idea: %v", err)
	}
	notes := &countingNotifier{}
	e := &Engine{Store: st, Registry: reg, Notifier: notes}
	fresh := []store.Match{{RepoID: "foo/bar", Score: NotifyThreshold + 10, RepoHash: RepoHash(rp)}}

	if err := e.PersistRematchResults(idea.ID, saved.UpdatedAt, "tldr", fresh, nil, ""); err != nil {
		t.Fatalf("PersistRematchResults: %v", err)
	}
	got, err := st.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Matches) != 1 || got.Matches[0].RepoID != "foo/bar" {
		t.Fatalf("matches = %+v, want the single replaced foo/bar", got.Matches)
	}
	if notes.n != 0 {
		t.Fatalf("notifier fired %d times, want 0 for an already-notified repo", notes.n)
	}
}
