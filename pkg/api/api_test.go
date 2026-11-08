package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/dibs/pkg/auth"
	"github.com/hivecommons/dibs/pkg/match"
	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/store"
)

// newAPIFixture builds an API over a temp-dir store and registry seeded with
// two repos: alice owns kubestellar/dibs (accepting), charlie owns org/other
// (not accepting).
func newAPIFixture(t *testing.T) (*API, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	reg, err := registry.New(dir)
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	if err := reg.Merge([]registry.RepoProfile{
		{RepoID: "kubestellar/dibs", HiveID: "hive-ks", Owner: "alice", AcceptingIdeas: true,
			Description: "ideas exchange", Topics: []string{"kubernetes"}},
		{RepoID: "org/other", HiveID: "hive-o", Owner: "charlie", AcceptingIdeas: false,
			Description: "widgets", Topics: []string{"widgets"}},
	}); err != nil {
		t.Fatalf("registry.Merge: %v", err)
	}
	a := &API{Store: st, Registry: reg}
	mux := http.NewServeMux()
	a.Register(mux, "")
	return a, mux
}

func ident(username string) *auth.Identity {
	return &auth.Identity{Username: username, DisplayName: strings.ToUpper(username[:1]) + username[1:]}
}

// do issues a request as id (nil for anonymous) and returns the recorder.
func do(t *testing.T, mux *http.ServeMux, id *auth.Identity, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if id != nil {
		req = req.WithContext(auth.WithIdentity(req.Context(), id))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func mustCreate(t *testing.T, a *API, author, title, visibility, status string) *store.Idea {
	t.Helper()
	idea := &store.Idea{Author: author, AuthorDisplay: author, Title: title,
		Body: "body of " + title, Visibility: visibility, Status: status}
	if err := a.Store.Create(idea); err != nil {
		t.Fatalf("Create(%s): %v", title, err)
	}
	return idea
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Fatalf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Fatalf("firstNonEmpty() = %q, want empty", got)
	}
}

func TestIdeaForViewerScrubsSeenAt(t *testing.T) {
	seen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	idea := &store.Idea{Author: "alice", SuggestionsSeenAt: seen}
	if got := ideaForViewer(idea, "alice"); !got.SuggestionsSeenAt.Equal(seen) {
		t.Fatalf("author view lost SuggestionsSeenAt")
	}
	if got := ideaForViewer(idea, "bob"); !got.SuggestionsSeenAt.IsZero() {
		t.Fatalf("non-author view kept SuggestionsSeenAt")
	}
	if !idea.SuggestionsSeenAt.Equal(seen) {
		t.Fatalf("ideaForViewer mutated the original")
	}
}

func TestSummarizeMatchesCapsTopAtThree(t *testing.T) {
	matches := []store.Match{
		{RepoID: "a/a", Score: 4}, {RepoID: "b/b", Score: 3},
		{RepoID: "c/c", Score: 2}, {RepoID: "d/d", Score: 1},
	}
	cncf := []store.CNCFMatch{{Name: "proj"}}
	out := summarizeMatches(matches, cncf)
	if out.Count != 5 {
		t.Fatalf("Count = %d, want 5", out.Count)
	}
	if len(out.Top) != 3 || out.Top[0].RepoID != "a/a" || out.Top[2].RepoID != "c/c" {
		t.Fatalf("Top = %+v, want first three hive matches", out.Top)
	}
}

func TestResponseSinceFiltersEvents(t *testing.T) {
	job := &adminRematchJob{ID: "7", Status: "running", Next: 3, Events: []adminRematchEvent{
		{Seq: 1}, {Seq: 2}, {Seq: 3},
	}}
	res := job.responseSince(2)
	if len(res.Events) != 1 || res.Events[0].Seq != 3 {
		t.Fatalf("events since 2 = %+v, want only seq 3", res.Events)
	}
	if res.Next != 3 || res.JobID != "7" || res.Status != "running" {
		t.Fatalf("response = %+v", res)
	}
}

func TestStoreErrStatus(t *testing.T) {
	if code, msg := storeErrStatus(store.ErrNotFound); code != http.StatusNotFound || msg != "idea not found" {
		t.Fatalf("ErrNotFound -> %d %q", code, msg)
	}
	if code, msg := storeErrStatus(&store.ValidationError{Msg: "title is required"}); code != http.StatusBadRequest || msg != "title is required" {
		t.Fatalf("ValidationError -> %d %q", code, msg)
	}
	if code, msg := storeErrStatus(http.ErrBodyNotAllowed); code != http.StatusInternalServerError || msg != "internal error" {
		t.Fatalf("unknown error -> %d %q", code, msg)
	}
}

func TestHandleMe(t *testing.T) {
	_, mux := newAPIFixture(t)
	rec := do(t, mux, ident("alice"), "GET", "/api/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeBody[auth.Identity](t, rec)
	if got.Username != "alice" || got.DisplayName != "Alice" {
		t.Fatalf("identity = %+v", got)
	}
}

func TestAdminEndpointsRejectNonAdmins(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	_, mux := newAPIFixture(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/admin/ideas"},
		{"POST", "/api/admin/ideas/x/rematch"},
		{"GET", "/api/admin/ideas/x/rematch"},
	} {
		rec := do(t, mux, ident("alice"), tc.method, tc.path, "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as alice: status = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	// No identity at all is also forbidden.
	rec := do(t, mux, nil, "GET", "/api/admin/ideas", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous admin list: status = %d, want 403", rec.Code)
	}
}

func TestAdminIdeasListsEveryAuthor(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	a, mux := newAPIFixture(t)
	mine := mustCreate(t, a, "alice", "Idea one", store.VisibilityPrivate, store.StatusDraft)
	mine.Matches = nil
	theirs := mustCreate(t, a, "okta:00u1", "Idea two", store.VisibilityPublic, store.StatusOffered)
	_ = theirs

	rec := do(t, mux, ident("root"), "GET", "/api/admin/ideas", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBody[[]adminIdea](t, rec)
	if len(got) != 2 {
		t.Fatalf("got %d ideas, want 2 (admin sees private ideas too)", len(got))
	}
	byAuthor := map[string]adminIdea{}
	for _, it := range got {
		byAuthor[it.Author] = it
	}
	if byAuthor["alice"].AuthorProvider != "github" {
		t.Fatalf("alice provider = %q, want github fallback", byAuthor["alice"].AuthorProvider)
	}
	if byAuthor["okta:00u1"].AuthorProvider != "okta" {
		t.Fatalf("okta provider = %q, want okta", byAuthor["okta:00u1"].AuthorProvider)
	}
}

func TestAdminRematchWithoutEngine(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "alice", "Engineless", store.VisibilityPublic, store.StatusDraft)
	rec := do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestAdminRematchUnknownIdeaAndStaleApply(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	a, mux := newAPIFixture(t)
	a.Engine = &match.Engine{Store: a.Store.(*store.Store), Registry: a.Registry.(*registry.Registry)}

	rec := do(t, mux, ident("root"), "POST", "/api/admin/ideas/nope/rematch", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown idea: status = %d, want 404", rec.Code)
	}

	// Applying (non-dry) with no completed dry run must refuse.
	idea := mustCreate(t, a, "alice", "Rematchable", store.VisibilityPublic, store.StatusDraft)
	rec = do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("apply without dry run: status = %d, want 409", rec.Code)
	}
}

// gatedMatcher wraps a Matcher and blocks RematchIdea until release is
// closed, so tests can assert against a job that is deterministically
// still running.
type gatedMatcher struct {
	Matcher
	started chan struct{} // closed once RematchIdea has been entered
	release chan struct{} // close to let RematchIdea proceed
}

func (g *gatedMatcher) RematchIdea(ctx context.Context, idea *store.Idea, persist bool, progress match.ProgressFunc) (string, []store.Match, []match.CNCFMatch, error) {
	close(g.started)
	select {
	case <-g.release:
	case <-ctx.Done():
		return "", nil, nil, ctx.Err()
	}
	return g.Matcher.RematchIdea(ctx, idea, persist, progress)
}

// TestAdminRematchDoubleStartConflicts pins the busy branches: while a dry
// rematch job is running, both a second dry start and a non-dry apply must
// return 409 rather than starting another job or applying anything.
func TestAdminRematchDoubleStartConflicts(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	a, mux := newAPIFixture(t)
	gate := &gatedMatcher{
		Matcher: &match.Engine{Store: a.Store.(*store.Store), Registry: a.Registry.(*registry.Registry)},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	a.Engine = gate
	idea := mustCreate(t, a, "alice", "Kubernetes idea marketplace", store.VisibilityPublic, store.StatusDraft)

	rec := do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch?dry=1", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start dry run: %d body=%s", rec.Code, rec.Body.String())
	}
	<-gate.started

	rec = do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("apply while running: %d, want 409", rec.Code)
	}
	rec = do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch?dry=1", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("dry start while running: %d, want 409", rec.Code)
	}

	close(gate.release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = do(t, mux, ident("root"), "GET", "/api/admin/ideas/"+idea.ID+"/rematch", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("poll status: %d body=%s", rec.Code, rec.Body.String())
		}
		status := decodeBody[adminRematchResponse](t, rec)
		if status.Status == "done" {
			break
		}
		if status.Status == "error" || time.Now().After(deadline) {
			t.Fatalf("rematch did not finish cleanly: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAdminRematchDryRunThenApply drives the full async lifecycle: start a
// dry run (202), poll status until done, then apply the reviewed results
// (200) and verify they were persisted.
func TestAdminRematchDryRunThenApply(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	a, mux := newAPIFixture(t)
	a.Engine = &match.Engine{Store: a.Store.(*store.Store), Registry: a.Registry.(*registry.Registry)}
	idea := mustCreate(t, a, "alice", "Kubernetes idea marketplace", store.VisibilityPublic, store.StatusDraft)

	rec := do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch?dry=1", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start dry run: %d body=%s", rec.Code, rec.Body.String())
	}
	started := decodeBody[adminRematchResponse](t, rec)
	if started.JobID == "" || started.Status != "running" || !started.Dry {
		t.Fatalf("start payload = %+v", started)
	}

	var status adminRematchResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = do(t, mux, ident("root"), "GET", "/api/admin/ideas/"+idea.ID+"/rematch?job="+started.JobID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("poll status: %d body=%s", rec.Code, rec.Body.String())
		}
		status = decodeBody[adminRematchResponse](t, rec)
		if status.Status == "done" || status.Status == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rematch never finished: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.Status != "done" || status.TLDR == "" {
		t.Fatalf("dry run result = %+v", status)
	}

	rec = do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d body=%s", rec.Code, rec.Body.String())
	}
	applied := decodeBody[adminRematchResponse](t, rec)
	if applied.Status != "done" || applied.Dry {
		t.Fatalf("apply payload = %+v", applied)
	}
	got, err := a.Store.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get after apply: %v", err)
	}
	if got.TLDR != status.TLDR {
		t.Fatalf("persisted TLDR %q, want dry-run TLDR %q", got.TLDR, status.TLDR)
	}
	// A second apply with the dry run consumed must refuse again.
	rec = do(t, mux, ident("root"), "POST", "/api/admin/ideas/"+idea.ID+"/rematch", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-apply: %d, want 409", rec.Code)
	}
}

func TestAdminRematchStatusErrors(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	_, mux := newAPIFixture(t)
	rec := do(t, mux, ident("root"), "GET", "/api/admin/ideas/nope/rematch", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: status = %d, want 404", rec.Code)
	}
	rec = do(t, mux, ident("root"), "GET", "/api/admin/ideas/nope/rematch?since=abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since: status = %d, want 400", rec.Code)
	}
}

func TestAdminRematchStatusStaleJobID(t *testing.T) {
	t.Setenv("DIBS_ADMINS", "root")
	a, mux := newAPIFixture(t)
	a.rematchJobs = map[string]*adminRematchJob{"idea1": {ID: "1", Status: "done"}}
	rec := do(t, mux, ident("root"), "GET", "/api/admin/ideas/idea1/rematch?job=2", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale job: status = %d, want 409", rec.Code)
	}
	rec = do(t, mux, ident("root"), "GET", "/api/admin/ideas/idea1/rematch?job=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("matching job: status = %d, want 200", rec.Code)
	}
	if got := decodeBody[adminRematchResponse](t, rec); got.Status != "done" {
		t.Fatalf("status payload = %+v", got)
	}
}

func TestDecodeInputRejectsBadBodies(t *testing.T) {
	_, mux := newAPIFixture(t)
	rec := do(t, mux, ident("alice"), "POST", "/api/ideas", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON: status = %d, want 400", rec.Code)
	}
	huge := `{"title":"t","body":"` + strings.Repeat("x", maxRequestBody) + `"}`
	rec = do(t, mux, ident("alice"), "POST", "/api/ideas", huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status = %d, want 413", rec.Code)
	}
}

func TestCreateIdea(t *testing.T) {
	_, mux := newAPIFixture(t)
	rec := do(t, mux, &auth.Identity{Username: "bob"}, "POST", "/api/ideas",
		`{"title":"Shiny","body":"details","visibility":"public"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBody[store.Idea](t, rec)
	if got.ID == "" || got.Author != "bob" || got.Status != store.StatusDraft {
		t.Fatalf("created idea = %+v", got)
	}
	if got.AuthorDisplay != "bob" {
		t.Fatalf("AuthorDisplay = %q, want username fallback", got.AuthorDisplay)
	}

	rec = do(t, mux, ident("bob"), "POST", "/api/ideas",
		`{"title":"Bad","body":"b","visibility":"public","status":"settled"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("client-set settled status: %d, want 400", rec.Code)
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas", `{"body":"b","visibility":"public"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing title: %d, want 400", rec.Code)
	}
	if got := decodeBody[map[string]string](t, rec); got["error"] != "title is required" {
		t.Fatalf("validation error surfaced as %q", got["error"])
	}
}

func TestListIdeasScopes(t *testing.T) {
	a, mux := newAPIFixture(t)
	mustCreate(t, a, "alice", "Private of alice", store.VisibilityPrivate, store.StatusDraft)
	pub := mustCreate(t, a, "alice", "Public of alice", store.VisibilityPublic, store.StatusDraft)
	if _, err := a.Store.Mutate(pub.ID, false, func(i *store.Idea) error {
		i.SuggestionsSeenAt = time.Now().UTC()
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}

	rec := do(t, mux, ident("alice"), "GET", "/api/ideas", "")
	if got := decodeBody[[]store.Idea](t, rec); len(got) != 2 {
		t.Fatalf("scope=mine for alice: %d ideas, want 2", len(got))
	}
	rec = do(t, mux, ident("bob"), "GET", "/api/ideas", "")
	if got := decodeBody[[]store.Idea](t, rec); len(got) != 0 {
		t.Fatalf("scope=mine for bob: %d ideas, want 0", len(got))
	}
	rec = do(t, mux, ident("bob"), "GET", "/api/ideas?scope=public", "")
	got := decodeBody[[]store.Idea](t, rec)
	if len(got) != 1 || got[0].Title != "Public of alice" {
		t.Fatalf("scope=public: %+v", got)
	}
	if !got[0].SuggestionsSeenAt.IsZero() {
		t.Fatalf("public listing leaked SuggestionsSeenAt to a non-author")
	}
	rec = do(t, mux, ident("bob"), "GET", "/api/ideas?scope=weird", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scope: status = %d, want 400", rec.Code)
	}
}

func TestGetIdeaAccessControl(t *testing.T) {
	a, mux := newAPIFixture(t)
	private := mustCreate(t, a, "alice", "Secret plan", store.VisibilityPrivate, store.StatusDraft)
	public := mustCreate(t, a, "alice", "Open plan", store.VisibilityPublic, store.StatusDraft)

	if rec := do(t, mux, ident("alice"), "GET", "/api/ideas/"+private.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("author on private: %d", rec.Code)
	}
	// A private idea must 404 (not 403) for strangers so it doesn't leak.
	if rec := do(t, mux, ident("bob"), "GET", "/api/ideas/"+private.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger on private: %d, want 404", rec.Code)
	}
	if rec := do(t, mux, ident("bob"), "GET", "/api/ideas/"+public.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("stranger on public: %d, want 200", rec.Code)
	}

	// Offering a private idea to alice's repo reveals it to the repo owner —
	// but only to that owner.
	if _, err := a.Store.Mutate(private.ID, false, func(i *store.Idea) error {
		i.Offers = append(i.Offers, store.Offer{RepoID: "kubestellar/dibs", Status: "pending", CreatedAt: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	// alice authored it; use a second author to prove the offer path.
	offered := mustCreate(t, a, "bob", "For alice repo", store.VisibilityPrivate, store.StatusOffered)
	if _, err := a.Store.Mutate(offered.ID, false, func(i *store.Idea) error {
		i.Offers = append(i.Offers, store.Offer{RepoID: "kubestellar/dibs", Status: "pending", CreatedAt: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if rec := do(t, mux, ident("alice"), "GET", "/api/ideas/"+offered.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("offer-target repo owner: %d, want 200", rec.Code)
	}
	if rec := do(t, mux, ident("charlie"), "GET", "/api/ideas/"+offered.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unrelated repo owner on private: %d, want 404", rec.Code)
	}
}

func TestGetIdeaScopesAuthorOnlyState(t *testing.T) {
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "bob", "Offered plan", store.VisibilityPublic, store.StatusOffered)
	if _, err := a.Store.Mutate(idea.ID, false, func(i *store.Idea) error {
		i.Matches = []store.Match{{RepoID: "match/repo", Score: 3}}
		i.CNCFMatches = []store.CNCFMatch{{Name: "cncfproj"}}
		i.PassedRepos = []string{"passed/repo"}
		i.Offers = append(i.Offers,
			store.Offer{RepoID: "kubestellar/dibs", Status: store.OfferPending, CreatedAt: time.Now().UTC()},
			store.Offer{RepoID: "done/repo", Status: store.OfferAccepted, CreatedAt: time.Now().UTC()})
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	for _, path := range []string{"/api/ideas/" + idea.ID, "/api/ideas?scope=public"} {
		body := do(t, mux, ident("charlie"), "GET", path, "").Body.String()
		for _, leak := range []string{"kubestellar/dibs", "match/repo", "cncfproj", "passed/repo"} {
			if strings.Contains(body, leak) {
				t.Fatalf("%s leaked %q to a non-author: %s", path, leak, body)
			}
		}
		if !strings.Contains(body, "done/repo") {
			t.Fatalf("%s dropped the accepted offer: %s", path, body)
		}
	}
	// The author and the owner of the offered repo keep their view.
	if body := do(t, mux, ident("bob"), "GET", "/api/ideas/"+idea.ID, "").Body.String(); !strings.Contains(body, "match/repo") || !strings.Contains(body, "kubestellar/dibs") {
		t.Fatalf("author view lost state: %s", body)
	}
	if body := do(t, mux, ident("alice"), "GET", "/api/ideas/"+idea.ID, "").Body.String(); !strings.Contains(body, "kubestellar/dibs") || strings.Contains(body, "match/repo") {
		t.Fatalf("repo owner view wrong: %s", body)
	}
}

func TestUpdateIdea(t *testing.T) {
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "alice", "Original", store.VisibilityPublic, store.StatusDraft)

	rec := do(t, mux, ident("alice"), "PUT", "/api/ideas/"+idea.ID,
		`{"title":"Renamed","body":"new body","visibility":"public","status":"offered"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("author update: %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBody[store.Idea](t, rec)
	if got.Title != "Renamed" || got.Status != store.StatusOffered {
		t.Fatalf("updated idea = %+v", got)
	}

	// Write access is author-only even on public ideas: 403, not 404.
	rec = do(t, mux, ident("bob"), "PUT", "/api/ideas/"+idea.ID,
		`{"title":"Hijack","body":"b","visibility":"public"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-author update on public: %d, want 403", rec.Code)
	}
	rec = do(t, mux, ident("alice"), "PUT", "/api/ideas/"+idea.ID,
		`{"title":"T","body":"b","visibility":"public","status":"accepted"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("client-set accepted status: %d, want 400", rec.Code)
	}
}

// TestUpdateIdeaStatusTransitions pins the state machine on the PUT path:
// a client-supplied status must satisfy store.CanTransition from the idea's
// current status — in particular, a terminal settled idea can never be
// reverted to draft/offered (hivecommons/dibs#153).
func TestUpdateIdeaStatusTransitions(t *testing.T) {
	a, mux := newAPIFixture(t)

	settled := mustCreate(t, a, "alice", "Done deal", store.VisibilityPublic, store.StatusDraft)
	if _, err := a.Store.Mutate(settled.ID, true, func(i *store.Idea) error {
		i.Status = store.StatusSettled
		i.TargetRepo = "kubestellar/dibs"
		i.IssueURL = "https://github.com/kubestellar/dibs/issues/1"
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	for _, status := range []string{store.StatusDraft, store.StatusOffered} {
		rec := do(t, mux, ident("alice"), "PUT", "/api/ideas/"+settled.ID,
			`{"title":"Done deal","body":"body of Done deal","visibility":"public","status":"`+status+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("settled → %s: %d, want 400 (body=%s)", status, rec.Code, rec.Body.String())
		}
	}
	if got, err := a.Store.Get(settled.ID); err != nil || got.Status != store.StatusSettled {
		t.Fatalf("settled idea after rejected PUTs = %+v, %v", got, err)
	}

	offered := mustCreate(t, a, "alice", "In play", store.VisibilityPublic, store.StatusOffered)
	rec := do(t, mux, ident("alice"), "PUT", "/api/ideas/"+offered.ID,
		`{"title":"In play","body":"body of In play","visibility":"public","status":"draft"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("offered → draft: %d, want 400", rec.Code)
	}
	// Same-status writes and legal transitions still pass.
	rec = do(t, mux, ident("alice"), "PUT", "/api/ideas/"+offered.ID,
		`{"title":"In play","body":"body of In play","visibility":"public","status":"offered"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("offered → offered no-op: %d body=%s", rec.Code, rec.Body.String())
	}

	declined := mustCreate(t, a, "alice", "Second wind", store.VisibilityPublic, store.StatusDraft)
	if _, err := a.Store.Mutate(declined.ID, true, func(i *store.Idea) error {
		i.Status = store.StatusDeclined
		return nil
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	rec = do(t, mux, ident("alice"), "PUT", "/api/ideas/"+declined.ID,
		`{"title":"Second wind","body":"body of Second wind","visibility":"public","status":"offered"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("declined → offered re-offer: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteIdea(t *testing.T) {
	a, mux := newAPIFixture(t)
	private := mustCreate(t, a, "alice", "Doomed", store.VisibilityPrivate, store.StatusDraft)

	if rec := do(t, mux, ident("bob"), "DELETE", "/api/ideas/"+private.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger delete on private: %d, want 404", rec.Code)
	}
	if rec := do(t, mux, ident("alice"), "DELETE", "/api/ideas/"+private.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("author delete: %d, want 204", rec.Code)
	}
	if rec := do(t, mux, ident("alice"), "GET", "/api/ideas/"+private.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted idea still readable: %d", rec.Code)
	}
}

func TestListReposScopes(t *testing.T) {
	_, mux := newAPIFixture(t)
	rec := do(t, mux, ident("bob"), "GET", "/api/repos", "")
	if got := decodeBody[[]registry.RepoProfile](t, rec); len(got) != 1 || got[0].RepoID != "kubestellar/dibs" {
		t.Fatalf("default scope (accepting): %+v", got)
	}
	rec = do(t, mux, ident("bob"), "GET", "/api/repos?scope=all", "")
	if got := decodeBody[[]registry.RepoProfile](t, rec); len(got) != 2 {
		t.Fatalf("scope=all: %d repos, want 2", len(got))
	}
	rec = do(t, mux, ident("charlie"), "GET", "/api/repos?scope=mine", "")
	if got := decodeBody[[]registry.RepoProfile](t, rec); len(got) != 1 || got[0].RepoID != "org/other" {
		t.Fatalf("scope=mine for charlie: %+v", got)
	}
	if rec := do(t, mux, ident("bob"), "GET", "/api/repos?scope=nope", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scope: %d, want 400", rec.Code)
	}
}

func TestUpdateRepo(t *testing.T) {
	_, mux := newAPIFixture(t)
	rec := do(t, mux, ident("alice"), "PUT", "/api/repos/kubestellar/dibs",
		`{"acceptingIdeas":false,"appetite":"CLI polish"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner update: %d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBody[registry.RepoProfile](t, rec)
	if got.AcceptingIdeas || got.Appetite != "CLI polish" {
		t.Fatalf("updated profile = %+v", got)
	}
	if rec := do(t, mux, ident("bob"), "PUT", "/api/repos/kubestellar/dibs", `{"appetite":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner update: %d, want 403", rec.Code)
	}
	if rec := do(t, mux, ident("alice"), "PUT", "/api/repos/no/repo", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown repo: %d, want 404", rec.Code)
	}
}

func TestGetIdeaAuthorMatchIsCaseInsensitive(t *testing.T) {
	a, mux := newAPIFixture(t)
	private := mustCreate(t, a, "Alice", "Secret plan", store.VisibilityPrivate, store.StatusDraft)
	if rec := do(t, mux, ident("aLICE"), "GET", "/api/ideas/"+private.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("author with different login case: %d, want 200", rec.Code)
	}
}
