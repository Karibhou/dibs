package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

func validIdea(author string) *Idea {
	return &Idea{
		Author:        author,
		AuthorDisplay: author + " Display",
		Title:         "Test idea",
		Body:          "A body in **markdown**.",
		Visibility:    VisibilityPublic,
	}
}

func TestCreateGetUpdateDelete(t *testing.T) {
	s, _ := newTestStore(t)

	idea := validIdea("alice")
	if err := s.Create(idea); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if idea.ID == "" || idea.Status != StatusDraft || idea.CreatedAt.IsZero() {
		t.Fatalf("Create did not fill defaults: %+v", idea)
	}
	if idea.Matches == nil || len(idea.Matches) != 0 {
		t.Fatalf("Matches should be empty non-nil, got %#v", idea.Matches)
	}

	got, err := s.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != idea.Title || got.Author != "alice" {
		t.Fatalf("Get mismatch: %+v", got)
	}

	got.Title = "Updated title"
	got.Author = "mallory" // must NOT be persistable via Update
	got.Status = StatusOffered
	if err := s.Update(got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, _ := s.Get(idea.ID)
	if got2.Title != "Updated title" || got2.Status != StatusOffered {
		t.Fatalf("Update lost fields: %+v", got2)
	}
	if got2.Author != "alice" {
		t.Fatalf("Update let author change: %q", got2.Author)
	}
	if !got2.UpdatedAt.After(got2.CreatedAt) {
		t.Fatalf("UpdatedAt not bumped: %+v", got2)
	}

	if err := s.Delete(idea.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(idea.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(idea.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double Delete: want ErrNotFound, got %v", err)
	}
}

func TestCreateStoresAuthorIdentityFields(t *testing.T) {
	s, _ := newTestStore(t)
	idea := validIdea("okta:00u123")
	idea.AuthorAvatar = "https://avatars.example/oidc.png"
	if err := s.Create(idea); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AuthorProvider != "okta" || got.AuthorAvatar != idea.AuthorAvatar {
		t.Fatalf("author identity fields = provider %q avatar %q", got.AuthorProvider, got.AuthorAvatar)
	}
	if AuthorProvider("alice") != "github" {
		t.Fatalf("legacy github provider inference failed")
	}
}

func TestValidation(t *testing.T) {
	s, _ := newTestStore(t)
	cases := []struct {
		name   string
		mutate func(*Idea)
	}{
		{"empty title", func(i *Idea) { i.Title = "  " }},
		{"empty body", func(i *Idea) { i.Body = "" }},
		{"long title", func(i *Idea) { i.Title = strings.Repeat("x", MaxTitleLen+1) }},
		{"long multibyte title", func(i *Idea) { i.Title = strings.Repeat("é", MaxTitleLen+1) }},
		{"long multibyte tag", func(i *Idea) { i.Tags = []string{strings.Repeat("é", MaxTagLen+1)} }},
		{"oversized body", func(i *Idea) { i.Body = strings.Repeat("x", MaxBodyBytes+1) }},
		{"bad visibility", func(i *Idea) { i.Visibility = "friends-only" }},
		{"bad status", func(i *Idea) { i.Status = "bogus" }},
	}
	for _, tc := range cases {
		idea := validIdea("alice")
		tc.mutate(idea)
		err := s.Create(idea)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: want ValidationError, got %v", tc.name, err)
		}
	}
}

func TestValidationCountsCharacters(t *testing.T) {
	s, _ := newTestStore(t)
	idea := validIdea("alice")
	idea.Title = strings.Repeat("é", MaxTitleLen)
	idea.Tags = []string{strings.Repeat("é", MaxTagLen)}
	if err := s.Create(idea); err != nil {
		t.Fatalf("at-limit multibyte idea rejected: %v", err)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	s, dir := newTestStore(t)
	idea := validIdea("alice")
	if err := s.Create(idea); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s2.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Title != idea.Title {
		t.Fatalf("reopen lost data: %+v", got)
	}
}

// TestAtomicWriteLeavesNoTempFiles verifies temp+rename hygiene: after many
// writes, the ideas dir contains only <id>.json files and the index.
func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	s, dir := newTestStore(t)
	for range 20 {
		if err := s.Create(validIdea("alice")); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "ideas"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("unexpected file: %s", e.Name())
		}
	}
	// Every file must be complete, parseable JSON (no torn writes).
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, "ideas", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if !json.Valid(raw) {
			t.Fatalf("torn/invalid JSON in %s", e.Name())
		}
	}
}

// TestConcurrentWrites exercises the mutex guard under -race.
func TestConcurrentWrites(t *testing.T) {
	s, _ := newTestStore(t)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			idea := validIdea("alice")
			if err := s.Create(idea); err != nil {
				t.Errorf("Create %d: %v", n, err)
				return
			}
			idea.Title = "updated"
			if err := s.Update(idea); err != nil {
				t.Errorf("Update %d: %v", n, err)
			}
			if _, err := s.ListByAuthor("alice"); err != nil {
				t.Errorf("List %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
	ideas, err := s.ListByAuthor("alice")
	if err != nil {
		t.Fatalf("ListByAuthor: %v", err)
	}
	if len(ideas) != 10 {
		t.Fatalf("want 10 ideas, got %d", len(ideas))
	}
}

// TestPrivateInvariantAtStoreLevel: ListPublic must NEVER return a private
// idea, regardless of status.
func TestPrivateInvariantAtStoreLevel(t *testing.T) {
	s, _ := newTestStore(t)

	priv := validIdea("alice")
	priv.Visibility = VisibilityPrivate
	priv.Status = StatusOffered
	if err := s.Create(priv); err != nil {
		t.Fatalf("Create private: %v", err)
	}
	pub := validIdea("alice")
	if err := s.Create(pub); err != nil {
		t.Fatalf("Create public: %v", err)
	}

	public, err := s.ListPublic()
	if err != nil {
		t.Fatalf("ListPublic: %v", err)
	}
	for _, i := range public {
		if i.Visibility == VisibilityPrivate || i.ID == priv.ID {
			t.Fatalf("PRIVATE IDEA LEAKED into public listing: %+v", i)
		}
	}
	if len(public) != 1 || public[0].ID != pub.ID {
		t.Fatalf("public listing wrong: %+v", public)
	}

	// The author still sees both.
	mine, err := s.ListByAuthor("alice")
	if err != nil {
		t.Fatalf("ListByAuthor: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("author should see 2 ideas, got %d", len(mine))
	}
}

// TestPing verifies Ping succeeds against a healthy data dir and fails once
// the dependency it checks (the data dir itself) is gone — this is what the
// server's /readyz handler relies on to keep unready pods out of rotation.
func TestPing(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Ping(); err != nil {
		t.Fatalf("Ping on healthy store: %v", err)
	}

	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if err := s.Ping(); err == nil {
		t.Fatal("Ping should fail once the data dir is removed")
	}
}

// TestPingWriteProbeThrottled verifies the write probe runs at most once per
// pingWriteProbeInterval: /readyz is unauthenticated, so without the cap any
// remote caller could turn readiness checks into arbitrary write I/O on the
// data volume. Within the interval a read-only dir must still pass (stat
// only); once the interval elapses the write probe runs again and fails.
func TestPingWriteProbeThrottled(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Ping(); err != nil {
		t.Fatalf("Ping on healthy store: %v", err)
	}

	// Make the dir unwritable; a throttled Ping (stat only) still passes.
	if err := os.Chmod(s.dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.dir, 0o755) })
	if err := s.Ping(); err != nil {
		t.Fatalf("Ping within probe interval should skip the write probe: %v", err)
	}

	// Expire the throttle: the write probe runs and detects the failure.
	s.pingMu.Lock()
	s.lastWriteProbe = time.Now().Add(-2 * pingWriteProbeInterval)
	s.pingMu.Unlock()
	if err := s.Ping(); err == nil {
		t.Fatal("Ping past the probe interval should re-run the write probe and fail on a read-only dir")
	}
}

func TestListByAuthorIsCaseInsensitive(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Create(validIdea("Alice")); err != nil {
		t.Fatal(err)
	}
	ideas, err := s.ListByAuthor("aLiCe")
	if err != nil || len(ideas) != 1 {
		t.Fatalf("ListByAuthor(mixed case) = %d, %v; want 1", len(ideas), err)
	}
}
