package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/dibs/pkg/fsutil"
)

func TestGetReadErrors(t *testing.T) {
	t.Run("indexed but file missing", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		idea := validIdea("alice")
		if err := s.Create(idea); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(s.ideaPath(idea.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(idea.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("corrupt idea file", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		idea := validIdea("alice")
		if err := s.Create(idea); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.ideaPath(idea.ID), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(idea.ID); err == nil || !strings.Contains(err.Error(), "corrupt idea") {
			t.Fatalf("err = %v, want corrupt idea error", err)
		}
	})
	t.Run("idea path unreadable", func(t *testing.T) {
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		idea := validIdea("alice")
		if err := s.Create(idea); err != nil {
			t.Fatal(err)
		}
		// Replace the idea file with a directory: ReadFile fails with a
		// non-ErrNotExist error, exercising the wrapped-read branch.
		if err := os.Remove(s.ideaPath(idea.ID)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(s.ideaPath(idea.ID), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err = s.Get(idea.ID)
		if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "reading idea") {
			t.Fatalf("err = %v, want reading idea error", err)
		}
	})
}

func TestCreatePersistError(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Removing the data dir makes fsutil.AtomicWriteJSON's CreateTemp fail, so the
	// persistLocked error propagates out of Create.
	if err := os.RemoveAll(filepath.Join(dir, "ideas")); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(validIdea("alice")); err == nil || !strings.HasPrefix(err.Error(), "store: creating temp file:") {
		t.Fatalf("err = %v, want creating temp file error", err)
	}
}

// blockIndex replaces index.json with a non-empty directory so the index
// write (rename) fails while idea-file writes still succeed.
func blockIndex(t *testing.T, s *Store) {
	t.Helper()
	if err := os.Remove(s.indexPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.indexPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.indexPath(), "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateIndexWriteFailureRollsBack(t *testing.T) {
	s, _ := newTestStore(t)
	blockIndex(t, s)
	idea := validIdea("alice")
	if err := s.Create(idea); err == nil {
		t.Fatal("Create succeeded, want index write error")
	}
	if _, err := s.Get(idea.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
	if pub, err := s.ListPublic(); err != nil || len(pub) != 0 {
		t.Fatalf("ListPublic = %v, %v; want empty", pub, err)
	}
	if _, err := os.Stat(s.ideaPath(idea.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idea file stat err = %v, want not exist", err)
	}
}

func TestMutateIndexWriteFailureRollsBack(t *testing.T) {
	s, _ := newTestStore(t)
	idea := validIdea("alice")
	if err := s.Create(idea); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.indexPath()); err != nil {
		t.Fatal(err)
	}
	blockIndex(t, s)
	if _, err := s.Mutate(idea.ID, true, func(i *Idea) error {
		i.Title = "Changed title"
		return nil
	}); err == nil {
		t.Fatal("Mutate succeeded, want index write error")
	}
	got, err := s.Get(idea.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Test idea" {
		t.Fatalf("Title = %q, want old data after failed write", got.Title)
	}
}

func TestNewAdoptsOrphanedIdeaFiles(t *testing.T) {
	s, dir := newTestStore(t)
	idea := validIdea("alice")
	if err := s.Create(idea); err != nil {
		t.Fatal(err)
	}
	orphan := validIdea("bob")
	orphan.ID = "orphan0001"
	orphan.Matches = []Match{}
	if err := fsutil.AtomicWriteJSON(s.ideaPath(orphan.ID), orphan); err != nil {
		t.Fatal(err)
	}
	// Corrupt and mismatched-ID files must be skipped.
	if err := os.WriteFile(s.ideaPath("corrupt001"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := validIdea("carol")
	other.ID = "someoneelse"
	if err := fsutil.AtomicWriteJSON(s.ideaPath("mismatch01"), other); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.ideaPath("dir0000001"), 0o755); err != nil {
		t.Fatal(err)
	}

	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(orphan.ID); err != nil {
		t.Fatalf("Get orphan: %v", err)
	}
	if _, err := s2.Get("corrupt001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt err = %v, want ErrNotFound", err)
	}
	if _, err := s2.Get("mismatch01"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatch err = %v, want ErrNotFound", err)
	}
	pub, err := s2.ListPublic()
	if err != nil || len(pub) != 2 {
		t.Fatalf("ListPublic = %d, %v; want 2", len(pub), err)
	}
	// The index was rewritten, so a further reopen sees the orphan too.
	s3, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s3.Get(orphan.ID); err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
}

func TestNewPrunesIndexRowsWithoutFiles(t *testing.T) {
	s, dir := newTestStore(t)
	keep := validIdea("alice")
	gone := validIdea("bob")
	for _, i := range []*Idea{keep, gone} {
		if err := s.Create(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(s.ideaPath(gone.ID)); err != nil {
		t.Fatal(err)
	}

	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(gone.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get removed err = %v, want ErrNotFound", err)
	}
	pub, err := s2.ListPublic()
	if err != nil || len(pub) != 1 {
		t.Fatalf("ListPublic = %d, %v; want 1", len(pub), err)
	}
	// The index was rewritten, so the row is gone from disk too.
	raw, err := os.ReadFile(s2.indexPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), gone.ID) {
		t.Fatalf("index.json still contains %s", gone.ID)
	}
}

func TestListSkipsIdeaFileRemovedAfterOpen(t *testing.T) {
	s, _ := newTestStore(t)
	keep := validIdea("alice")
	gone := validIdea("bob")
	for _, i := range []*Idea{keep, gone} {
		if err := s.Create(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(s.ideaPath(gone.ID)); err != nil {
		t.Fatal(err)
	}
	pub, err := s.ListPublic()
	if err != nil || len(pub) != 1 {
		t.Fatalf("ListPublic = %d, %v; want 1, nil", len(pub), err)
	}
}

func TestNewReconcileKeepsPrivateOrphansOutOfPublicList(t *testing.T) {
	s, dir := newTestStore(t)
	orphan := validIdea("bob")
	orphan.ID = "private001"
	orphan.Visibility = VisibilityPrivate
	orphan.Matches = []Match{}
	if err := fsutil.AtomicWriteJSON(s.ideaPath(orphan.ID), orphan); err != nil {
		t.Fatal(err)
	}
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pub, err := s2.ListPublic(); err != nil || len(pub) != 0 {
		t.Fatalf("ListPublic = %d, %v; want 0", len(pub), err)
	}
	if mine, err := s2.ListByAuthor("bob"); err != nil || len(mine) != 1 {
		t.Fatalf("ListByAuthor = %d, %v; want 1", len(mine), err)
	}
}
