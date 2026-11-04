package match

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/dibs/pkg/catalog"
	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/store"
)

// MaxMatches caps how many candidate repos are RETURNED/surfaced for an idea.
// It does not cap the persisted cache, which holds every scored candidate so
// repos ranked below the cap are not re-scored (LLM cost) or re-notified.
const MaxMatches = 10

// CapMatches returns a score-desc sorted copy of ms limited to MaxMatches
// entries. It sorts first because the persisted order is not guaranteed
// (ScoreForRepo appends without re-sorting).
func CapMatches(ms []store.Match) []store.Match {
	out := append([]store.Match(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > MaxMatches {
		out = out[:MaxMatches]
	}
	return out
}

// cncfMatchCacheVersion invalidates persisted results when matching semantics
// change. v2 excludes zero-BM25 fillers, including old LLM-scored fillers.
const cncfMatchCacheVersion = "v2:"

// MaxCNCFCandidates caps how many BM25-ranked CNCF projects reach the LLM.
const MaxCNCFCandidates = 15

// NotifyThreshold is the minimum fresh score that triggers a "new match"
// notification to both sides.
const NotifyThreshold = 60

// RematchHiveKeep and RematchCNCFKeep shape the rematch result: hive-managed
// repos are favored (3) over CNCF suggestions (2).
const (
	RematchHiveKeep = 3
	RematchCNCFKeep = 2
)

// AcceptingBoost is added to a hive repo's score when its owner has opted in
// to accepting ideas — opted-in repos rank ahead of equal lexical fits.
const AcceptingBoost = 5

// BM25Weight and LLMWeight blend the deterministic lexical score with the
// LLM's judgment. Lexical evidence dominates: small models score nearly
// everything 90+, which would otherwise erase BM25's signal entirely.
const (
	BM25Weight = 0.6
	LLMWeight  = 0.4
)

// blendScores combines a BM25-scaled score (0-100) with an LLM score (0-100).
func blendScores(bm25, llm float64) float64 {
	return BM25Weight*bm25 + LLMWeight*llm
}

// MaxTLDRLen caps a generated TLDR.
const MaxTLDRLen = 280

var (
	ErrIdeaChanged = errors.New("match: idea changed during rematch")
	ErrRepoChanged = errors.New("match: repo changed during rematch")
	// ErrCatalogChanged means the CNCF catalog changed after the results were computed.
	ErrCatalogChanged = errors.New("match: catalog changed during rematch")
)

// maxPromptBody bounds how much idea body we send to the LLM.
const maxPromptBody = 4000

// Notifier receives match events (both directions). Nil disables.
type Notifier interface {
	NewMatch(ideaAuthor, repoOwner string, idea *store.Idea, repo *registry.RepoProfile, score float64)
}

// ProgressFunc receives best-effort rematch progress events. Nil disables
// reporting, keeping the organic matching path allocation-free.
type ProgressFunc func(ProgressEvent)

// ProgressEvent describes one observable step in the rematch pipeline.
type ProgressEvent struct {
	Phase  string  `json:"phase"`
	RepoID string  `json:"repoID,omitempty"`
	Symbol string  `json:"symbol,omitempty"`
	Score  float64 `json:"score,omitempty"`
	ByLLM  bool    `json:"byLLM,omitempty"`
	Note   string  `json:"note,omitempty"`
	Done   int     `json:"done,omitempty"`
	Total  int     `json:"total,omitempty"`
}

// Engine lazily computes and caches TLDRs and idea↔repo scores.
type Engine struct {
	Store    *store.Store
	Registry *registry.Registry
	Catalog  *catalog.Store
	// LLM is nil in fallback-only mode.
	LLM      *LLM
	Notifier Notifier
}

// CNCFMatch is a scored candidate from the separate CNCF catalog.
type CNCFMatch = store.CNCFMatch

// RepoHash fingerprints the score-relevant part of a repo profile; a changed
// hash invalidates every cached match against that repo.
func RepoHash(rp *registry.RepoProfile) string {
	h := sha256.New()
	h.Write([]byte(rp.RepoID + "\x00" + rp.Description + "\x00" + strings.Join(rp.Topics, ",") + "\x00" + rp.Appetite + "\x00" + strconv.FormatBool(rp.AcceptingIdeas)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// EnsureTLDR returns the idea's cached TLDR, generating and persisting one
// if missing ("X has an idea — let me TLDR it for you").
func (e *Engine) EnsureTLDR(ctx context.Context, idea *store.Idea) (string, error) {
	if idea.TLDR != "" {
		return idea.TLDR, nil
	}
	expected := idea.UpdatedAt
	tldr := e.generateTLDR(ctx, idea)
	updated, committed, err := e.commitIfFresh(idea.ID, expected, func(i *store.Idea) {
		if i.TLDR == "" {
			i.TLDR = tldr
		}
	})
	if err != nil {
		return "", err
	}
	if !committed {
		return tldr, nil
	}
	idea.TLDR = updated.TLDR
	return updated.TLDR, nil
}

func (e *Engine) generateTLDR(ctx context.Context, idea *store.Idea) string {
	if e.LLM != nil {
		out, err := e.LLM.Chat(ctx,
			"You summarize open-source project ideas. Reply with ONLY a punchy TLDR of at most two sentences, third person, no preamble.",
			"Title: "+idea.Title+"\n\n"+truncate(idea.Body, maxPromptBody))
		if err == nil && out != "" {
			llmStats.record(opTLDR, outcomeLLMOK)
			return truncate(out, MaxTLDRLen)
		}
		if err != nil {
			llmStats.record(opTLDR, outcomeLLMError)
			slog.Warn("match tldr llm failed, using fallback", "op", opTLDR, "err", err)
		} else {
			llmStats.record(opTLDR, outcomeLLMEmpty)
			slog.Warn("match tldr llm empty reply, using fallback", "op", opTLDR)
		}
	}
	return FallbackTLDR(idea)
}

// FallbackTLDR is the deterministic no-LLM TLDR: the first paragraph,
// clipped.
func FallbackTLDR(idea *store.Idea) string {
	body := strings.TrimSpace(idea.Body)
	if i := strings.Index(body, "\n\n"); i > 0 {
		body = body[:i]
	}
	body = strings.Join(strings.Fields(body), " ")
	return truncate(body, MaxTLDRLen)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// MatchesForIdea returns the idea's candidate repos (accepting-only, minus
// passed/offered ones), scoring lazily: cached matches whose RepoHash still
// matches are reused; everything else is (re)scored, persisted, and — if
// strong and fresh — notified to both sides. The full set is persisted; the
// returned slice is sorted by score desc and capped at MaxMatches.
func (e *Engine) MatchesForIdea(ctx context.Context, idea *store.Idea) ([]store.Match, error) {
	expected := idea.UpdatedAt
	cached := map[string]store.Match{}
	for _, m := range idea.Matches {
		cached[m.RepoID] = m
	}
	var out []store.Match
	var notify []pendingMatch
	changed := false
	// All hive-managed repos are candidates; acceptingIdeas is a boost, not a
	// gate (no hub repo has opted in yet — a gate would empty the pool).
	for _, rp := range e.Registry.List(false) {
		rp := rp
		if idea.HasPassed(rp.RepoID) || idea.OfferTo(rp.RepoID) != nil {
			continue
		}
		hash := RepoHash(&rp)
		if m, ok := cached[rp.RepoID]; ok && m.RepoHash == hash {
			out = append(out, m)
			continue
		}
		m := e.score(ctx, idea, &rp, hash)
		boostAccepting(&m, &rp)
		out = append(out, m)
		changed = true
		if m.Score >= NotifyThreshold {
			notify = append(notify, pendingMatch{repo: rp, score: m.Score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if changed {
		persisted := append([]store.Match(nil), out...)
		_, committed, err := e.commitIfFresh(idea.ID, expected, func(i *store.Idea) {
			i.Matches = persisted
			i.MatchesUpdatedAt = time.Now().UTC()
		})
		if err != nil {
			return nil, err
		}
		if committed {
			e.notifyMatches(idea, notify)
		}
	}
	out = CapMatches(out)
	if out == nil {
		out = []store.Match{}
	}
	return out, nil
}

// ScoreForRepo returns the (cached or fresh) match between one idea and one
// repo — the repo-side feed uses this so both directions share the cache.
func (e *Engine) ScoreForRepo(ctx context.Context, idea *store.Idea, rp *registry.RepoProfile) (store.Match, error) {
	hash := RepoHash(rp)
	for _, m := range idea.Matches {
		if m.RepoID == rp.RepoID && m.RepoHash == hash {
			return m, nil
		}
	}
	expected := idea.UpdatedAt
	m := e.score(ctx, idea, rp, hash)
	boostAccepting(&m, rp)
	_, committed, err := e.commitIfFresh(idea.ID, expected, func(i *store.Idea) {
		kept := m
		replaced := false
		for k := range i.Matches {
			if i.Matches[k].RepoID == rp.RepoID {
				i.Matches[k] = kept
				replaced = true
				break
			}
		}
		if !replaced {
			i.Matches = append(i.Matches, kept)
		}
		i.MatchesUpdatedAt = time.Now().UTC()
	})
	if err != nil {
		return store.Match{}, err
	}
	if committed && m.Score >= NotifyThreshold {
		e.notifyMatches(idea, []pendingMatch{{repo: *rp, score: m.Score}})
	}
	return m, nil
}

func (e *Engine) score(ctx context.Context, idea *store.Idea, rp *registry.RepoProfile, hash string) store.Match {
	m := store.Match{RepoID: rp.RepoID, SuggestedAt: time.Now().UTC(), RepoHash: hash}
	if e.LLM != nil {
		if score, reason, err := e.llmScore(ctx, idea, rp); err == nil {
			llmStats.record(opScore, outcomeLLMOK)
			m.Score, m.Reason, m.ByLLM = score, reason, true
			return m
		} else {
			llmStats.record(opScore, outcomeForErr(err))
			slog.Warn("match llm score failed, using fallback", "op", opScore, "idea_id", idea.ID, "repo_id", rp.RepoID, "err", err)
		}

	}
	m.Score, m.Reason = FallbackScore(idea, rp)
	return m
}

// CNCFMatchesForIdea returns CNCF project candidates from the catalog. BM25
// selects the top 15; the LLM reranks only those candidates when configured.
func (e *Engine) CNCFMatchesForIdea(ctx context.Context, idea *store.Idea) ([]CNCFMatch, error) {
	return e.cncfMatchesForIdea(ctx, idea, true, nil)
}

func (e *Engine) cncfMatchesForIdea(ctx context.Context, idea *store.Idea, persist bool, progress ProgressFunc) ([]CNCFMatch, error) {
	if e == nil || e.Catalog == nil {
		return []CNCFMatch{}, nil
	}
	// Edits to title/body clear the persisted set in Store.Update, so a hash
	// match means neither the idea, catalog, nor matching semantics changed.
	catalogHash := e.CatalogFingerprint()
	if persist && idea.CNCFCatalogHash == catalogHash {
		return append([]CNCFMatch{}, idea.CNCFMatches...), nil
	}
	expected := idea.UpdatedAt
	scanned := len(e.Catalog.List())
	candidates := e.Catalog.TopK(idea.Title+" "+idea.Body, MaxCNCFCandidates)
	// TopK pads with zero-score fillers; a filler is not a keyword fit and must not reach the LLM or the ideator.
	kept := candidates[:0]
	for _, c := range candidates {
		if c.Score > 0 {
			kept = append(kept, c)
		}
	}
	candidates = kept
	if progress != nil {
		progress(ProgressEvent{Phase: "cncf_bm25_start", Note: fmt.Sprintf("%d projects scanned via BM25", scanned), Done: len(candidates), Total: scanned})
		for i, c := range candidates {
			progress(ProgressEvent{
				Phase:  "cncf_bm25",
				RepoID: c.Project.RepoID,
				Score:  c.Score,
				Note:   c.Project.Name,
				Done:   i + 1,
				Total:  len(candidates),
			})
		}
	}
	topScore := 0.0
	if len(candidates) > 0 {
		topScore = candidates[0].Score
	}
	out := make([]CNCFMatch, 0, len(candidates))
	for i, c := range candidates {
		m := CNCFMatch{
			Name:     c.Project.Name,
			RepoID:   c.Project.RepoID,
			RepoURL:  c.Project.RepoURL,
			Maturity: c.Project.Maturity,
			Category: c.Project.Category,
			Score:    bm25ToScore(c.Score, topScore),
			Reason:   "BM25 keyword fit across CNCF project metadata",
		}
		if e.LLM != nil {
			if score, reason, err := e.llmScoreCNCF(ctx, idea, c.Project); err == nil {
				// Blend with the BM25 baseline instead of replacing it — see
				// blendScores.
				llmStats.record(opCNCFScore, outcomeLLMOK)
				m.Score, m.Reason, m.ByLLM = blendScores(m.Score, score), reason, true
			} else {
				llmStats.record(opCNCFScore, outcomeForErr(err))
				slog.Warn("match cncf llm score failed, using BM25 fallback", "op", opCNCFScore, "idea_id", idea.ID, "repo_id", c.Project.RepoID, "err", err)
			}
		}
		if progress != nil {
			progress(ProgressEvent{
				Phase:  "cncf_score",
				RepoID: m.RepoID,
				Score:  m.Score,
				ByLLM:  m.ByLLM,
				Note:   fmt.Sprintf("%d projects scanned via BM25", scanned),
				Done:   i + 1,
				Total:  len(candidates),
			})
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if persist && e.Store != nil {
		persisted := e.nonHiveCNCF(out)
		if len(persisted) > MaxMatches {
			persisted = persisted[:MaxMatches]
		}
		if _, _, err := e.commitIfFresh(idea.ID, expected, func(i *store.Idea) {
			i.CNCFMatches = persisted
			i.CNCFCatalogHash = catalogHash
			i.MatchesUpdatedAt = time.Now().UTC()
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// boostAccepting bumps an opted-in repo's score so accepting repos rank
// ahead of equal lexical fits. Capped at 100.
func boostAccepting(m *store.Match, rp *registry.RepoProfile) {
	if !rp.AcceptingIdeas {
		return
	}
	m.Score += AcceptingBoost
	if m.Score > 100 {
		m.Score = 100
	}
}

func (e *Engine) nonHiveCNCF(matches []CNCFMatch) []CNCFMatch {
	out := make([]CNCFMatch, 0, len(matches))
	for _, m := range matches {
		if e.Registry != nil {
			if _, err := e.Registry.Get(m.RepoID); err == nil {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

type hiveCandidate struct {
	Repo  registry.RepoProfile
	Score float64
}

func (e *Engine) hiveBM25Candidates(idea *store.Idea, repos []registry.RepoProfile) []hiveCandidate {
	projects := make([]catalog.Project, 0, len(repos))
	for _, rp := range repos {
		projects = append(projects, catalog.Project{
			Name:        hiveRepoCorpusName(rp.RepoID),
			RepoID:      rp.RepoID,
			Description: rp.Description,
			Topics:      rp.Topics,
			Readme:      rp.Appetite,
		})
	}
	ranked := catalog.NewBM25(projects).TopK(idea.Title+" "+idea.TLDR+" "+idea.Body, len(projects))
	out := make([]hiveCandidate, 0, len(ranked))
	byID := map[string]registry.RepoProfile{}
	for _, rp := range repos {
		byID[rp.RepoID] = rp
	}
	top := 0.0
	if len(ranked) > 0 {
		top = ranked[0].Score
	}
	for _, c := range ranked {
		rp := byID[c.Project.RepoID]
		out = append(out, hiveCandidate{Repo: rp, Score: bm25ToScore(c.Score, top)})
	}
	return out
}

func hiveRepoCorpusName(repoID string) string {
	return repoID + " " + strings.NewReplacer("/", " ", "-", " ", "_", " ").Replace(splitCamel(repoID))
}

func splitCamel(s string) string {
	var b strings.Builder
	var prevLower bool
	for _, r := range s {
		isUpper := r >= 'A' && r <= 'Z'
		if isUpper && prevLower {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prevLower = r >= 'a' && r <= 'z'
	}
	return b.String()
}

// RematchIdea runs the full idea matching pipeline from fresh inputs. When
// persist is false it does not update the store or notify; when true it saves
// the same fields organic matching uses and emits normal fresh-match events.
func (e *Engine) RematchIdea(ctx context.Context, idea *store.Idea, persist bool, progress ProgressFunc) (string, []store.Match, []CNCFMatch, error) {
	if e == nil {
		return "", nil, nil, fmt.Errorf("match: engine is nil")
	}
	work := *idea
	catalogHash := e.CatalogFingerprint()
	if work.TLDR == "" {
		if progress != nil {
			progress(ProgressEvent{Phase: "tldr_start", Note: "Generating TLDR"})
		}
		work.TLDR = e.generateTLDR(ctx, &work)
		if progress != nil {
			progress(ProgressEvent{Phase: "tldr_done", Note: work.TLDR})
		}
	} else if progress != nil {
		progress(ProgressEvent{Phase: "tldr_done", Note: "Using cached TLDR"})
	}
	var matches []store.Match
	repos := []registry.RepoProfile{}
	// Candidates are all hive-managed repos; acceptingIdeas boosts, never gates.
	for _, rp := range e.Registry.List(false) {
		if work.HasPassed(rp.RepoID) || work.OfferTo(rp.RepoID) != nil {
			continue
		}
		repos = append(repos, rp)
	}
	if progress != nil {
		progress(ProgressEvent{Phase: "hive_start", Note: "Scoring hive repos", Total: len(repos)})
	}
	baseline := e.hiveBM25Candidates(&work, repos)
	for i, c := range baseline {
		rp := c.Repo
		m := store.Match{
			RepoID:      rp.RepoID,
			Score:       c.Score,
			Reason:      "BM25 keyword fit across repo name, description, topics, and appetite",
			SuggestedAt: time.Now().UTC(),
			RepoHash:    RepoHash(&rp),
		}
		boostAccepting(&m, &rp)
		matches = append(matches, m)
		if progress != nil {
			progress(ProgressEvent{
				Phase:  "hive_score",
				RepoID: rp.RepoID,
				Symbol: rp.Symbol,
				Score:  m.Score,
				Note:   m.Reason,
				Done:   i + 1,
				Total:  len(baseline),
			})
		}
	}
	rerank := len(matches)
	if rerank > MaxCNCFCandidates {
		rerank = MaxCNCFCandidates
	}
	for i := 0; i < rerank; i++ {
		rp := baseline[i].Repo
		llmMatch := e.score(ctx, &work, &rp, RepoHash(&rp))
		if llmMatch.ByLLM {
			// Blend, don't replace: the LLM refines the lexical ranking but
			// cannot erase it (small models score nearly everything 90+).
			llmMatch.Score = blendScores(baseline[i].Score, llmMatch.Score)
			boostAccepting(&llmMatch, &rp)
			matches[i] = llmMatch
		}
		if progress != nil && llmMatch.ByLLM {
			progress(ProgressEvent{
				Phase:  "hive_rerank",
				RepoID: rp.RepoID,
				Symbol: rp.Symbol,
				Score:  matches[i].Score,
				ByLLM:  matches[i].ByLLM,
				Note:   matches[i].Reason,
				Done:   i + 1,
				Total:  rerank,
			})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	scored := matches
	matches = capHiveMatches(scored)
	cncf, err := e.cncfMatchesForIdea(ctx, &work, false, progress)
	if err != nil {
		return "", nil, nil, err
	}
	cncf = e.nonHiveCNCF(cncf)
	if len(cncf) > RematchCNCFKeep {
		cncf = cncf[:RematchCNCFKeep]
	}
	if progress != nil {
		progress(ProgressEvent{
			Phase: "final_selection",
			Note:  fmt.Sprintf("%d hive, %d CNCF selected", len(matches), len(cncf)),
		})
	}
	if persist {
		if err := e.PersistRematchResults(idea.ID, work.UpdatedAt, work.TLDR, scored, cncf, catalogHash); err != nil {
			return "", nil, nil, err
		}
	}
	if progress != nil {
		progress(ProgressEvent{Phase: "done", Note: "Rematch complete"})
	}
	return work.TLDR, matches, cncf, nil
}

// capHiveMatches trims a ranked hive slice to RematchHiveKeep for return;
// persistence keeps the full scored slice so the cache never shrinks below it.
func capHiveMatches(matches []store.Match) []store.Match {
	if len(matches) > RematchHiveKeep {
		return matches[:RematchHiveKeep]
	}
	return matches
}

// unchangedSince reports whether the stored idea is still the version that
// was scored; a mismatch means an edit invalidated the snapshot's results.
func unchangedSince(i *store.Idea, expected time.Time) bool {
	return i.UpdatedAt.Equal(expected)
}

// errStaleSnapshot aborts a Store.Mutate so a stale snapshot writes nothing.
var errStaleSnapshot = errors.New("match: stale idea snapshot")

// commitIfFresh is the single gate through which Engine writers release side
// effects. It runs apply on the stored idea and persists the result only if
// the idea is still the version captured at expected; otherwise it writes
// nothing and reports committed=false. Callers must notify (or otherwise act
// on the computed results) only when committed is true.
func (e *Engine) commitIfFresh(ideaID string, expected time.Time, apply func(*store.Idea)) (updated *store.Idea, committed bool, err error) {
	updated, err = e.Store.Mutate(ideaID, false, func(i *store.Idea) error {
		if !unchangedSince(i, expected) {
			return errStaleSnapshot
		}
		apply(i)
		return nil
	})
	if errors.Is(err, errStaleSnapshot) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// pendingMatch is a strong match held back until its persist has committed.
type pendingMatch struct {
	repo  registry.RepoProfile
	score float64
}

// notifyMatches emits NewMatch for already-committed matches, in order.
func (e *Engine) notifyMatches(idea *store.Idea, pending []pendingMatch) {
	if e.Notifier == nil {
		return
	}
	for k := range pending {
		p := &pending[k]
		e.Notifier.NewMatch(idea.Author, p.repo.Owner, idea, &p.repo, p.score)
	}
}

// CatalogFingerprint returns the current CNCF match-cache fingerprint, combining
// the matching version and catalog contents, or "" when no catalog is configured.
func (e *Engine) CatalogFingerprint() string {
	if e == nil || e.Catalog == nil {
		return ""
	}
	return cncfMatchCacheVersion + e.Catalog.Fingerprint()
}

// PersistRematchResults commits a previously computed rematch result if the
// idea has not changed since expectedUpdatedAt, then emits the normal match
// notifications for persisted hive matches. catalogHash is the catalog
// fingerprint taken before the CNCF set was computed; a catalog that has since
// changed yields ErrCatalogChanged.
//
// matches is merged over the cached hive matches by RepoID: entries for repos
// in matches are replaced and every other cached entry is kept, so the cache
// still holds every scored candidate and the next MatchesForIdea neither
// re-scores nor re-notifies repos the rematch merely ranked below its cap.
// Repos already cached at or above NotifyThreshold are not notified again.
func (e *Engine) PersistRematchResults(ideaID string, expectedUpdatedAt time.Time, tldr string, matches []store.Match, cncf []CNCFMatch, catalogHash string) error {
	if e.Registry != nil {
		for _, m := range matches {
			rp, err := e.Registry.Get(m.RepoID)
			if err != nil || m.RepoHash != RepoHash(rp) {
				return ErrRepoChanged
			}
		}
	}
	if e.Catalog != nil && e.CatalogFingerprint() != catalogHash {
		return ErrCatalogChanged
	}
	alreadyNotified := map[string]bool{}
	updated, committed, err := e.commitIfFresh(ideaID, expectedUpdatedAt, func(i *store.Idea) {
		i.TLDR = tldr
		replaced := make(map[string]bool, len(matches))
		for _, m := range matches {
			replaced[m.RepoID] = true
		}
		merged := append([]store.Match(nil), matches...)
		for _, old := range i.Matches {
			if !replaced[old.RepoID] {
				merged = append(merged, old)
			}
			if old.Score >= NotifyThreshold {
				alreadyNotified[old.RepoID] = true
			}
		}
		i.Matches = merged
		i.CNCFMatches = append([]CNCFMatch(nil), cncf...)
		if e.Catalog != nil {
			i.CNCFCatalogHash = catalogHash
		}
		i.MatchesUpdatedAt = time.Now().UTC()
	})
	if err != nil {
		return err
	}
	if !committed {
		return ErrIdeaChanged
	}
	if e.Notifier != nil {
		var pending []pendingMatch
		for _, m := range matches {
			if m.Score < NotifyThreshold || alreadyNotified[m.RepoID] {
				continue
			}
			if rp, err := e.Registry.Get(m.RepoID); err == nil {
				pending = append(pending, pendingMatch{repo: *rp, score: m.Score})
			}
		}
		e.notifyMatches(updated, pending)
	}
	return nil
}

func bm25ToScore(score, top float64) float64 {
	if top <= 0 {
		return 0
	}
	scaled := 100 * score / top
	if scaled > 100 {
		return 100
	}
	return scaled
}

var llmJSONRe = regexp.MustCompile(`\{[^{}]*\}`)

func (e *Engine) llmScore(ctx context.Context, idea *store.Idea, rp *registry.RepoProfile) (float64, string, error) {
	user := "IDEA\nTitle: " + idea.Title + "\nTLDR: " + idea.TLDR + "\nBody:\n" + truncate(idea.Body, maxPromptBody) +
		"\n\nREPO\nName: " + rp.RepoID + "\nDescription: " + rp.Description +
		"\nTopics: " + strings.Join(rp.Topics, ", ") + "\nAppetite: " + rp.Appetite
	out, err := e.LLM.Chat(ctx,
		`You score how well an idea fits an open-source repository. Reply with ONLY a JSON object: {"score": <0-100 integer>, "reason": "<one line why>"}.`,
		user)
	if err != nil {
		return 0, "", err
	}
	if strings.TrimSpace(out) == "" {
		return 0, "", errLLMEmpty
	}
	raw := llmJSONRe.FindString(out)
	if raw == "" {
		return 0, "", fmt.Errorf("match: no JSON in llm reply: %.80s (%w)", out, errLLMUnparsable)
	}
	var parsed struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return 0, "", fmt.Errorf("%w: %w", errLLMUnparsable, err)
	}
	if parsed.Score < 0 {
		parsed.Score = 0
	}
	if parsed.Score > 100 {
		parsed.Score = 100
	}
	return parsed.Score, truncate(parsed.Reason, 200), nil
}

func (e *Engine) llmScoreCNCF(ctx context.Context, idea *store.Idea, p catalog.Project) (float64, string, error) {
	user := "IDEA\nTitle: " + idea.Title + "\nTLDR: " + idea.TLDR + "\nBody:\n" + truncate(idea.Body, maxPromptBody) +
		"\n\nREPO\nName: " + p.RepoID + "\nProject: " + p.Name + "\nDescription: " + p.Description +
		"\nTopics: " + strings.Join(p.Topics, ", ") + "\nLanguage: " + p.Language +
		"\nCNCF maturity: " + p.Maturity + "\nCategory: " + p.Category + "\nREADME intro:\n" + truncate(p.Readme, 1200)
	out, err := e.LLM.Chat(ctx,
		`You score how well an idea fits an open-source repository. Reply with ONLY a JSON object: {"score": <0-100 integer>, "reason": "<one line why>"}.`,
		user)
	if err != nil {
		return 0, "", err
	}
	if strings.TrimSpace(out) == "" {
		return 0, "", errLLMEmpty
	}
	raw := llmJSONRe.FindString(out)
	if raw == "" {
		return 0, "", fmt.Errorf("match: no JSON in llm reply: %.80s (%w)", out, errLLMUnparsable)
	}
	var parsed struct {
		Score  float64 `json:"score"`
		Reason string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return 0, "", fmt.Errorf("%w: %w", errLLMUnparsable, err)
	}
	if parsed.Score < 0 {
		parsed.Score = 0
	}
	if parsed.Score > 100 {
		parsed.Score = 100
	}
	return parsed.Score, truncate(parsed.Reason, 200), nil
}

// ── deterministic fallback ──────────────────────────────────────────────

var tokenRe = regexp.MustCompile(`[a-z0-9][a-z0-9\-]{2,}`)

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true,
	"with": true, "are": true, "was": true, "you": true, "your": true,
	"have": true, "has": true, "can": true, "will": true, "would": true,
	"should": true, "could": true, "from": true, "into": true, "its": true,
	"our": true, "their": true, "them": true, "they": true, "not": true,
	"but": true, "all": true, "any": true, "more": true, "some": true,
	"what": true, "when": true, "where": true, "how": true, "why": true,
	"idea": true, "ideas": true, "repo": true, "repos": true,
}

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokenRe.FindAllString(strings.ToLower(s), -1) {
		if !stopwords[t] {
			out[t] = true
		}
	}
	return out
}

// FallbackScore is the deterministic no-LLM scorer: weighted term overlap
// between the idea's text and the repo's topics (weight 3), appetite
// (weight 2), and name/description (weight 1). Score = 100 × matched weight
// / total repo weight.
func FallbackScore(idea *store.Idea, rp *registry.RepoProfile) (float64, string) {
	ideaTerms := tokens(idea.Title + " " + idea.TLDR + " " + idea.Body)
	type term struct {
		word   string
		weight float64
	}
	var terms []term
	for _, t := range rp.Topics {
		for w := range tokens(t) {
			terms = append(terms, term{w, 3})
		}
	}
	for w := range tokens(rp.Appetite) {
		terms = append(terms, term{w, 2})
	}
	for w := range tokens(rp.RepoID + " " + rp.Description) {
		terms = append(terms, term{w, 1})
	}
	var total, matched float64
	var hits []string
	for _, t := range terms {
		total += t.weight
		if ideaTerms[t.word] {
			matched += t.weight
			hits = append(hits, t.word)
		}
	}
	if total == 0 {
		return 0, "no repo profile terms to match against"
	}
	score := 100 * matched / total
	if len(hits) == 0 {
		return 0, "no keyword overlap with the repo profile"
	}
	sort.Strings(hits)
	if len(hits) > 5 {
		hits = hits[:5]
	}
	return score, "keyword overlap: " + strings.Join(hits, ", ")
}
