// Market API: the public, logged-out-friendly "idea exchange" endpoints
// that power the market-style landing page — the ticker tape, the exchange
// stats strip, and the live markets board.
//
// PRIVACY INVARIANT: all three endpoints are registered OUTSIDE the auth
// middleware, so they must only ever expose (a) PUBLIC ideas, (b) SETTLED
// ideas — whose title/author/venue are already public via the credited
// GitHub issue (same rule as the credit wall), and (c) aggregate counts.
// A private, unsettled idea must never appear here in any form.
package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/dibs/pkg/store"
)

// Market phases: the exchange-flavored rendering of the idea state machine.
// OPEN → MATCHED → BUILDING → SHIPPED.
const (
	PhaseOpen     = "OPEN"     // listed, seeking a venue (draft, declined)
	PhaseMatched  = "MATCHED"  // offered to a repo, awaiting its decision
	PhaseBuilding = "BUILDING" // accepted — agent capacity committed (accepted, issue_launched)
	PhaseShipped  = "SHIPPED"  // settled — credited issue filed
)

// MarketPhase maps an idea status onto its market phase.
func MarketPhase(status string) string {
	switch status {
	case store.StatusOffered:
		return PhaseMatched
	case store.StatusAccepted, store.StatusIssueLaunched:
		return PhaseBuilding
	case store.StatusSettled:
		return PhaseShipped
	default: // draft, declined — back on the market
		return PhaseOpen
	}
}

// momentumWindow is how recent an idea's last activity must be to show the
// board's ▲ momentum indicator.
const momentumWindow = 7 * 24 * time.Hour

// BoardRow is one row on the public live-markets board. Deliberately thin:
// symbol, title, phase, venue, timestamps — never the body, never the
// author of an unsettled idea beyond what the public listing already shows.
type BoardRow struct {
	ID            string    `json:"id"`
	Symbol        string    `json:"symbol"`
	Title         string    `json:"title"`
	AuthorDisplay string    `json:"authorDisplay"`
	Phase         string    `json:"phase"`
	RepoID        string    `json:"repoID,omitempty"` // venue, once matched
	Momentum      bool      `json:"momentum"`         // activity within the last 7 days
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// TickerEntry is one item on the scrolling tape: the idea's most recent
// market event.
type TickerEntry struct {
	Symbol string    `json:"symbol"`
	Title  string    `json:"title"`
	Event  string    `json:"event"` // listed | matched | shipped
	RepoID string    `json:"repoID,omitempty"`
	At     time.Time `json:"at"`
}

// maxTickerEntries caps the tape length.
const maxTickerEntries = 30

// marketIdeas returns the ideas the public market surfaces may show:
// every PUBLIC idea, plus SETTLED ideas of any visibility (their facts are
// already public via the credited issue — the credit-wall rule).
func (a *API) marketIdeas() ([]*store.Idea, error) {
	public, err := a.Store.ListPublic()
	if err != nil {
		return nil, err
	}
	settled, err := a.Store.ListSettled()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []*store.Idea{}
	for _, idea := range append(public, settled...) {
		if !seen[idea.ID] {
			seen[idea.ID] = true
			out = append(out, idea)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// displaySymbol falls back to a derived symbol for ideas created before
// symbols were persisted.
func displaySymbol(idea *store.Idea) string {
	if idea.Symbol != "" {
		return idea.Symbol
	}
	return store.TickerSymbol(idea.Title)
}

// HandleBoard serves the live-markets board (public — see the privacy
// invariant at the top of this file).
func (a *API) HandleBoard(w http.ResponseWriter, r *http.Request) {
	ideas, err := a.marketIdeas()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	now := timeNow()
	rows := []BoardRow{}
	for _, idea := range ideas {
		row := BoardRow{
			ID:            idea.ID,
			Symbol:        displaySymbol(idea),
			Title:         idea.Title,
			AuthorDisplay: idea.AuthorDisplay,
			Phase:         MarketPhase(idea.Status),
			Momentum:      now.Sub(idea.UpdatedAt) <= momentumWindow,
			CreatedAt:     idea.CreatedAt,
			UpdatedAt:     idea.UpdatedAt,
		}
		// The venue is public only for hive-managed repos. External/CNCF
		// targets stay out of market surfaces.
		if (row.Phase == PhaseBuilding || row.Phase == PhaseShipped) && a.hiveManagedRepo(idea.TargetRepo) {
			row.RepoID = idea.TargetRepo
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"board": rows})
}

// HandleTicker serves the tape: every listed repo's ticker (symbol, index
// value, delta), plus each market idea's latest event, newest first
// (public — see the privacy invariant at the top of this file).
func (a *API) HandleTicker(w http.ResponseWriter, r *http.Request) {
	ideas, err := a.marketIdeas()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	repos, err := a.repoTickers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	entries := []TickerEntry{}
	for _, idea := range ideas {
		if len(entries) >= maxTickerEntries {
			break
		}
		e := TickerEntry{Symbol: displaySymbol(idea), Title: idea.Title, At: idea.UpdatedAt}
		switch MarketPhase(idea.Status) {
		case PhaseShipped:
			e.Event = "shipped"
			if a.hiveManagedRepo(idea.TargetRepo) {
				e.RepoID = idea.TargetRepo
			}
		case PhaseBuilding:
			e.Event = "matched"
			if a.hiveManagedRepo(idea.TargetRepo) {
				e.RepoID = idea.TargetRepo
			}
		default: // OPEN or MATCHED (pending offer venue stays private)
			e.Event = "listed"
			e.At = idea.CreatedAt
		}
		entries = append(entries, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticker": entries, "repos": repos})
}

func (a *API) hiveManagedRepo(repoID string) bool {
	if a == nil || a.Registry == nil || repoID == "" {
		return false
	}
	_, err := a.Registry.Get(repoID)
	return err == nil
}

// MarketStats are the exchange-wide aggregate numbers under the hero.
// Aggregates are derived over ALL ideas (private included) — counts leak
// nothing — mirroring how the leaderboard aggregates.
type MarketStats struct {
	Listed   int `json:"listed"`   // ideas ever listed
	Matched  int `json:"matched"`  // ideas a repo committed to (accepted or beyond)
	Shipped  int `json:"shipped"`  // settled ideas
	Ideators int `json:"ideators"` // ideators on The Board (≥1 settled idea)
}

// HandleStats serves the market stats strip (public — aggregate counts
// only, never idea content).
func (a *API) HandleStats(w http.ResponseWriter, r *http.Request) {
	all, err := a.Store.ListAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var st MarketStats
	settledAuthors := map[string]bool{}
	for _, idea := range all {
		st.Listed++
		switch idea.Status {
		case store.StatusAccepted, store.StatusIssueLaunched:
			st.Matched++
		case store.StatusSettled:
			st.Matched++
			st.Shipped++
			settledAuthors[strings.ToLower(idea.Author)] = true
		}
	}
	st.Ideators = len(settledAuthors)
	writeJSON(w, http.StatusOK, st)
}
