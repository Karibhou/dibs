// Credits API: the public credit wall and ideator profile stats.
//
// Credit-wall positioning: ideators are a first-class contributor class —
// the all-contributors spec has had a 💡 "ideas" emoji since 2016 and the
// academic CRediT taxonomy a "Conceptualization" role; Dibs makes that
// credit machine-readable and public.
package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/dibs/pkg/game"
	"github.com/hivecommons/dibs/pkg/store"
)

// CreditEntry is one row on the public credit wall: a settled idea's
// public-by-construction facts (settlement opened a public GitHub issue).
// The idea body is deliberately absent.
type CreditEntry struct {
	Author        string    `json:"author"`
	AuthorDisplay string    `json:"authorDisplay"`
	Title         string    `json:"title"`
	Symbol        string    `json:"symbol,omitempty"`
	TLDR          string    `json:"tldr,omitempty"`
	RepoID        string    `json:"repoID"`
	IssueURL      string    `json:"issueURL"`
	SettledAt     time.Time `json:"settledAt"`
}

// HandleCredits serves the credit wall data. It is registered OUTSIDE the
// auth middleware — the wall is public by design — so it must only ever
// expose settled-idea facts that are already public via the credited issue.
func (a *API) HandleCredits(w http.ResponseWriter, r *http.Request) {
	ideas, err := a.Store.ListSettled()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	entries := []CreditEntry{}
	for _, idea := range ideas {
		entries = append(entries, CreditEntry{
			Author:        idea.Author,
			AuthorDisplay: idea.AuthorDisplay,
			Title:         idea.Title,
			Symbol:        displaySymbol(idea),
			TLDR:          idea.TLDR,
			RepoID:        idea.TargetRepo,
			IssueURL:      idea.IssueURL,
			SettledAt:     idea.SettledTime(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credits": entries})
}

// IdeatorStats are the caller's lifecycle counts plus the gamification
// layer. Buckets are cumulative down the funnel: an accepted idea was
// offered, a settled idea was accepted — so settled ⊆ accepted ⊆ offered ⊆
// posted. Score, level, and badges are DERIVED from stored idea state on
// every request (see pkg/game) — never a mutable counter, so retries can't
// double-award.
type IdeatorStats struct {
	Posted   int `json:"posted"`
	Offered  int `json:"offered"`
	Accepted int `json:"accepted"`
	Settled  int `json:"settled"`
	game.Progress
	Badges []game.Badge `json:"badges"`
}

// handleMyStats returns the caller's ideator profile stats.
func (a *API) handleMyStats(w http.ResponseWriter, r *http.Request) {
	ideas, err := a.Store.ListByAuthor(identity(r).Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var st IdeatorStats
	for _, idea := range ideas {
		st.Posted++
		if len(idea.Offers) > 0 || idea.Status != store.StatusDraft {
			st.Offered++
		}
		if idea.Status == store.StatusAccepted || idea.Status == store.StatusIssueLaunched || idea.Status == store.StatusSettled {
			st.Accepted++
		}
		if idea.Status == store.StatusSettled {
			st.Settled++
		}
	}
	st.Progress = game.ProgressFor(game.Score(ideas))
	st.Badges = game.Badges(ideas)
	writeJSON(w, http.StatusOK, st)
}

// LeaderboardEntry is one ranked row on the public leaderboard.
type LeaderboardEntry struct {
	Rank          int          `json:"rank"`
	Author        string       `json:"author"`
	AuthorDisplay string       `json:"authorDisplay"`
	Score         int          `json:"score"`
	Level         game.Level   `json:"level"`
	Settled       int          `json:"settled"`
	Badges        []game.Badge `json:"badges"`
}

// HandleLeaderboard serves the public ranked leaderboard. Like the credit
// wall it is registered OUTSIDE the auth middleware, so it only lists
// ideators who already have at least one SETTLED idea (their handle is
// public by construction via the credited GitHub issue), and it exposes
// only aggregate numbers — never idea titles or bodies. Ordering: score
// desc, then settled count desc, then handle asc.
func (a *API) HandleLeaderboard(w http.ResponseWriter, r *http.Request) {
	all, err := a.Store.ListAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	byAuthor := map[string][]*store.Idea{}
	display := map[string]string{}
	handle := map[string]string{}
	settledAuthors := map[string]bool{}
	for _, idea := range all {
		key := strings.ToLower(idea.Author)
		byAuthor[key] = append(byAuthor[key], idea)
		display[key] = idea.AuthorDisplay
		if _, ok := handle[key]; !ok {
			handle[key] = idea.Author
		}
		if idea.Status == store.StatusSettled {
			settledAuthors[key] = true
		}
	}
	entries := []LeaderboardEntry{}
	for key := range settledAuthors {
		ideas := byAuthor[key]
		score := game.Score(ideas)
		level, _ := game.LevelFor(score)
		settled := 0
		for _, i := range ideas {
			if i.Status == store.StatusSettled {
				settled++
			}
		}
		entries = append(entries, LeaderboardEntry{
			Author:        handle[key],
			AuthorDisplay: display[key],
			Score:         score,
			Level:         level,
			Settled:       settled,
			Badges:        game.Badges(ideas),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		if entries[i].Settled != entries[j].Settled {
			return entries[i].Settled > entries[j].Settled
		}
		return entries[i].Author < entries[j].Author
	})
	for k := range entries {
		entries[k].Rank = k + 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"leaderboard": entries})
}
