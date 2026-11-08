// Leaderboard ordering: equal scores fall through to the settled-count
// tiebreak before the handle tiebreak.
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/dibs/pkg/game"
	"github.com/hivecommons/dibs/pkg/store"
)

// TestLeaderboardTiebreaksOnSettledCount builds two ideators with identical
// scores (2×settled vs 1×settled + 1×accepted + 10×draft) and checks the one
// with more settled ideas ranks first even though their handle sorts later.
func TestLeaderboardTiebreaksOnSettledCount(t *testing.T) {
	a, _ := newAPIFixture(t)

	mustCreate(t, a, "zed", "zed one", store.VisibilityPublic, store.StatusSettled)
	mustCreate(t, a, "zed", "zed two", store.VisibilityPublic, store.StatusSettled)

	mustCreate(t, a, "amy", "amy settled", store.VisibilityPublic, store.StatusSettled)
	mustCreate(t, a, "amy", "amy accepted", store.VisibilityPublic, store.StatusAccepted)
	settledPts := game.IdeaPoints(&store.Idea{Status: store.StatusSettled})
	acceptedPts := game.IdeaPoints(&store.Idea{Status: store.StatusAccepted})
	draftPts := game.IdeaPoints(&store.Idea{Status: store.StatusDraft})
	gap := 2*settledPts - (settledPts + acceptedPts)
	if gap <= 0 || gap%draftPts != 0 {
		t.Fatalf("fixture cannot tie: gap %d not a multiple of draft points %d", gap, draftPts)
	}
	for i := 0; i < gap/draftPts; i++ {
		mustCreate(t, a, "amy", "amy draft", store.VisibilityPublic, store.StatusDraft)
	}

	rec := httptest.NewRecorder()
	a.HandleLeaderboard(rec, httptest.NewRequest("GET", "/api/leaderboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBody[struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}](t, rec).Leaderboard
	if len(got) != 2 {
		t.Fatalf("leaderboard = %+v, want 2 entries", got)
	}
	if got[0].Score != got[1].Score {
		t.Fatalf("fixture must tie on score: %d vs %d (%+v)", got[0].Score, got[1].Score, got)
	}
	if got[0].Author != "zed" || got[0].Settled != 2 || got[0].Rank != 1 {
		t.Fatalf("rank 1 = %+v, want zed with 2 settled", got[0])
	}
	if got[1].Author != "amy" || got[1].Settled != 1 || got[1].Rank != 2 {
		t.Fatalf("rank 2 = %+v, want amy with 1 settled", got[1])
	}
}

// TestLeaderboardFoldsAuthorLoginCase checks one ideator whose settled ideas
// are stored under differently cased logins gets a single, summed row.
func TestLeaderboardFoldsAuthorLoginCase(t *testing.T) {
	a, _ := newAPIFixture(t)

	mustCreate(t, a, "Alice", "alice one", store.VisibilityPublic, store.StatusSettled)
	mustCreate(t, a, "alice", "alice two", store.VisibilityPublic, store.StatusSettled)

	rec := httptest.NewRecorder()
	a.HandleLeaderboard(rec, httptest.NewRequest("GET", "/api/leaderboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBody[struct {
		Leaderboard []LeaderboardEntry `json:"leaderboard"`
	}](t, rec).Leaderboard
	if len(got) != 1 {
		t.Fatalf("leaderboard = %+v, want 1 entry", got)
	}
	want := 2 * game.IdeaPoints(&store.Idea{Status: store.StatusSettled})
	if got[0].Settled != 2 || got[0].Score != want {
		t.Fatalf("entry = %+v, want 2 settled with score %d", got[0], want)
	}
}

// TestStatsFoldsAuthorLoginCase checks the market stats count one ideator
// once regardless of login spelling.
func TestStatsFoldsAuthorLoginCase(t *testing.T) {
	a, _ := newAPIFixture(t)

	mustCreate(t, a, "Alice", "alice one", store.VisibilityPublic, store.StatusSettled)
	mustCreate(t, a, "alice", "alice two", store.VisibilityPublic, store.StatusSettled)

	rec := httptest.NewRecorder()
	a.HandleStats(rec, httptest.NewRequest("GET", "/api/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBody[MarketStats](t, rec)
	if got.Ideators != 1 {
		t.Fatalf("ideators = %d, want 1", got.Ideators)
	}
}
