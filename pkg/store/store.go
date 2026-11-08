// Package store persists Idea records as JSON files under DATA_DIR — one
// file per idea plus an index — with atomic temp+rename writes and a mutex
// guard. No external database, mirroring the hive file-store style.
package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/dibs/pkg/fsutil"
)

// Visibility values.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Status values for an idea's lifecycle state machine:
//
//	draft ──offer──▶ offered ──accept──▶ accepted ──launch──▶ issue_launched ──confirm──▶ settled
//	  │                 │  │                 └────────────direct confirm──────────────────────▲
//	  │                 │  └──launch (external target, no acceptance)──▶ issue_launched
//	  │                 └──decline──▶ declined ──re-offer──▶ offered
//	  └──direct accept (public ideas only)──▶ accepted
//
// issue_launched means the ideator opened GitHub's prefilled new-issue form
// (Dibs is just the matchmaker — the ideator files the issue with their own
// account); settled means they confirmed the filed issue's URL. The direct
// accepted → settled edge remains for the legacy token-based settlement.
// The offered → issue_launched edge serves EXTERNAL (non-hive) target repos,
// which have no owner on our side to accept.
const (
	StatusDraft         = "draft"
	StatusOffered       = "offered"
	StatusAccepted      = "accepted"
	StatusDeclined      = "declined"
	StatusIssueLaunched = "issue_launched"
	StatusSettled       = "settled"
)

// CanTransition reports whether the idea state machine allows from→to.
func CanTransition(from, to string) bool {
	switch from {
	case StatusDraft:
		return to == StatusOffered || to == StatusAccepted
	case StatusOffered:
		// offered → issue_launched covers EXTERNAL (non-hive) targets:
		// there is no repo owner on our side, so the ideator launches the
		// credited issue without an acceptance step.
		return to == StatusAccepted || to == StatusDeclined || to == StatusIssueLaunched
	case StatusDeclined:
		return to == StatusOffered
	case StatusAccepted:
		return to == StatusIssueLaunched || to == StatusSettled
	case StatusIssueLaunched:
		return to == StatusSettled
	default:
		return false
	}
}

// Offer status values (per-repo offers hanging off an idea).
const (
	OfferPending  = "pending"
	OfferAccepted = "accepted"
	OfferDeclined = "declined"
)

// Offer records the ideator explicitly offering the idea to one repo. For a
// PRIVATE idea, an offer is the one and only thing that reveals it to that
// repo's owner.
type Offer struct {
	RepoID    string     `json:"repoID"`
	Status    string     `json:"status"` // pending | accepted | declined
	CreatedAt time.Time  `json:"createdAt"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	// External marks an offer aimed at a repo OUTSIDE the hive registry.
	// There is no repo owner on our side to accept it, so the ideator can
	// launch the credited issue directly (offered → issue_launched).
	External bool `json:"external,omitempty"`
}

// MaxBodyBytes caps an idea's markdown body.
const MaxBodyBytes = 64 * 1024

// MaxTitleLen caps an idea's title.
const MaxTitleLen = 200

// MaxTags caps the number of idea tags.
const MaxTags = 20

// MaxTagLen caps a single idea tag.
const MaxTagLen = 40

// MaxPassedRepos caps the ideator's swipe-away history. Every idea file is
// decoded by the public listings, so per-idea lists that grow with each
// request need a ceiling.
const MaxPassedRepos = 500

// MaxOffers caps the number of per-repo offers hanging off one idea.
const MaxOffers = 100

// MaxRepoIDLen caps a repo identifier stored on an idea ("org/name":
// GitHub allows 39 + 1 + 100).
const MaxRepoIDLen = 140

// Match is a cached idea↔repo fit score. RepoHash fingerprints the repo
// profile the score was computed against so repo edits invalidate the cache;
// idea edits clear Matches wholesale (see Update).
type Match struct {
	RepoID      string    `json:"repoID"`
	Score       float64   `json:"score"`
	Reason      string    `json:"reason"`
	SuggestedAt time.Time `json:"suggestedAt"`
	RepoHash    string    `json:"repoHash,omitempty"`
	// ByLLM records whether the score came from the LLM (vs the
	// deterministic fallback).
	ByLLM bool `json:"byLLM,omitempty"`
}

// CNCFMatch is a cached idea↔CNCF project fit score.
type CNCFMatch struct {
	Name     string  `json:"name"`
	RepoID   string  `json:"repoID"`
	RepoURL  string  `json:"repoURL"`
	Maturity string  `json:"maturity"`
	Category string  `json:"category"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
	ByLLM    bool    `json:"byLLM,omitempty"`
}

// Idea is one ideator-authored idea.
type Idea struct {
	ID             string      `json:"id"`
	Author         string      `json:"author"`        // identity key (github login / provider:sub)
	AuthorDisplay  string      `json:"authorDisplay"` // human name for credit
	AuthorAvatar   string      `json:"authorAvatar,omitempty"`
	AuthorProvider string      `json:"authorProvider,omitempty"`
	Title          string      `json:"title"`
	Tags           []string    `json:"tags,omitempty"`
	Symbol         string      `json:"symbol,omitempty"` // $TICKER-style display symbol, assigned at creation
	Body           string      `json:"body"`             // markdown
	TLDR           string      `json:"tldr,omitempty"`   // filled by the matching wave
	Visibility     string      `json:"visibility"`       // public | private
	Status         string      `json:"status"`
	CreatedAt      time.Time   `json:"createdAt"`
	UpdatedAt      time.Time   `json:"updatedAt"`
	Matches        []Match     `json:"matches"`
	CNCFMatches    []CNCFMatch `json:"cncfMatches,omitempty"`
	// CNCFCatalogHash fingerprints the catalog and matching version used for
	// CNCFMatches; a missing or different value means the cache is stale.
	CNCFCatalogHash string `json:"cncfCatalogHash,omitempty"`
	// MatchesUpdatedAt is bumped whenever stored hive/CNCF suggestions change.
	MatchesUpdatedAt time.Time `json:"matchesUpdatedAt,omitempty"`
	// SuggestionsSeenAt records when the author last opened this idea's suggestions.
	SuggestionsSeenAt time.Time `json:"suggestionsSeenAt,omitempty"`
	// Offers are the repos the ideator explicitly offered this idea to.
	Offers []Offer `json:"offers,omitempty"`
	// PassedRepos are repos the ideator swiped away; they never resurface
	// in the idea's match candidates.
	PassedRepos []string `json:"passedRepos,omitempty"`
	TargetRepo  string   `json:"targetRepo,omitempty"`
	IssueURL    string   `json:"issueURL,omitempty"`
	// SettledAt is stamped once when the idea settles. The credit wall and
	// repo index read it (via SettledTime) instead of UpdatedAt, which later
	// edits keep bumping.
	SettledAt time.Time `json:"settledAt,omitempty"`
}

// SettledTime is when the idea settled. Legacy settled records predate
// SettledAt and fall back to UpdatedAt.
func (i *Idea) SettledTime() time.Time {
	if i.SettledAt.IsZero() {
		return i.UpdatedAt
	}
	return i.SettledAt
}

// issueURLKey normalizes a GitHub issue URL for uniqueness checks: the
// host, org/repo, and number compare case-insensitively, and a www. host,
// trailing slash, query, or fragment does not make it a different issue.
func issueURLKey(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	path := strings.ToLower(strings.Trim(u.Path, "/"))
	// "+5" and "05" name the same issue as "5"; compare by numeric value.
	if i := strings.LastIndex(path, "/"); i >= 0 {
		if n, err := strconv.Atoi(path[i+1:]); err == nil && n > 0 {
			path = path[:i+1] + strconv.Itoa(n)
		}
	}
	return host + "/" + path
}

// checkIssueURLFreeLocked rejects issueURL when another idea already holds
// it: one filed issue settles at most one idea. A sibling whose file was
// removed at runtime holds no issue, so it is skipped like list() skips it;
// any other read error aborts the claim. Caller holds s.mu.
func (s *Store) checkIssueURLFreeLocked(id, issueURL string) error {
	key := issueURLKey(issueURL)
	for otherID := range s.index {
		if otherID == id {
			continue
		}
		other, err := s.readLocked(otherID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if other.IssueURL != "" && issueURLKey(other.IssueURL) == key {
			return &ValidationError{"that issue is already credited to another idea"}
		}
	}
	return nil
}

// AuthorProvider infers the identity provider for legacy ideas. GitHub users
// predate provider prefixes, so an unprefixed author is a GitHub login.
func AuthorProvider(author string) string {
	if i := strings.Index(author, ":"); i > 0 {
		return author[:i]
	}
	return "github"
}

// OfferTo returns the idea's offer to repoID, or nil.
func (i *Idea) OfferTo(repoID string) *Offer {
	for k := range i.Offers {
		if strings.EqualFold(i.Offers[k].RepoID, repoID) {
			return &i.Offers[k]
		}
	}
	return nil
}

// HasPassed reports whether the ideator swiped repoID away.
func (i *Idea) HasPassed(repoID string) bool {
	for _, r := range i.PassedRepos {
		if strings.EqualFold(r, repoID) {
			return true
		}
	}
	return false
}

// TransitionTo advances the idea to status to, enforcing the lifecycle
// state machine. action names the attempted move (e.g. "offer", "accept",
// "settle") for the returned error message. A no-op (to == i.Status) always
// succeeds. Callers that previously duplicated the CanTransition-then-set
// pattern inline should call this instead so the invariant lives in one
// place.
func (i *Idea) TransitionTo(to, action string) error {
	if i.Status == to {
		return nil
	}
	if !CanTransition(i.Status, to) {
		return &ValidationError{Msg: "cannot " + action + " an idea in status " + i.Status}
	}
	i.Status = to
	return nil
}

// TryTransition sets the idea's status to to if the lifecycle state machine
// allows it, reporting whether the transition happened. Used where a
// disallowed transition is a silent no-op rather than a hard error.
func (i *Idea) TryTransition(to string) bool {
	if !CanTransition(i.Status, to) {
		return false
	}
	i.Status = to
	return true
}

// ErrNotFound is returned when an idea does not exist.
var ErrNotFound = errors.New("store: idea not found")

// ValidationError describes a rejected write.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return "store: " + e.Msg }

// Validate checks the user-writable fields.
func Validate(idea *Idea) error {
	if strings.TrimSpace(idea.Title) == "" {
		return &ValidationError{"title is required"}
	}
	if utf8.RuneCountInString(idea.Title) > MaxTitleLen {
		return &ValidationError{fmt.Sprintf("title exceeds %d characters", MaxTitleLen)}
	}
	if len(idea.Tags) > MaxTags {
		return &ValidationError{fmt.Sprintf("more than %d tags", MaxTags)}
	}
	for _, tag := range idea.Tags {
		if strings.TrimSpace(tag) == "" {
			return &ValidationError{"tags cannot be blank"}
		}
		if utf8.RuneCountInString(tag) > MaxTagLen {
			return &ValidationError{fmt.Sprintf("tag exceeds %d characters", MaxTagLen)}
		}
	}
	if strings.TrimSpace(idea.Body) == "" {
		return &ValidationError{"body is required"}
	}
	if len(idea.Body) > MaxBodyBytes {
		return &ValidationError{fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes)}
	}
	if len(idea.PassedRepos) > MaxPassedRepos {
		return &ValidationError{fmt.Sprintf("more than %d passed repos", MaxPassedRepos)}
	}
	for _, r := range idea.PassedRepos {
		if len(r) > MaxRepoIDLen {
			return &ValidationError{fmt.Sprintf("passed repo id exceeds %d characters", MaxRepoIDLen)}
		}
	}
	if len(idea.Offers) > MaxOffers {
		return &ValidationError{fmt.Sprintf("more than %d offers", MaxOffers)}
	}
	for _, o := range idea.Offers {
		if len(o.RepoID) > MaxRepoIDLen {
			return &ValidationError{fmt.Sprintf("offer repo id exceeds %d characters", MaxRepoIDLen)}
		}
	}
	if idea.Visibility != VisibilityPublic && idea.Visibility != VisibilityPrivate {
		return &ValidationError{`visibility must be "public" or "private"`}
	}
	switch idea.Status {
	case StatusDraft, StatusOffered, StatusAccepted, StatusDeclined, StatusIssueLaunched, StatusSettled:
	default:
		return &ValidationError{"invalid status"}
	}
	return nil
}

// indexEntry is the per-idea row kept in index.json so listings don't read
// every idea file.
type indexEntry struct {
	ID         string    `json:"id"`
	Author     string    `json:"author"`
	Visibility string    `json:"visibility"`
	Symbol     string    `json:"symbol,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Store is a mutex-guarded JSON file store.
type Store struct {
	mu    sync.RWMutex
	dir   string
	index map[string]indexEntry

	// pingMu guards lastWriteProbe: the last time Ping's write probe
	// succeeded, used to throttle probe-file churn (see Ping).
	pingMu         sync.Mutex
	lastWriteProbe time.Time
}

// New opens (creating if needed) a store rooted at dir. Ideas live in
// dir/ideas/<id>.json; the index at dir/ideas/index.json.
func New(dir string) (*Store, error) {
	ideasDir := filepath.Join(dir, "ideas")
	if err := os.MkdirAll(ideasDir, 0o755); err != nil {
		return nil, fmt.Errorf("store: creating data dir: %w", err)
	}
	s := &Store{dir: ideasDir, index: map[string]indexEntry{}}
	raw, err := os.ReadFile(s.indexPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fresh store
	case err != nil:
		return nil, fmt.Errorf("store: reading index: %w", err)
	default:
		var entries []indexEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("store: corrupt index: %w", err)
		}
		for _, e := range entries {
			s.index[e.ID] = e
		}
	}
	if err := s.reconcileLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// reconcileLocked adopts idea files that are missing from the index (e.g.
// left behind by a crash or failed index write), prunes index rows whose
// idea file is gone, and rewrites the index if anything changed. Rows whose
// file cannot be stat'ed for other reasons are kept so disk faults surface.
// Unreadable or corrupt files are skipped, not adopted.
func (s *Store) reconcileLocked() error {
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("store: scanning ideas dir: %w", err)
	}
	changed := false
	for _, f := range files {
		name := f.Name()
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || id == "index" || f.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if _, ok := s.index[id]; ok {
			continue
		}
		raw, err := os.ReadFile(s.ideaPath(id))
		if err != nil {
			continue
		}
		var idea Idea
		if err := json.Unmarshal(raw, &idea); err != nil || idea.ID != id {
			continue
		}
		s.index[id] = indexEntry{ID: id, Author: idea.Author, Visibility: idea.Visibility, Symbol: idea.Symbol, UpdatedAt: idea.UpdatedAt}
		changed = true
	}
	for id := range s.index {
		if _, err := os.Stat(s.ideaPath(id)); errors.Is(err, os.ErrNotExist) {
			delete(s.index, id)
			changed = true
		}
	}
	if changed {
		return s.writeIndexLocked()
	}
	return nil
}

func (s *Store) indexPath() string         { return filepath.Join(s.dir, "index.json") }
func (s *Store) ideaPath(id string) string { return filepath.Join(s.dir, id+".json") }

// pingWriteProbeInterval caps how often Ping performs its write probe; in
// between, a successful stat of the data dir is considered sufficient.
const pingWriteProbeInterval = 5 * time.Second

// Ping reports whether the store's data directory is still accessible and
// writable, i.e. the dependency required to actually serve traffic (every
// mutating API call and most reads go through this directory). It is meant
// for readiness probes: a stat failure here means the pod cannot serve
// requests even though the process itself is alive.
func (s *Store) Ping() error {
	s.mu.RLock()
	dir := s.dir
	s.mu.RUnlock()

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("store: data dir unavailable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("store: data dir path is not a directory: %s", dir)
	}
	// Confirm the directory is writable (not just present) with a probe
	// file; the store is single-writer so a stray leftover file from a
	// crashed probe is harmless and self-heals on the next successful
	// check. The probe is throttled: Ping is reachable through the
	// unauthenticated /readyz route, and an uncapped create+delete per
	// request would let any remote caller generate arbitrary write I/O
	// on the (network-attached) data volume. The stat above still runs
	// on every call, so a vanished data dir is detected immediately.
	s.pingMu.Lock()
	defer s.pingMu.Unlock()
	if time.Since(s.lastWriteProbe) < pingWriteProbeInterval {
		return nil
	}
	probe := filepath.Join(dir, ".healthz-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("store: data dir not writable: %w", err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	s.lastWriteProbe = time.Now()
	return nil
}

// newID returns a short random id (10 chars, url-safe).
func newID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generating id: %w", err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// persistLocked writes the idea file and index. Caller holds s.mu.
// If the index write fails, the idea file and in-memory index are rolled
// back to their previous state so memory and disk stay consistent.
func (s *Store) persistLocked(idea *Idea) error {
	prevRaw, readErr := os.ReadFile(s.ideaPath(idea.ID))
	prevEntry, hadEntry := s.index[idea.ID]
	if err := fsutil.AtomicWriteJSON(s.ideaPath(idea.ID), idea); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	s.index[idea.ID] = indexEntry{ID: idea.ID, Author: idea.Author, Visibility: idea.Visibility, Symbol: idea.Symbol, UpdatedAt: idea.UpdatedAt}
	if err := s.writeIndexLocked(); err != nil {
		if hadEntry {
			s.index[idea.ID] = prevEntry
		} else {
			delete(s.index, idea.ID)
		}
		if readErr == nil {
			_ = fsutil.AtomicWriteFile(s.ideaPath(idea.ID), prevRaw)
		} else {
			_ = os.Remove(s.ideaPath(idea.ID))
		}
		return err
	}
	return nil
}

func (s *Store) writeIndexLocked() error {
	entries := make([]indexEntry, 0, len(s.index))
	for _, e := range s.index {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	if err := fsutil.AtomicWriteJSON(s.indexPath(), entries); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Create validates and persists a new idea, filling ID/timestamps.
func (s *Store) Create(idea *Idea) error {
	if idea.Status == "" {
		idea.Status = StatusDraft
	}
	if err := Validate(idea); err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	idea.ID = id
	idea.CreatedAt = now
	idea.UpdatedAt = now
	if idea.Matches == nil {
		idea.Matches = []Match{}
	}
	if idea.AuthorProvider == "" {
		idea.AuthorProvider = AuthorProvider(idea.Author)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	idea.Symbol = uniqueSymbol(TickerSymbol(idea.Title), s.symbolTakenLocked)
	return s.persistLocked(idea)
}

// Get returns a copy of the idea.
func (s *Store) Get(id string) (*Idea, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readLocked(id)
}

func (s *Store) readLocked(id string) (*Idea, error) {
	if _, ok := s.index[id]; !ok {
		return nil, ErrNotFound
	}
	raw, err := os.ReadFile(s.ideaPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: reading idea: %w", err)
	}
	var idea Idea
	if err := json.Unmarshal(raw, &idea); err != nil {
		return nil, fmt.Errorf("store: corrupt idea %s: %w", id, err)
	}
	return &idea, nil
}

// Update validates and persists an existing idea, bumping UpdatedAt. The
// caller is responsible for authorization; ID/Author/CreatedAt and the
// server-managed fields (offers, passes, target, issue URL, settledAt) are
// preserved from the stored record. The caller's Status is applied only
// when it is a legal transition from the STORED status, checked under the
// lock, so a stale copy cannot overwrite a concurrent accept or settle.
// A settled idea's title and body are frozen: they are what the credit
// wall shows for the filed issue. Editing the CONTENT (title/body)
// invalidates the cached TLDR and matches so the match engine recomputes
// them.
func (s *Store) Update(idea *Idea) error {
	if err := Validate(idea); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.readLocked(idea.ID)
	if err != nil {
		return err
	}
	if existing.Status == StatusSettled && (idea.Title != existing.Title || idea.Body != existing.Body) {
		return &ValidationError{"a settled idea's title and body cannot be edited"}
	}
	if idea.Status != existing.Status && !CanTransition(existing.Status, idea.Status) {
		idea.Status = existing.Status
	}
	idea.Author = existing.Author
	idea.Tags = existing.Tags
	idea.Symbol = existing.Symbol
	idea.AuthorAvatar = existing.AuthorAvatar
	idea.AuthorProvider = existing.AuthorProvider
	if idea.AuthorProvider == "" {
		idea.AuthorProvider = AuthorProvider(idea.Author)
	}
	idea.CreatedAt = existing.CreatedAt
	idea.UpdatedAt = time.Now().UTC()
	idea.Offers = existing.Offers
	idea.PassedRepos = existing.PassedRepos
	idea.TargetRepo = existing.TargetRepo
	idea.IssueURL = existing.IssueURL
	idea.SettledAt = existing.SettledAt
	if idea.Status == StatusSettled && idea.SettledAt.IsZero() {
		// Legacy settled record: pin the settle time before UpdatedAt moves.
		idea.SettledAt = existing.UpdatedAt
	}
	if idea.Title != existing.Title || idea.Body != existing.Body {
		idea.TLDR = ""
		idea.Matches = []Match{}
		idea.CNCFMatches = []CNCFMatch{}
		idea.CNCFCatalogHash = ""
		idea.MatchesUpdatedAt = time.Time{}
		idea.SuggestionsSeenAt = time.Time{}
	} else {
		idea.TLDR = existing.TLDR
		if idea.Matches == nil {
			idea.Matches = existing.Matches
		}
		if idea.CNCFMatches == nil {
			idea.CNCFMatches = existing.CNCFMatches
			idea.CNCFCatalogHash = existing.CNCFCatalogHash
		}
		idea.MatchesUpdatedAt = existing.MatchesUpdatedAt
		idea.SuggestionsSeenAt = existing.SuggestionsSeenAt
	}
	return s.persistLocked(idea)
}

// Mutate atomically read-modify-writes an idea under the store lock. fn may
// change any field except identity/timestamps; the result is re-validated.
// An IssueURL set by fn must not already be held by another idea.
// touch controls whether UpdatedAt is bumped (cache refreshes shouldn't
// reorder listings). Returns a copy of the persisted idea.
func (s *Store) Mutate(id string, touch bool, fn func(*Idea) error) (*Idea, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idea, err := s.readLocked(id)
	if err != nil {
		return nil, err
	}
	prevIssueURL := idea.IssueURL
	if err := fn(idea); err != nil {
		return nil, err
	}
	if err := Validate(idea); err != nil {
		return nil, err
	}
	if idea.IssueURL != "" && idea.IssueURL != prevIssueURL {
		if err := s.checkIssueURLFreeLocked(idea.ID, idea.IssueURL); err != nil {
			return nil, err
		}
	}
	if touch {
		idea.UpdatedAt = time.Now().UTC()
	}
	if err := s.persistLocked(idea); err != nil {
		return nil, err
	}
	cp := *idea
	return &cp, nil
}

// Transition moves an idea through the state machine, rejecting any move
// CanTransition disallows.
func (s *Store) Transition(id, to string) (*Idea, error) {
	return s.Mutate(id, true, func(idea *Idea) error {
		if !CanTransition(idea.Status, to) {
			return &ValidationError{fmt.Sprintf("cannot transition %s → %s", idea.Status, to)}
		}
		idea.Status = to
		return nil
	})
}

// Delete removes an idea. The caller is responsible for authorization.
// The idea file must be readable for rollback; if unreadable (other than not-exist),
// the delete is aborted to ensure rollback can restore it if the index write fails.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[id]; !ok {
		return ErrNotFound
	}
	prevEntry := s.index[id]
	prevRaw, readErr := os.ReadFile(s.ideaPath(id))
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("store: reading idea before delete: %w", readErr)
	}
	if err := os.Remove(s.ideaPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: deleting idea: %w", err)
	}
	delete(s.index, id)
	if err := s.writeIndexLocked(); err != nil {
		s.index[id] = prevEntry
		if readErr == nil {
			_ = fsutil.AtomicWriteFile(s.ideaPath(id), prevRaw)
		}
		return err
	}
	return nil
}

// ListByAuthor returns every idea (any visibility) owned by author, newest
// first.
func (s *Store) ListByAuthor(author string) ([]*Idea, error) {
	return s.list(func(e indexEntry) bool { return strings.EqualFold(e.Author, author) })
}

// ListPublic returns every PUBLIC idea, newest first. Private ideas are
// filtered at the index level so they can never leak into a listing —
// this is the invariant the private-idea tests pin down.
func (s *Store) ListPublic() ([]*Idea, error) {
	return s.list(func(e indexEntry) bool { return e.Visibility == VisibilityPublic })
}

// ListOfferedTo returns every idea — INCLUDING private ones — that carries a
// PENDING offer (while the idea is still offered) to one of repoIDs, newest first. This is the only path by
// which a private idea reaches anyone but its author: the ideator's explicit
// offer to that specific repo.
func (s *Store) ListOfferedTo(repoIDs []string) ([]*Idea, error) {
	want := map[string]bool{}
	for _, r := range repoIDs {
		want[strings.ToLower(r)] = true
	}
	all, err := s.list(func(indexEntry) bool { return true })
	if err != nil {
		return nil, err
	}
	out := []*Idea{}
	for _, idea := range all {
		if idea.Status != StatusOffered {
			continue
		}
		for _, o := range idea.Offers {
			if o.Status == OfferPending && want[strings.ToLower(o.RepoID)] {
				out = append(out, idea)
				break
			}
		}
	}
	return out, nil
}

// ListAll returns every idea (any author, any visibility), newest first.
// Callers own the responsibility of not exposing private-idea CONTENT —
// the leaderboard, for example, derives only aggregate numbers from it.
func (s *Store) ListAll() ([]*Idea, error) {
	return s.list(func(indexEntry) bool { return true })
}

// ListSettled returns every SETTLED idea (any visibility), newest first.
// Settled ideas power the public credit wall: settlement opened a public
// GitHub issue, so the idea's existence, title, TLDR, author, and issue URL
// are already public — the credit wall exposes exactly that and nothing
// more (never the body, never unsettled ideas).
func (s *Store) ListSettled() ([]*Idea, error) {
	all, err := s.list(func(indexEntry) bool { return true })
	if err != nil {
		return nil, err
	}
	out := []*Idea{}
	for _, idea := range all {
		if idea.Status == StatusSettled {
			out = append(out, idea)
		}
	}
	return out, nil
}

func (s *Store) list(keep func(indexEntry) bool) ([]*Idea, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Idea
	for _, e := range s.index {
		if !keep(e) {
			continue
		}
		idea, err := s.readLocked(e.ID)
		if errors.Is(err, ErrNotFound) {
			// A file removed at runtime must not take every listing down.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, idea)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if out == nil {
		out = []*Idea{}
	}
	return out, nil
}
