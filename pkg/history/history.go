// Package history backfills public GitHub activity for hive-managed repos and
// persists the aggregate daily counts that enrich Dibs repo index charts.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/dibs/pkg/envutil"
	"github.com/hivecommons/dibs/pkg/fsutil"
	"github.com/hivecommons/dibs/pkg/ghrate"
	"github.com/hivecommons/dibs/pkg/indexformula"
	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/settle"
)

const (
	defaultBaseURL = "https://api.github.com"
	windowDays     = 30
	maxPullPages   = 3
	perPage        = 100
	requestTimeout = 10 * time.Second
	maxBodyBytes   = 4 << 20
	maxConcurrent  = 4

	BackfillVersion = 3

	// EnvClankerMarkers optionally overrides the comma-separated, case-insensitive
	// markers matched against PR branch, labels, body, and author login.
	EnvClankerMarkers = "DIBS_CLANKER_MARKERS"
)

// DayActivity is one UTC calendar day's public GitHub activity for a repo.
type DayActivity struct {
	Date                 string `json:"date"`
	RegularIssuesCreated int    `json:"regularIssuesCreated"`
	MergedPRs            int    `json:"mergedPRs"` // regular (non-ClankeR) PR merges
	ClankerPRsCreated    int    `json:"clankerPRsCreated"`
	ClankerPRsMerged     int    `json:"clankerPRsMerged"`
	IdeasFiled           int    `json:"ideasFiled"`
	// Bar-series counts: human-filed Dibs idea issues count as human issues
	// because Dibs opens a prefilled GitHub URL that the ideator files under
	// their own account; bot-filed footer issues are excluded here.
	IssuesHuman int  `json:"issuesHuman"`
	PRsClanker  int  `json:"prsClanker"`
	Commits     int  `json:"commits,omitempty"` // legacy; ignored by the composite formula
	Backfilled  bool `json:"backfilled"`
}

// RepoHistory is the persisted aggregate history for one repo.
type RepoHistory struct {
	RepoID    string        `json:"repoID"`
	Version   int           `json:"version"`
	Days      []DayActivity `json:"days"`
	FetchedAt time.Time     `json:"fetchedAt"`
}

// MergedPullRequest is the public subset of a merged GitHub PR needed by
// higher-level enrichments such as repo news.
type MergedPullRequest struct {
	Title    string    `json:"title"`
	MergedAt time.Time `json:"mergedAt"`
	Author   string    `json:"author,omitempty"`
}

// Store persists repo histories under DATA_DIR with atomic writes.
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]RepoHistory
}

// NewStore opens dir/repo-history.json, creating an empty store if absent.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("history: creating data dir: %w", err)
	}
	s := &Store{path: filepath.Join(dir, "repo-history.json"), data: map[string]RepoHistory{}}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("history: reading repo-history.json: %w", err)
	default:
		var histories []RepoHistory
		if err := json.Unmarshal(raw, &histories); err != nil {
			return nil, fmt.Errorf("history: corrupt repo-history.json: %w", err)
		}
		for _, h := range histories {
			s.data[h.RepoID] = normalizeHistory(h)
		}
	}
	return s, nil
}

// List returns a copy of all persisted histories.
func (s *Store) List() []RepoHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RepoHistory, 0, len(s.data))
	for _, h := range s.data {
		out = append(out, copyHistory(h))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RepoID < out[j].RepoID })
	return out
}

// Get returns a copy of one repo's history.
func (s *Store) Get(repoID string) (RepoHistory, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.data[repoID]
	if !ok {
		return RepoHistory{}, false
	}
	return copyHistory(h), true
}

// Upsert replaces the stored days for repoID. It is idempotent for identical
// day counts, so repeated backfills cannot double-apply chart movement.
func (s *Store) Upsert(repoID string, days []DayActivity, fetchedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := normalizeHistory(RepoHistory{RepoID: repoID, Version: BackfillVersion, Days: days, FetchedAt: fetchedAt.UTC()})
	prev, existed := s.data[repoID]
	s.data[repoID] = h
	if err := s.persistLocked(); err != nil {
		if existed {
			s.data[repoID] = prev
		} else {
			delete(s.data, repoID)
		}
		return err
	}
	return nil
}

func (s *Store) persistLocked() error {
	histories := make([]RepoHistory, 0, len(s.data))
	for _, h := range s.data {
		histories = append(histories, h)
	}
	sort.Slice(histories, func(i, j int) bool { return histories[i].RepoID < histories[j].RepoID })
	if err := fsutil.AtomicWriteJSON(s.path, histories); err != nil {
		return fmt.Errorf("history: %w", err)
	}
	return nil
}

func copyHistory(h RepoHistory) RepoHistory {
	h.Days = append([]DayActivity(nil), h.Days...)
	return h
}

func normalizeHistory(h RepoHistory) RepoHistory {
	byDate := map[string]DayActivity{}
	for _, d := range h.Days {
		if d.Date == "" {
			continue
		}
		d.Backfilled = true
		byDate[d.Date] = d
	}
	h.Days = h.Days[:0]
	for _, d := range byDate {
		h.Days = append(h.Days, d)
	}
	sort.Slice(h.Days, func(i, j int) bool { return h.Days[i].Date < h.Days[j].Date })
	return h
}

// Backfiller fetches GitHub activity and stores aggregate daily counts.
type Backfiller struct {
	Store   *Store
	BaseURL string
	Token   string
	Client  *http.Client
	Now     func() time.Time
	Logf    func(string, ...any)

	sem     chan struct{}
	mu      sync.Mutex
	active  map[string]bool
	limiter ghrate.Limiter
}

// NewBackfiller returns a production backfiller.
func NewBackfiller(store *Store, token string) *Backfiller {
	return &Backfiller{
		Store:  store,
		Token:  token,
		Client: &http.Client{Timeout: requestTimeout},
		Logf:   logWarn,
	}
}

// RefreshAsync starts a bounded, non-blocking refresh for repos that have no
// history or whose last successful fetch is from a previous UTC day.
func (b *Backfiller) RefreshAsync(repos []registry.RepoProfile) {
	if b == nil || b.Store == nil || b.RateLimited() {
		return
	}
	for _, rp := range repos {
		repoID := rp.RepoID
		if !b.needsRefresh(repoID) || !b.markActive(repoID) {
			continue
		}
		go func() {
			defer b.clearActive(repoID)
			if err := b.Backfill(context.Background(), repoID); err != nil && b.Logf != nil {
				b.Logf("history backfill %s: %v", repoID, err)
			}
		}()
	}
}

func (b *Backfiller) needsRefresh(repoID string) bool {
	h, ok := b.Store.Get(repoID)
	if !ok || len(h.Days) == 0 || h.Version != BackfillVersion {
		return true
	}
	now := b.now().UTC().Truncate(24 * time.Hour)
	return h.FetchedAt.UTC().Truncate(24 * time.Hour).Before(now)
}

func (b *Backfiller) markActive(repoID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active == nil {
		b.active = map[string]bool{}
	}
	if b.sem == nil {
		b.sem = make(chan struct{}, maxConcurrent)
	}
	if b.active[repoID] {
		return false
	}
	b.active[repoID] = true
	return true
}

func (b *Backfiller) clearActive(repoID string) {
	b.mu.Lock()
	delete(b.active, repoID)
	b.mu.Unlock()
}

// Backfill fetches trailing GitHub activity for repoID and replaces its stored
// daily counts. Rate limits are graceful skips.
func (b *Backfiller) Backfill(ctx context.Context, repoID string) error {
	if b == nil || b.Store == nil {
		return nil
	}
	if b.RateLimited() {
		return errSkipRepo
	}
	select {
	case b.semaphore() <- struct{}{}:
		defer func() { <-b.semaphore() }()
	case <-ctx.Done():
		return ctx.Err()
	}

	fetchedAt := b.now()
	days := zeroDays(fetchedAt)
	issues, issuesCoveredSince, err := b.fetchIssueActivity(ctx, repoID, windowStart(fetchedAt))
	if err != nil {
		return err
	}
	prs, coveredSince, err := b.fetchPullActivity(ctx, repoID, windowStart(fetchedAt))
	if err != nil {
		return err
	}
	kept := map[string]bool{}
	if !coveredSince.IsZero() {
		// The page cap cut the pull fetch short; days up to and including the
		// boundary were never fully seen, so keep their stored PR counts instead
		// of zeroing (or, for the boundary day itself, undercounting and then
		// freezing that undercount on the next run).
		boundary := coveredSince.UTC().Format("2006-01-02")
		if prev, ok := b.Store.Get(repoID); ok {
			for _, old := range prev.Days {
				if d, ok := days[old.Date]; ok && old.Date <= boundary {
					d.MergedPRs = old.MergedPRs
					d.ClankerPRsCreated = old.ClankerPRsCreated
					d.ClankerPRsMerged = old.ClankerPRsMerged
					d.PRsClanker = old.PRsClanker
					days[old.Date] = d
					kept[old.Date] = true
				}
			}
		}
		for date := range days {
			if date < boundary {
				kept[date] = true
			}
		}
	}
	keptIssues := map[string]bool{}
	if !issuesCoveredSince.IsZero() {
		// Same as the pull side: the issue page cap hid everything created
		// before the boundary, so keep stored issue counts for those days.
		boundary := issuesCoveredSince.UTC().Format("2006-01-02")
		if prev, ok := b.Store.Get(repoID); ok {
			for _, old := range prev.Days {
				if d, ok := days[old.Date]; ok && old.Date <= boundary {
					d.RegularIssuesCreated = old.RegularIssuesCreated
					d.IdeasFiled = old.IdeasFiled
					d.IssuesHuman = old.IssuesHuman
					days[old.Date] = d
					keptIssues[old.Date] = true
				}
			}
		}
		for date := range days {
			if date < boundary {
				keptIssues[date] = true
			}
		}
	}
	for date, counts := range issues {
		if d, ok := days[date]; ok && !keptIssues[date] {
			d.RegularIssuesCreated = counts.RegularIssuesCreated
			d.IdeasFiled = counts.IdeasFiled
			d.IssuesHuman = counts.IssuesHuman
			days[date] = d
		}
	}
	for date, counts := range prs {
		if d, ok := days[date]; ok && !kept[date] {
			d.MergedPRs += counts.RegularPRsMerged
			d.ClankerPRsCreated += counts.ClankerPRsCreated
			d.ClankerPRsMerged += counts.ClankerPRsMerged
			d.PRsClanker += counts.PRsClanker
			days[date] = d
		}
	}
	return b.Store.Upsert(repoID, sortedDays(days), fetchedAt)
}

// RateLimited reports whether GitHub asked us to back off and the reset has
// not passed yet.
func (b *Backfiller) RateLimited() bool {
	if b == nil {
		return false
	}
	return b.limiter.WaitAt(b.now())
}

func (b *Backfiller) semaphore() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sem == nil {
		b.sem = make(chan struct{}, maxConcurrent)
	}
	return b.sem
}

func (b *Backfiller) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now().UTC()
}

func windowStart(now time.Time) time.Time {
	return now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(windowDays - 1))
}

func zeroDays(now time.Time) map[string]DayActivity {
	out := map[string]DayActivity{}
	start := windowStart(now)
	for i := 0; i < windowDays; i++ {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		out[date] = DayActivity{Date: date, Backfilled: true}
	}
	return out
}

func sortedDays(days map[string]DayActivity) []DayActivity {
	out := make([]DayActivity, 0, len(days))
	for _, d := range days {
		d.Backfilled = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

type activityCounts struct {
	indexformula.Counts
	IssuesHuman int
	PRsClanker  int
}

// issuePageDone reports whether paging should stop after a page of issues
// (sorted by created desc). covered is non-zero only when the page cap
// truncated a window that still had more issues inside it.
func issuePageDone(createdAts []time.Time, page int, since time.Time) (done bool, covered time.Time) {
	if len(createdAts) < perPage {
		return true, time.Time{}
	}
	oldest := createdAts[len(createdAts)-1]
	if oldest.Before(since) {
		return true, time.Time{}
	}
	if page >= maxPullPages {
		return true, oldest.UTC()
	}
	return false, time.Time{}
}

// fetchIssueActivity returns per-day issue counts. A non-zero coveredSince
// means the page cap cut the fetch short and days up to that instant are
// incomplete.
func (b *Backfiller) fetchIssueActivity(ctx context.Context, repoID string, since time.Time) (map[string]activityCounts, time.Time, error) {
	out := map[string]activityCounts{}
	var coveredSince time.Time
	for page := 1; page <= maxPullPages; page++ {
		path := fmt.Sprintf("/repos/%s/issues?state=all&sort=created&direction=desc&since=%s&per_page=%d&page=%d", repoID, since.UTC().Format(time.RFC3339), perPage, page)
		var issues []struct {
			CreatedAt   time.Time        `json:"created_at"`
			Body        string           `json:"body"`
			PullRequest *json.RawMessage `json:"pull_request"`
			User        struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := b.getJSON(ctx, path, &issues); err != nil {
			return nil, time.Time{}, err
		}
		createdAts := make([]time.Time, len(issues))
		for i, issue := range issues {
			createdAts[i] = issue.CreatedAt
		}
		for _, issue := range issues {
			if issue.PullRequest != nil || issue.CreatedAt.Before(since) {
				continue
			}
			date := issue.CreatedAt.UTC().Format("2006-01-02")
			counts := out[date]
			if isDibsFiledIssue(issue.Body) {
				counts.IdeasFiled++
			} else {
				counts.RegularIssuesCreated++
			}
			if isHumanLogin(issue.User.Login) {
				counts.IssuesHuman++
			}
			out[date] = counts
		}
		done, covered := issuePageDone(createdAts, page, since)
		if done {
			coveredSince = covered
			break
		}
	}
	return out, coveredSince, nil
}

func isDibsFiledIssue(body string) bool {
	return strings.Contains(body, settle.Footer) || strings.Contains(body, settle.ExternalFooter)
}

// FetchMergedPullRequests fetches recent merged PRs for repoID using the same
// GitHub client, auth, timeout, pagination, and skip semantics as Backfill.
func (b *Backfiller) FetchMergedPullRequests(ctx context.Context, repoID string) ([]MergedPullRequest, error) {
	out, _, err := b.FetchMergedPullRequestsSince(ctx, repoID)
	return out, err
}

// FetchMergedPullRequestsSince is FetchMergedPullRequests plus the coverage
// boundary: a non-zero coveredSince means the page cap was hit while PRs were
// still inside the window, so merges before that instant may be missing.
func (b *Backfiller) FetchMergedPullRequestsSince(ctx context.Context, repoID string) (prs []MergedPullRequest, coveredSince time.Time, err error) {
	if b.RateLimited() {
		return nil, time.Time{}, errSkipRepo
	}
	since := windowStart(b.now())
	var out []MergedPullRequest
	for page := 1; page <= maxPullPages; page++ {
		path := fmt.Sprintf("/repos/%s/pulls?state=closed&sort=updated&direction=desc&per_page=%d&page=%d", repoID, perPage, page)
		var pulls []pullActivity
		if err := b.getJSON(ctx, path, &pulls); err != nil {
			return nil, time.Time{}, err
		}
		for _, pr := range pulls {
			if pr.MergedAt == nil {
				continue
			}
			out = append(out, MergedPullRequest{
				Title:    strings.TrimSpace(pr.Title),
				MergedAt: pr.MergedAt.UTC(),
				Author:   strings.TrimSpace(pr.User.Login),
			})
		}
		done, covered := pullPageDone(pulls, page, since)
		if done {
			coveredSince = covered
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MergedAt.After(out[j].MergedAt) })
	return out, coveredSince, nil
}

// pullPageDone reports whether paging should stop after pulls (sorted by
// updated desc). covered is non-zero only when the page cap truncated a window
// that still had more PRs inside it.
func pullPageDone(pulls []pullActivity, page int, since time.Time) (done bool, covered time.Time) {
	if len(pulls) < perPage {
		return true, time.Time{}
	}
	oldest := pulls[len(pulls)-1].UpdatedAt
	if oldest.IsZero() {
		return page >= maxPullPages, time.Time{}
	}
	if oldest.Before(since) {
		return true, time.Time{}
	}
	if page >= maxPullPages {
		return true, oldest.UTC()
	}
	return false, time.Time{}
}

func (b *Backfiller) fetchPullActivity(ctx context.Context, repoID string, since time.Time) (map[string]activityCounts, time.Time, error) {
	out := map[string]activityCounts{}
	var coveredSince time.Time
	for page := 1; page <= maxPullPages; page++ {
		path := fmt.Sprintf("/repos/%s/pulls?state=all&sort=updated&direction=desc&per_page=%d&page=%d", repoID, perPage, page)
		var pulls []pullActivity
		if err := b.getJSON(ctx, path, &pulls); err != nil {
			return nil, time.Time{}, err
		}
		for _, pr := range pulls {
			clanker := isClankerPR(pr, clankerMarkers())
			createdDate := pr.CreatedAt.UTC().Format("2006-01-02")
			if clanker {
				counts := out[createdDate]
				counts.ClankerPRsCreated++
				counts.PRsClanker++
				out[createdDate] = counts
			}
			if pr.MergedAt == nil {
				continue
			}
			date := pr.MergedAt.UTC().Format("2006-01-02")
			counts := out[date]
			if clanker {
				counts.ClankerPRsMerged++
			} else {
				counts.RegularPRsMerged++
			}
			out[date] = counts
		}
		done, covered := pullPageDone(pulls, page, since)
		if done {
			coveredSince = covered
			break
		}
	}
	return out, coveredSince, nil
}

func isHumanLogin(login string) bool {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return false
	}
	return !strings.HasSuffix(login, "[bot]")
}

type pullActivity struct {
	Title     string     `json:"title"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
	Body      string     `json:"body"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func clankerMarkers() []string {
	raw := envutil.Get(EnvClankerMarkers)
	if raw == "" {
		raw = "hive: agent=,hivecommons-hive[bot],kubestellar-hive[bot]"
	}
	parts := strings.Split(raw, ",")
	markers := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			markers = append(markers, part)
		}
	}
	return markers
}

func isClankerPR(pr pullActivity, markers []string) bool {
	if len(markers) == 0 {
		return false
	}
	fields := []string{pr.Head.Ref, pr.Body, pr.User.Login}
	for _, l := range pr.Labels {
		fields = append(fields, l.Name)
	}
	haystack := strings.ToLower(strings.Join(fields, "\n"))
	for _, marker := range markers {
		if strings.Contains(haystack, marker) {
			return true
		}
	}
	return false
}

var (
	errSkipRepo     = errors.New("skip repo")
	errStatsPending = errors.New("github stats pending")
)

func (b *Backfiller) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	base := b.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if shouldSkip(resp) {
		b.limiter.ObserveAt(resp, b.now())
		return errSkipRepo
	}
	if resp.StatusCode == http.StatusAccepted {
		return errStatsPending
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(out)
}

func shouldSkip(resp *http.Response) bool { return ghrate.IsLimited(resp) }

// logWarn is the default Logf: a constant message with the detail as an attr.
func logWarn(format string, args ...any) {
	slog.Warn("history", "detail", fmt.Sprintf(format, args...))
}
