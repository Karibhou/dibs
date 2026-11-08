// Package api implements the Dibs JSON API: author-scoped idea CRUD and
// the repo-registry endpoints. All handlers assume auth.Middleware has run
// and an identity is on the context.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/dibs/pkg/auth"
	"github.com/hivecommons/dibs/pkg/intake"
	"github.com/hivecommons/dibs/pkg/match"
	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/store"
)

// timeNow is stubbed in tests.
var timeNow = func() time.Time { return time.Now().UTC() }

// maxRequestBody bounds any request body we parse: idea body cap plus
// generous headroom for JSON framing and other fields.
const maxRequestBody = store.MaxBodyBytes + 64*1024

const adminRematchTimeout = 5 * time.Minute

// API wires the store, registry, match engine, settler, and notifications
// into HTTP handlers.
type API struct {
	Store    IdeaStore
	Registry RepoRegistry
	History  RepoHistory
	News     RepoNews
	// Engine is nil when matching is disabled (Wave-2 features degrade).
	Engine Matcher
	// Settler opens credited GitHub issues; a nil-GitHub settler records
	// accepts without opening issues.
	Settler IdeaSettler
	// Issues verifies the issue URL an ideator pastes at confirm-issue
	// against GitHub (nil skips verification; production always sets it).
	Issues IssueLookup
	// Notify is the in-app notification store (nil disables).
	Notify Notifier

	rematchMu   sync.Mutex
	rematchJobs map[string]*adminRematchJob
	rematchSeq  int64
}

// Register mounts the API routes onto mux under basePath — "" for the root,
// or a normalized "/prefix" (e.g. "/ideas").
func (a *API) Register(mux *http.ServeMux, basePath string) {
	mux.HandleFunc("GET "+basePath+"/api/me", a.handleMe)
	mux.HandleFunc("GET "+basePath+"/api/me/stats", a.handleMyStats)
	mux.HandleFunc("GET "+basePath+"/api/admin/ideas", a.handleAdminIdeas)
	mux.HandleFunc("POST "+basePath+"/api/admin/ideas/{id}/rematch", a.handleAdminRematch)
	mux.HandleFunc("GET "+basePath+"/api/admin/ideas/{id}/rematch", a.handleAdminRematchStatus)
	mux.HandleFunc("GET "+basePath+"/api/intake/config", intake.HandleConfig)
	mux.HandleFunc("POST "+basePath+"/api/intake", intake.HandleUpload)
	mux.HandleFunc("GET "+basePath+"/api/ideas", a.handleListIdeas)
	mux.HandleFunc("POST "+basePath+"/api/ideas", a.handleCreateIdea)
	mux.HandleFunc("GET "+basePath+"/api/ideas/{id}", a.handleGetIdea)
	mux.HandleFunc("PUT "+basePath+"/api/ideas/{id}", a.handleUpdateIdea)
	mux.HandleFunc("DELETE "+basePath+"/api/ideas/{id}", a.handleDeleteIdea)
	mux.HandleFunc("GET "+basePath+"/api/repos", a.handleListRepos)
	mux.HandleFunc("PUT "+basePath+"/api/repos/{org}/{repo}", a.handleUpdateRepo)
	a.registerOfferRoutes(mux, basePath)
	a.registerSettlement(mux, basePath)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func identity(r *http.Request) *auth.Identity {
	return auth.FromContext(r.Context())
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func ideaForViewer(idea *store.Idea, viewer string) *store.Idea {
	cp := *idea
	if !strings.EqualFold(cp.Author, viewer) {
		cp.SuggestionsSeenAt = time.Time{}
	}
	return &cp
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, identity(r))
}

type adminMatchSummary struct {
	Count int               `json:"count"`
	Hive  []store.Match     `json:"hive"`
	CNCF  []store.CNCFMatch `json:"cncf,omitempty"`
	Top   []struct {
		RepoID string  `json:"repoID"`
		Score  float64 `json:"score"`
	} `json:"top"`
}

type adminIdea struct {
	ID             string            `json:"id"`
	Author         string            `json:"author"`
	AuthorDisplay  string            `json:"authorDisplay,omitempty"`
	AuthorAvatar   string            `json:"authorAvatar,omitempty"`
	AuthorProvider string            `json:"authorProvider,omitempty"`
	Title          string            `json:"title"`
	TLDR           string            `json:"tldr,omitempty"`
	Symbol         string            `json:"symbol,omitempty"`
	Visibility     string            `json:"visibility"`
	Status         string            `json:"status"`
	CreatedAt      time.Time         `json:"createdAt"`
	UpdatedAt      time.Time         `json:"updatedAt"`
	Matches        adminMatchSummary `json:"matches"`
}

type adminRematchJob struct {
	ID            string
	Status        string
	Dry           bool
	IdeaUpdatedAt time.Time
	TLDR          string
	CatalogHash   string
	Matches       adminMatchSummary
	Error         string
	FinishedAt    time.Time
	Events        []adminRematchEvent
	Next          int64
}

type adminRematchEvent struct {
	Seq int64 `json:"seq"`
	match.ProgressEvent
}

type adminRematchResponse struct {
	JobID      string              `json:"jobID,omitempty"`
	Status     string              `json:"status"`
	Dry        bool                `json:"dry"`
	TLDR       string              `json:"tldr,omitempty"`
	Matches    adminMatchSummary   `json:"matches,omitempty"`
	Error      string              `json:"error,omitempty"`
	FinishedAt time.Time           `json:"finishedAt,omitempty"`
	Events     []adminRematchEvent `json:"events,omitempty"`
	Next       int64               `json:"next"`
}

func (j *adminRematchJob) responseSince(since int64) adminRematchResponse {
	events := []adminRematchEvent{}
	for _, ev := range j.Events {
		if ev.Seq > since {
			events = append(events, ev)
		}
	}
	return adminRematchResponse{
		Status:     j.Status,
		JobID:      j.ID,
		Dry:        j.Dry,
		TLDR:       j.TLDR,
		Matches:    j.Matches,
		Error:      j.Error,
		FinishedAt: j.FinishedAt,
		Events:     events,
		Next:       j.Next,
	}
}

func summarizeMatches(matches []store.Match, cncf []store.CNCFMatch) adminMatchSummary {
	out := adminMatchSummary{Count: len(matches) + len(cncf), Hive: matches, CNCF: cncf}
	for _, m := range matches {
		if len(out.Top) >= 3 {
			break
		}
		out.Top = append(out.Top, struct {
			RepoID string  `json:"repoID"`
			Score  float64 `json:"score"`
		}{RepoID: m.RepoID, Score: m.Score})
	}
	return out
}

func (a *API) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	id := identity(r)
	if id == nil || !auth.IsAdmin(id.Username) {
		writeError(w, http.StatusForbidden, "admin access required")
		return false
	}
	return true
}

func (a *API) handleAdminIdeas(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	ideas, err := a.Store.ListAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]adminIdea, 0, len(ideas))
	for _, idea := range ideas {
		out = append(out, adminIdea{
			ID:             idea.ID,
			Author:         idea.Author,
			AuthorDisplay:  idea.AuthorDisplay,
			AuthorAvatar:   idea.AuthorAvatar,
			AuthorProvider: firstNonEmpty(idea.AuthorProvider, store.AuthorProvider(idea.Author)),
			Title:          idea.Title,
			TLDR:           idea.TLDR,
			Symbol:         idea.Symbol,
			Visibility:     idea.Visibility,
			Status:         idea.Status,
			CreatedAt:      idea.CreatedAt,
			UpdatedAt:      idea.UpdatedAt,
			Matches:        summarizeMatches(match.CapMatches(idea.Matches), idea.CNCFMatches),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleAdminRematch(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "matching not configured")
		return
	}
	idea, err := a.Store.Get(r.PathValue("id"))
	if err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	persist := r.URL.Query().Get("dry") != "1"
	dry := !persist
	if !dry {
		a.tryApplyDryRematch(w, idea)
		return
	}

	a.startAdminRematch(w, r, idea, persist, dry)
}

func (a *API) startAdminRematch(w http.ResponseWriter, r *http.Request, idea *store.Idea, persist, dry bool) {
	a.rematchMu.Lock()
	if a.rematchJobs == nil {
		a.rematchJobs = map[string]*adminRematchJob{}
	}
	if existing := a.rematchJobs[idea.ID]; existing != nil && (existing.Status == "running" || existing.Status == "applying") {
		a.rematchMu.Unlock()
		writeError(w, http.StatusConflict, "rematch already running")
		return
	}
	a.rematchSeq++
	job := &adminRematchJob{ID: strconv.FormatInt(a.rematchSeq, 10), Status: "running", Dry: dry, IdeaUpdatedAt: idea.UpdatedAt}
	a.rematchJobs[idea.ID] = job
	res := job.responseSince(0)
	a.rematchMu.Unlock()

	go a.runAdminRematch(context.WithoutCancel(r.Context()), idea, persist, job)
	writeJSON(w, http.StatusAccepted, res)
}

func (a *API) tryApplyDryRematch(w http.ResponseWriter, idea *store.Idea) bool {
	a.rematchMu.Lock()
	if a.rematchJobs == nil {
		a.rematchJobs = map[string]*adminRematchJob{}
	}
	existing := a.rematchJobs[idea.ID]
	if existing != nil && (existing.Status == "running" || existing.Status == "applying") {
		a.rematchMu.Unlock()
		writeError(w, http.StatusConflict, "rematch already running")
		return true
	}
	if existing == nil || !existing.Dry || existing.Status != "done" || !idea.UpdatedAt.Equal(existing.IdeaUpdatedAt) {
		a.rematchMu.Unlock()
		writeError(w, http.StatusConflict, "no completed dry run to apply")
		return true
	}
	existing.Status = "applying"
	tldr := existing.TLDR
	catalogHash := existing.CatalogHash
	hive := append([]store.Match(nil), existing.Matches.Hive...)
	cncf := append([]store.CNCFMatch(nil), existing.Matches.CNCF...)
	a.rematchMu.Unlock()

	if err := a.Engine.PersistRematchResults(idea.ID, idea.UpdatedAt, tldr, hive, cncf, catalogHash); err != nil {
		a.rematchMu.Lock()
		existing.Status = "stale"
		a.rematchMu.Unlock()
		if errors.Is(err, match.ErrIdeaChanged) || errors.Is(err, match.ErrRepoChanged) || errors.Is(err, match.ErrCatalogChanged) {
			writeError(w, http.StatusConflict, "no completed dry run to apply")
			return true
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return true
	}
	a.rematchMu.Lock()
	existing.Status = "done"
	existing.Dry = false
	existing.FinishedAt = timeNow()
	a.rematchMu.Unlock()
	writeJSON(w, http.StatusOK, adminRematchResponse{
		Status:     "done",
		Dry:        false,
		TLDR:       tldr,
		Matches:    adminMatchSummary{Count: len(hive) + len(cncf), Hive: hive, CNCF: cncf},
		FinishedAt: timeNow(),
	})
	return true
}

func (a *API) runAdminRematch(parent context.Context, idea *store.Idea, persist bool, job *adminRematchJob) {
	ctx, cancel := context.WithTimeout(parent, adminRematchTimeout)
	defer cancel()
	progress := func(ev match.ProgressEvent) {
		a.rematchMu.Lock()
		defer a.rematchMu.Unlock()
		job.Next++
		job.Events = append(job.Events, adminRematchEvent{Seq: job.Next, ProgressEvent: ev})
	}
	catalogHash := a.Engine.CatalogFingerprint()
	tldr, hive, cncf, err := a.Engine.RematchIdea(ctx, idea, persist, progress)
	a.rematchMu.Lock()
	defer a.rematchMu.Unlock()
	job.FinishedAt = timeNow()
	if err != nil {
		job.Status = "error"
		job.Error = "rematch failed"
		return
	}
	job.Status = "done"
	job.TLDR = tldr
	job.CatalogHash = catalogHash
	job.Matches = adminMatchSummary{Count: len(hive) + len(cncf), Hive: hive, CNCF: cncf}
}

func (a *API) handleAdminRematchStatus(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil && r.URL.Query().Get("since") != "" {
		writeError(w, http.StatusBadRequest, "invalid since")
		return
	}
	wantJob := r.URL.Query().Get("job")
	a.rematchMu.Lock()
	job := a.rematchJobs[id]
	var res adminRematchResponse
	if job != nil && (wantJob == "" || job.ID == wantJob) {
		res = job.responseSince(since)
	}
	a.rematchMu.Unlock()
	if job == nil {
		writeError(w, http.StatusNotFound, "rematch job not found")
		return
	}
	if wantJob != "" && job.ID != wantJob {
		writeError(w, http.StatusConflict, "stale rematch job")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ideaInput is the client-writable subset of an idea.
type ideaInput struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	Visibility string `json:"visibility"`
	Status     string `json:"status"`
}

func decodeInput(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body")
		return false
	}
	if len(body) > maxRequestBody {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func storeErrStatus(err error) (int, string) {
	var ve *store.ValidationError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "idea not found"
	case errors.As(err, &ve):
		return http.StatusBadRequest, ve.Msg
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// handleListIdeas lists ideas. ?scope=public returns everyone's PUBLIC ideas;
// the default scope=mine returns the caller's own ideas (any visibility).
// Private ideas can therefore only ever surface to their author.
func (a *API) handleListIdeas(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	switch scope := r.URL.Query().Get("scope"); scope {
	case "", "mine":
		ideas, err := a.Store.ListByAuthor(id.Username)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, ideas)
	case "public":
		ideas, err := a.Store.ListPublic()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		out := make([]*store.Idea, 0, len(ideas))
		for _, idea := range ideas {
			out = append(out, a.publicIdeaView(idea, id.Username))
		}
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusBadRequest, `scope must be "mine" or "public"`)
	}
}

func (a *API) handleCreateIdea(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	var in ideaInput
	if !decodeInput(w, r, &in) {
		return
	}
	if in.Status != "" && in.Status != store.StatusDraft && in.Status != store.StatusOffered {
		writeError(w, http.StatusBadRequest, `status must be "draft" or "offered"`)
		return
	}
	idea := &store.Idea{
		Author:         id.Username,
		AuthorDisplay:  id.DisplayName,
		AuthorAvatar:   id.AvatarURL,
		AuthorProvider: store.AuthorProvider(id.Username),
		Title:          in.Title,
		Body:           in.Body,
		Visibility:     in.Visibility,
		Status:         in.Status,
	}
	if idea.AuthorDisplay == "" {
		idea.AuthorDisplay = id.Username
	}
	if err := a.Store.Create(idea); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusCreated, idea)
}

// loadAuthorized fetches an idea and enforces read access: the author
// always; anyone authenticated if the idea is public; and — new in Wave 2 —
// the owner of a repo the idea has been OFFERED to (the ideator's explicit
// reveal). Write access (mustOwn) is author-only. A private idea is
// presented to everyone else as 404, not 403, so its very existence doesn't
// leak.
func (a *API) loadAuthorized(w http.ResponseWriter, r *http.Request, mustOwn bool) *store.Idea {
	id := identity(r)
	idea, err := a.Store.Get(r.PathValue("id"))
	if err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return nil
	}
	if strings.EqualFold(idea.Author, id.Username) {
		return idea
	}
	if !mustOwn && idea.Visibility == store.VisibilityPublic {
		return idea
	}
	if !mustOwn && a.offeredToCallerRepo(idea, id.Username) {
		return idea
	}
	if idea.Visibility == store.VisibilityPrivate {
		writeError(w, http.StatusNotFound, "idea not found")
	} else {
		writeError(w, http.StatusForbidden, "not the author")
	}
	return nil
}

// offeredToCallerRepo reports whether the idea carries a standing (pending
// or accepted) offer to a repo owned by username. A declined offer no longer
// grants access.
func (a *API) offeredToCallerRepo(idea *store.Idea, username string) bool {
	for _, o := range idea.Offers {
		if o.Status == store.OfferDeclined {
			continue
		}
		if rp, err := a.Registry.Get(o.RepoID); err == nil && strings.EqualFold(rp.Owner, username) {
			return true
		}
	}
	return false
}

func (a *API) handleGetIdea(w http.ResponseWriter, r *http.Request) {
	if idea := a.loadAuthorized(w, r, false); idea != nil {
		writeJSON(w, http.StatusOK, a.publicIdeaView(idea, identity(r).Username))
	}
}

// publicIdeaView scopes an idea to a viewer who may not be its author: the
// author-only match state is dropped and pending offers are kept only for
// repos the viewer owns.
func (a *API) publicIdeaView(idea *store.Idea, viewer string) *store.Idea {
	cp := ideaForViewer(idea, viewer)
	if strings.EqualFold(cp.Author, viewer) {
		return cp
	}
	cp.Matches = nil
	cp.CNCFMatches = nil
	cp.PassedRepos = nil
	var offers []store.Offer
	for _, o := range cp.Offers {
		if o.Status != store.OfferPending {
			offers = append(offers, o)
		} else if rp, err := a.Registry.Get(o.RepoID); err == nil && strings.EqualFold(rp.Owner, viewer) {
			offers = append(offers, o)
		}
	}
	cp.Offers = offers
	return cp
}

func (a *API) handleUpdateIdea(w http.ResponseWriter, r *http.Request) {
	idea := a.loadAuthorized(w, r, true)
	if idea == nil {
		return
	}
	var in ideaInput
	if !decodeInput(w, r, &in) {
		return
	}
	if in.Status != "" && in.Status != store.StatusDraft && in.Status != store.StatusOffered {
		writeError(w, http.StatusBadRequest, `status must be "draft" or "offered"`)
		return
	}
	// Enforce the lifecycle state machine on the update path too: without
	// this, a settled idea could be reverted to draft — dropping it from the
	// public credit wall while keeping its accepted offers, launch target,
	// and issue URL (see hivecommons/dibs#153). This check runs against the
	// copy loaded above; Store.Update re-checks the status against the
	// stored record under its lock, so a concurrent accept or settle is not
	// overwritten by this stale copy (hivecommons/dibs#345).
	if in.Status != "" && in.Status != idea.Status && !store.CanTransition(idea.Status, in.Status) {
		writeError(w, http.StatusBadRequest, "cannot change status "+idea.Status+" to "+in.Status)
		return
	}
	// A settled idea's title and body are what the credit wall shows for the
	// filed issue; they stay frozen (hivecommons/dibs#344). Store.Update
	// enforces this under its lock too.
	if idea.Status == store.StatusSettled && (in.Title != idea.Title || in.Body != idea.Body) {
		writeError(w, http.StatusBadRequest, "a settled idea's title and body cannot be edited")
		return
	}
	idea.Title = in.Title
	idea.Body = in.Body
	idea.Visibility = in.Visibility
	if in.Status != "" {
		idea.Status = in.Status
	}
	if err := a.Store.Update(idea); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, idea)
}

func (a *API) handleDeleteIdea(w http.ResponseWriter, r *http.Request) {
	idea := a.loadAuthorized(w, r, true)
	if idea == nil {
		return
	}
	if err := a.Store.Delete(idea.ID); err != nil {
		status, msg := storeErrStatus(err)
		writeError(w, status, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListRepos lists repo profiles. Default: repos accepting ideas.
// ?scope=mine lists the caller's own repos (for the "For repos" page).
// ?scope=all lists everything.
func (a *API) handleListRepos(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	switch scope := r.URL.Query().Get("scope"); scope {
	case "", "accepting":
		writeJSON(w, http.StatusOK, a.Registry.List(true))
	case "all":
		writeJSON(w, http.StatusOK, a.Registry.List(false))
	case "mine":
		writeJSON(w, http.StatusOK, a.Registry.ListByOwner(id.Username))
	default:
		writeError(w, http.StatusBadRequest, `scope must be "accepting", "all", or "mine"`)
	}
}

func (a *API) handleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	repoID := r.PathValue("org") + "/" + r.PathValue("repo")
	var upd registry.OwnerUpdate
	if !decodeInput(w, r, &upd) {
		return
	}
	rp, err := a.Registry.ApplyOwnerUpdate(repoID, id.Username, upd)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "repo not found")
	case errors.Is(err, registry.ErrForbidden):
		writeError(w, http.StatusForbidden, "only the repo owner can edit its profile")
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, rp)
	}
}
