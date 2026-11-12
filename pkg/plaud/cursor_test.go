package plaud

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "plaud-cursor.json")
	c, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor(missing): %v", err)
	}
	if len(c.Seen) != 0 || !c.Newest.IsZero() {
		t.Fatalf("missing cursor = %+v, want empty", c)
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	rec := now.Add(-2 * time.Hour)
	c.markSubmitted(Recording{ID: "a", StartAt: rec}, now)
	c.markSubmitted(Recording{ID: "b"}, now)
	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cursor file mode = %v, %v; want 0600", info, err)
	}
	got, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if !got.Seen["a"].Equal(rec) || !got.Seen["b"].Equal(now) || !got.Newest.Equal(now) || len(got.Submitted) != 2 {
		t.Fatalf("reloaded cursor = %+v", got)
	}
}

func TestLoadCursorErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadCursor(dir); err == nil {
		t.Fatal("directory as cursor: want read error")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCursor(bad); err == nil {
		t.Fatal("corrupt cursor: want parse error")
	}
	null := filepath.Join(dir, "null.json")
	if err := os.WriteFile(null, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCursor(null)
	if err != nil || c.Seen == nil {
		t.Fatalf("null cursor = %+v, %v; want usable empty cursor", c, err)
	}
}

func TestCursorSaveErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Cursor{Seen: map[string]time.Time{}}
	if err := c.Save(filepath.Join(file, "sub", "c.json")); err == nil {
		t.Fatal("parent is a file: want mkdir error")
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(target); err == nil {
		t.Fatal("target is a directory: want write error")
	}
}

func TestCursorPruneAndAllow(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	c := &Cursor{
		Newest: now,
		Seen: map[string]time.Time{
			"old":    now.Add(-seenRetention - time.Hour),
			"recent": now.Add(-time.Hour),
		},
		Submitted: []time.Time{now.Add(-25 * time.Hour), now.Add(-2 * time.Hour), now.Add(-time.Minute)},
	}
	c.prune(now)
	if _, ok := c.Seen["old"]; ok {
		t.Fatal("prune kept an id far behind the newest recording")
	}
	if _, ok := c.Seen["recent"]; !ok {
		t.Fatal("prune dropped a recent id")
	}
	if len(c.Submitted) != 2 {
		t.Fatalf("Submitted after prune = %v, want the two within 24h", c.Submitted)
	}
	cases := []struct {
		perHour, perDay int
		want            bool
	}{
		{0, 0, true},
		{2, 0, true},
		{1, 0, false},
		{0, 2, false},
		{0, 3, true},
		{10, 30, true},
	}
	for _, tc := range cases {
		if got := c.allow(now, tc.perHour, tc.perDay); got != tc.want {
			t.Errorf("allow(perHour=%d, perDay=%d) = %v, want %v", tc.perHour, tc.perDay, got, tc.want)
		}
	}
}
