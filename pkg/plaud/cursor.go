package plaud

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/dibs/pkg/fsutil"
)

// seenRetention is how long a submitted recording id is remembered past
// the newest recording; listing only reaches relistOverlap behind it.
const seenRetention = 30 * 24 * time.Hour

// Cursor is the persisted ingest state: which Plaud recordings were already
// submitted (dedupe on Plaud id), the newest recording time (the next
// list_files starts there), and recent submission times (rate budget).
type Cursor struct {
	Newest    time.Time            `json:"newest,omitzero"`
	Seen      map[string]time.Time `json:"seen"`
	Submitted []time.Time          `json:"submitted,omitempty"`
}

// LoadCursor reads path; a missing file is an empty cursor.
func LoadCursor(path string) (*Cursor, error) {
	c := &Cursor{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("plaud: reading cursor: %w", err)
	default:
		if err := json.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("plaud: parsing cursor %s: %w", path, err)
		}
	}
	if c.Seen == nil {
		c.Seen = map[string]time.Time{}
	}
	return c, nil
}

// Save writes the cursor atomically (mode 0600), creating its directory.
func (c *Cursor) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("plaud: creating cursor directory: %w", err)
	}
	if err := fsutil.AtomicWriteJSON(path, c); err != nil {
		return fmt.Errorf("plaud: writing cursor: %w", err)
	}
	return nil
}

// markSubmitted records r as submitted at now.
func (c *Cursor) markSubmitted(r Recording, now time.Time) {
	when := r.When()
	if when.IsZero() || when.After(now) {
		when = now
	}
	c.Seen[r.ID] = when
	if when.After(c.Newest) {
		c.Newest = when
	}
	c.Submitted = append(c.Submitted, now)
}

// prune forgets submission times older than a day and seen ids well behind
// the newest recording, so the file stays small.
func (c *Cursor) prune(now time.Time) {
	kept := c.Submitted[:0]
	for _, t := range c.Submitted {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
		}
	}
	c.Submitted = kept
	for id, t := range c.Seen {
		if c.Newest.Sub(t) > seenRetention {
			delete(c.Seen, id)
		}
	}
}

// allow reports whether another submission fits the per-hour and per-day
// budgets (a budget <= 0 is unlimited).
func (c *Cursor) allow(now time.Time, perHour, perDay int) bool {
	hour, day := 0, 0
	for _, t := range c.Submitted {
		if now.Sub(t) < time.Hour {
			hour++
		}
		if now.Sub(t) < 24*time.Hour {
			day++
		}
	}
	return (perHour <= 0 || hour < perHour) && (perDay <= 0 || day < perDay)
}
