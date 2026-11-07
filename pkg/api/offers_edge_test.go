// Edge-case coverage for the wave-2 handlers: offer rejections and
// re-offers, ideator-pass validation, ownedRepo authorization, decide
// validation, the legacy-settlement failure path, the feed candidate cap,
// and nil-notification-store behavior.
package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/dibs/pkg/notify"
	"github.com/hivecommons/dibs/pkg/settle"
	"github.com/hivecommons/dibs/pkg/store"
)

func TestOfferRejectionsAndReOffer(t *testing.T) {
	now := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)

	idea := mustCreate(t, a, "bob", "Edge offer", store.VisibilityPublic, store.StatusDraft)

	rec := do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{bad json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json offer: status=%d, want 400", rec.Code)
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"org/other"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not accepting") {
		t.Fatalf("closed repo offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"not-a-repo"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "org/repo format") {
		t.Fatalf("malformed external repo: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"kubestellar/dibs"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"kubestellar/dibs"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already offered") {
		t.Fatalf("duplicate offer: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Re-offer after a decline reuses the existing offer entry.	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"Outside/Project"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already offered") {
		t.Fatalf("case-variant external offer: status=%d body=%s", rec.Code, rec.Body.String())
	}

	decidedAt := now.Add(-time.Minute)
	if _, err := a.Store.Mutate(idea.ID, false, func(i *store.Idea) error {
		i.Status = store.StatusDeclined
		i.Offers[0].Status = store.OfferDeclined
		i.Offers[0].DecidedAt = &decidedAt
		return nil
	}); err != nil {
		t.Fatalf("seed decline: %v", err)
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"kubestellar/dibs"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-offer after decline: status=%d body=%s", rec.Code, rec.Body.String())
	}
	reoffered := decodeBody[store.Idea](t, rec)
	o := reoffered.OfferTo("kubestellar/dibs")
	if len(reoffered.Offers) != 1 || o == nil || o.Status != store.OfferPending || o.DecidedAt != nil {
		t.Fatalf("re-offered idea = %+v", reoffered)
	}

	// An accepted idea cannot go back to offered.
	accepted := mustCreate(t, a, "bob", "Already accepted", store.VisibilityPublic, store.StatusAccepted)
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+accepted.ID+"/offer", `{"repoID":"kubestellar/dibs"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot offer") {
		t.Fatalf("offer accepted idea: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+accepted.ID+"/offer", `{"repoID":"outside/project"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot offer") {
		t.Fatalf("external offer accepted idea: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestExternalOfferDuplicateAndReOffer(t *testing.T) {
	now := time.Date(2026, 9, 28, 9, 30, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "bob", "External edge", store.VisibilityPublic, store.StatusDraft)

	rec := do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"outside/project"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("external offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"outside/project"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already offered") {
		t.Fatalf("duplicate external offer: status=%d body=%s", rec.Code, rec.Body.String())
	}

	decidedAt := now.Add(-time.Minute)
	if _, err := a.Store.Mutate(idea.ID, false, func(i *store.Idea) error {
		i.Status = store.StatusDeclined
		i.Offers[0].Status = store.OfferDeclined
		i.Offers[0].DecidedAt = &decidedAt
		return nil
	}); err != nil {
		t.Fatalf("seed decline: %v", err)
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"outside/project"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("external re-offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	reoffered := decodeBody[store.Idea](t, rec)
	o := reoffered.OfferTo("outside/project")
	if len(reoffered.Offers) != 1 || o == nil || o.Status != store.OfferPending || !o.External || o.DecidedAt != nil {
		t.Fatalf("external re-offered idea = %+v", reoffered)
	}
	if reoffered.TargetRepo != "outside/project" {
		t.Fatalf("TargetRepo = %q, want outside/project", reoffered.TargetRepo)
	}
}

func TestIdeatorPassValidation(t *testing.T) {
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "bob", "Pass edge", store.VisibilityPublic, store.StatusDraft)

	rec := do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/pass", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json pass: status=%d, want 400", rec.Code)
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/pass", `{"repoID":""}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "repoID is required") {
		t.Fatalf("empty repoID pass: status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The list is persisted on the idea: anything that is not org/repo
	// shaped is rejected rather than appended.
	for _, bad := range []string{"not-a-repo", "a/b/c", strings.Repeat("x", 4096)} {
		rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/pass", `{"repoID":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "org/repo format") {
			t.Fatalf("malformed repoID %q pass: status=%d body=%s", bad[:min(len(bad), 20)], rec.Code, rec.Body.String())
		}
	}
	if got := decodeBody[store.Idea](t, do(t, mux, ident("bob"), "GET", "/api/ideas/"+idea.ID, "")).PassedRepos; len(got) != 0 {
		t.Fatalf("PassedRepos after rejected passes = %+v, want empty", got)
	}
	for range 2 { // second pass must not duplicate the entry
		rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/pass", `{"repoID":"kubestellar/dibs"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("pass: status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	passed := decodeBody[store.Idea](t, rec)
	if len(passed.PassedRepos) != 1 || passed.PassedRepos[0] != "kubestellar/dibs" {
		t.Fatalf("PassedRepos = %+v, want a single entry", passed.PassedRepos)
	}
}

func TestOwnedRepoNotFoundAndForbidden(t *testing.T) {
	_, mux := newAPIFixture(t)
	rec := do(t, mux, ident("alice"), "GET", "/api/repos/no/such/feed", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown repo feed: status=%d, want 404", rec.Code)
	}
	rec = do(t, mux, ident("bob"), "GET", "/api/repos/kubestellar/dibs/feed", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner feed: status=%d, want 403", rec.Code)
	}
}

func TestDecideValidationAndPartialDecline(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)

	rec := do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json decide: status=%d, want 400", rec.Code)
	}
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"nope","decision":"accept"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown idea decide: status=%d, want 404", rec.Code)
	}

	idea := mustCreate(t, a, "bob", "Decide edge", store.VisibilityPublic, store.StatusDraft)
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+idea.ID+`","decision":"decline"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no pending offer") {
		t.Fatalf("decline without offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+idea.ID+`","decision":"shrug"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "decision must be") {
		t.Fatalf("bogus decision: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Declining one offer while another repo's offer is still pending must
	// keep the idea in offered.
	multi := mustCreate(t, a, "bob", "Two suitors", store.VisibilityPublic, store.StatusOffered)
	if _, err := a.Store.Mutate(multi.ID, false, func(i *store.Idea) error {
		i.Offers = []store.Offer{
			{RepoID: "kubestellar/dibs", Status: store.OfferPending, CreatedAt: now},
			{RepoID: "org/other", Status: store.OfferPending, CreatedAt: now},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed offers: %v", err)
	}
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+multi.ID+`","decision":"decline"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial decline: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err := a.Store.Get(multi.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != store.StatusOffered {
		t.Fatalf("status after partial decline = %s, want offered", stored.Status)
	}
	if o := stored.OfferTo("kubestellar/dibs"); o == nil || o.Status != store.OfferDeclined {
		t.Fatalf("declined offer = %+v", o)
	}
	if o := stored.OfferTo("org/other"); o == nil || o.Status != store.OfferPending {
		t.Fatalf("other offer = %+v", o)
	}

	// Accepting while another repo's offer is pending neutralizes the sibling:
	// its record is untouched, but it leaves that repo's feed and can no
	// longer be declined.
	ns, err := notify.New(t.TempDir())
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	a.Notify = ns
	race := mustCreate(t, a, "bob", "Accept race", store.VisibilityPublic, store.StatusOffered)
	if _, err := a.Store.Mutate(race.ID, false, func(i *store.Idea) error {
		i.Offers = []store.Offer{
			{RepoID: "kubestellar/dibs", Status: store.OfferPending, CreatedAt: now},
			{RepoID: "org/other", Status: store.OfferPending, CreatedAt: now},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed offers: %v", err)
	}
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+race.ID+`","decision":"accept"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept with sibling pending: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err = a.Store.Get(race.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != store.StatusAccepted || stored.TargetRepo != "kubestellar/dibs" {
		t.Fatalf("after accept: status=%s target=%s", stored.Status, stored.TargetRepo)
	}
	if o := stored.OfferTo("org/other"); o == nil || o.Status != store.OfferPending {
		t.Fatalf("sibling offer mutated: %+v", o)
	}
	offered, err := a.Store.ListOfferedTo([]string{"org/other"})
	if err != nil {
		t.Fatalf("ListOfferedTo: %v", err)
	}
	for _, i := range offered {
		if i.ID == race.ID {
			t.Fatalf("accepted idea still in sibling's offers")
		}
	}
	rec = do(t, mux, ident("charlie"), "POST", "/api/repos/org/other/decide", `{"ideaID":"`+race.ID+`","decision":"decline"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("decline after accept: status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, n := range ns.ListByUser("bob", false) {
		if n.Kind == notify.KindDeclined {
			t.Fatalf("unexpected declined notification: %+v", n)
		}
	}

	// Accepting a settled idea: unavailable without an offer, and the state
	// machine refuses even with one.
	settled := mustCreate(t, a, "bob", "Already settled", store.VisibilityPublic, store.StatusSettled)
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+settled.ID+`","decision":"accept"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not available to accept") {
		t.Fatalf("accept settled: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := a.Store.Mutate(settled.ID, false, func(i *store.Idea) error {
		i.Offers = []store.Offer{{RepoID: "kubestellar/dibs", Status: store.OfferPending, CreatedAt: now}}
		return nil
	}); err != nil {
		t.Fatalf("seed offer: %v", err)
	}
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+settled.ID+`","decision":"accept"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cannot accept") {
		t.Fatalf("accept settled with offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLegacySettleGitHubFailureKeepsAccepted(t *testing.T) {
	now := time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)
	ns, err := notify.New(t.TempDir())
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	a.Notify = ns
	a.Settler = &settle.Settler{GitHub: &settle.Fake{FailErr: errors.New("github down")}}

	idea := mustCreate(t, a, "bob", "Legacy failure", store.VisibilityPublic, store.StatusOffered)
	if _, err := a.Store.Mutate(idea.ID, false, func(i *store.Idea) error {
		i.Offers = []store.Offer{{RepoID: "kubestellar/dibs", Status: store.OfferPending, CreatedAt: now}}
		return nil
	}); err != nil {
		t.Fatalf("seed offer: %v", err)
	}
	rec := do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+idea.ID+`","decision":"accept"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy accept: status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := decodeBody[struct {
		Result  string     `json:"result"`
		Idea    store.Idea `json:"idea"`
		Warning string     `json:"warning"`
	}](t, rec)
	if out.Result != "accepted" || !strings.Contains(out.Warning, "github down") {
		t.Fatalf("legacy failure payload = %+v", out)
	}
	stored, err := a.Store.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != store.StatusAccepted || stored.IssueURL != "" {
		t.Fatalf("idea after failed legacy settle = %+v", stored)
	}
}

func TestRepoFeedCapsCandidates(t *testing.T) {
	a, mux := newAPIFixture(t)
	for i := 0; i < maxFeedCandidates+2; i++ {
		mustCreate(t, a, "bob", fmt.Sprintf("Candidate %02d", i), store.VisibilityPublic, store.StatusDraft)
	}
	rec := do(t, mux, ident("alice"), "GET", "/api/repos/kubestellar/dibs/feed", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	feed := decodeBody[struct {
		Candidates []matchView `json:"candidates"`
	}](t, rec)
	if len(feed.Candidates) != maxFeedCandidates {
		t.Fatalf("candidates = %d, want %d", len(feed.Candidates), maxFeedCandidates)
	}
}

func TestIdeaMatchesWithoutEngine(t *testing.T) {
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "bob", "No engine", store.VisibilityPublic, store.StatusDraft)
	rec := do(t, mux, ident("bob"), "GET", "/api/ideas/"+idea.ID+"/matches", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("matches without engine: status=%d, want 503", rec.Code)
	}
}

func TestWave2IdeatorRoutesRejectNonAuthor(t *testing.T) {
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "bob", "Not yours", store.VisibilityPublic, store.StatusDraft)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/ideas/" + idea.ID + "/matches", ""},
		{"POST", "/api/ideas/" + idea.ID + "/matches/seen", ""},
		{"POST", "/api/ideas/" + idea.ID + "/offer", `{"repoID":"kubestellar/dibs"}`},
		{"POST", "/api/ideas/" + idea.ID + "/pass", `{"repoID":"kubestellar/dibs"}`},
	} {
		rec := do(t, mux, ident("mallory"), tc.method, tc.path, tc.body)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s %s as non-author: status=%d, want an error", tc.method, tc.path, rec.Code)
		}
	}
}

func TestNotificationsWithoutStore(t *testing.T) {
	_, mux := newAPIFixture(t) // a.Notify is nil
	rec := do(t, mux, ident("bob"), "GET", "/api/notifications", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list without store: status=%d", rec.Code)
	}
	out := decodeBody[struct {
		Notifications []notify.Notification `json:"notifications"`
		Unread        int                   `json:"unread"`
	}](t, rec)
	if len(out.Notifications) != 0 || out.Unread != 0 {
		t.Fatalf("nil-store notifications = %+v", out)
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/notifications/read", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json mark read: status=%d, want 400", rec.Code)
	}
	rec = do(t, mux, ident("bob"), "POST", "/api/notifications/read", `{"all":true}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("mark read without store: status=%d, want 204", rec.Code)
	}
}

// A hive-managed repo typed in a different case must still take the hive
// path (owner acceptance, AcceptingIdeas gate, canonical RepoID) rather
// than falling through to the external-target flow.
func TestOfferCaseFoldedRepoStaysHiveManaged(t *testing.T) {
	a, mux := newAPIFixture(t)

	idea := mustCreate(t, a, "bob", "Case folded offer", store.VisibilityPublic, store.StatusDraft)
	rec := do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"Org/Other"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not accepting") {
		t.Fatalf("closed repo in different case must still be gated: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"KubeStellar/Dibs"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("case-folded hive offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	offered := decodeBody[store.Idea](t, rec)
	o := offered.OfferTo("kubestellar/dibs")
	if o == nil || o.External || offered.TargetRepo != "" {
		t.Fatalf("case-folded offer must be a pending hive offer under the canonical RepoID, got %+v", offered)
	}
}

func TestAcceptAfterDeclineRejected(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)

	for _, vis := range []string{store.VisibilityPublic, store.VisibilityPrivate} {
		idea := mustCreate(t, a, "bob", "Declined then accept "+vis, vis, store.StatusOffered)
		if _, err := a.Store.Mutate(idea.ID, false, func(i *store.Idea) error {
			i.Offers = []store.Offer{
				{RepoID: "kubestellar/dibs", Status: store.OfferDeclined, CreatedAt: now, DecidedAt: &now},
				{RepoID: "org/other", Status: store.OfferPending, CreatedAt: now},
			}
			return nil
		}); err != nil {
			t.Fatalf("seed offers: %v", err)
		}
		rec := do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+idea.ID+`","decision":"accept"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no pending offer") {
			t.Fatalf("%s: accept after decline: status=%d body=%s", vis, rec.Code, rec.Body.String())
		}
		stored, err := a.Store.Get(idea.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if o := stored.OfferTo("kubestellar/dibs"); stored.Status != store.StatusOffered || o == nil || o.Status != store.OfferDeclined {
			t.Fatalf("%s: state changed: %+v", vis, stored)
		}
	}
}

func TestOfferedToCallerRepoIsCaseInsensitive(t *testing.T) {
	a, _ := newAPIFixture(t)
	idea := &store.Idea{Offers: []store.Offer{{RepoID: "kubestellar/dibs", Status: store.OfferPending}}}
	if !a.offeredToCallerRepo(idea, "ALICE") {
		t.Fatal("mixed-case owner should see the offered idea")
	}
	if a.offeredToCallerRepo(idea, "mallory") {
		t.Fatal("non-owner should not see the offered idea")
	}
}

// TestDeclinedOfferDoesNotGrantPrivateRead pins that only standing offers let
// a repo owner read a private idea via GET /api/ideas/{id}.
func TestDeclinedOfferDoesNotGrantPrivateRead(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)
	for _, tc := range []struct {
		status string
		want   int
	}{
		{store.OfferDeclined, http.StatusNotFound},
		{store.OfferPending, http.StatusOK},
		{store.OfferAccepted, http.StatusOK},
	} {
		idea := mustCreate(t, a, "bob", "Private "+tc.status, store.VisibilityPrivate, store.StatusOffered)
		if _, err := a.Store.Mutate(idea.ID, false, func(i *store.Idea) error {
			i.Offers = []store.Offer{{RepoID: "kubestellar/dibs", Status: tc.status, CreatedAt: now}}
			return nil
		}); err != nil {
			t.Fatalf("seed offers: %v", err)
		}
		rec := do(t, mux, ident("alice"), "GET", "/api/ideas/"+idea.ID, "")
		if rec.Code != tc.want {
			t.Fatalf("%s offer: status=%d, want %d", tc.status, rec.Code, tc.want)
		}
	}
}

// TestDecidePassRejectsPendingOffer pins that pass cannot silently succeed on
// a pending offer, which the repo feed would keep resurfacing.
func TestDecidePassRejectsPendingOffer(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "bob", "Offered pass", store.VisibilityPublic, store.StatusOffered)
	if _, err := a.Store.Mutate(idea.ID, false, func(i *store.Idea) error {
		i.Offers = []store.Offer{{RepoID: "kubestellar/dibs", Status: store.OfferPending, CreatedAt: now}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+idea.ID+`","decision":"pass"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "decline") {
		t.Fatalf("pass on pending offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", `{"ideaID":"`+idea.ID+`","decision":"decline"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("decline: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAcceptRefusesIdeaAimedAtExternalRepo(t *testing.T) {
	now := time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC)
	withFixedNow(t, now)
	a, mux := newAPIFixture(t)
	idea := mustCreate(t, a, "bob", "Aimed externally", store.VisibilityPublic, store.StatusDraft)

	rec := do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"outside/project"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("external offer: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = do(t, mux, ident("alice"), "GET", "/api/repos/kubestellar/dibs/feed", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	feed := decodeBody[struct {
		Candidates []matchView `json:"candidates"`
	}](t, rec)
	for _, c := range feed.Candidates {
		if c.Idea != nil && c.Idea.ID == idea.ID {
			t.Fatalf("aimed idea listed as candidate: %+v", c.Idea)
		}
	}

	decide := `{"ideaID":"` + idea.ID + `","decision":"accept"}`
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", decide)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already aimed at outside/project") {
		t.Fatalf("candidate accept: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err := a.Store.Get(idea.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.TargetRepo != "outside/project" || stored.Status != store.StatusOffered {
		t.Fatalf("state changed: %+v", stored)
	}

	// The ideator's explicit offer to the registered repo is a re-aim.
	rec = do(t, mux, ident("bob"), "POST", "/api/ideas/"+idea.ID+"/offer", `{"repoID":"kubestellar/dibs"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("offer to registered repo: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, mux, ident("alice"), "POST", "/api/repos/kubestellar/dibs/decide", decide)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept pending offer: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err = a.Store.Get(idea.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.TargetRepo != "kubestellar/dibs" || stored.Status != store.StatusAccepted {
		t.Fatalf("after re-aim accept: %+v", stored)
	}
}
