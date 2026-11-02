// Package catalog builds and searches a separate CNCF project catalog.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/dibs/pkg/fsutil"
	"github.com/hivecommons/dibs/pkg/ghrate"
	"gopkg.in/yaml.v3"
)

const (
	LandscapeURL    = "https://raw.githubusercontent.com/cncf/landscape/master/landscape.yml"
	CacheFile       = "cncf-catalog.json"
	RefreshInterval = 7 * 24 * time.Hour
	TopKLimit       = 15

	defaultGitHubAPI = "https://api.github.com"
	requestTimeout   = 10 * time.Second
	maxLandscapeBody = 32 << 20
	maxGitHubBody    = 4 << 20
	readmeIntroLimit = 2000
)

// Project is one CNCF sandbox/incubating/graduated GitHub project.
type Project struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	RepoURL     string    `json:"repo_url"`
	RepoID      string    `json:"repoID"`
	Category    string    `json:"category"`
	Subcategory string    `json:"subcategory"`
	Maturity    string    `json:"maturity"`
	Topics      []string  `json:"topics,omitempty"`
	Language    string    `json:"language,omitempty"`
	Readme      string    `json:"readme,omitempty"`
	FetchedAt   time.Time `json:"fetchedAt,omitempty"`
}

// Candidate is a BM25-ranked project.
type Candidate struct {
	Project Project `json:"project"`
	Score   float64 `json:"score"`
}

// Store owns the persisted CNCF catalog and its BM25 index.
type Store struct {
	mu       sync.RWMutex
	path     string
	projects []Project
	scorer   *BM25

	Client  *http.Client
	BaseURL string
	Token   string
	Now     func() time.Time
	Logf    func(string, ...any)

	// OnRefresh, when set, receives the result of each background refresh.
	OnRefresh func(error)

	active  bool
	limiter ghrate.Limiter
}

// New opens dir/cncf-catalog.json if present.
func New(dir, token string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("catalog: creating data dir: %w", err)
	}
	s := &Store{path: filepath.Join(dir, CacheFile), Token: token, Client: &http.Client{Timeout: requestTimeout}, Logf: logWarn}
	raw, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("catalog: reading %s: %w", CacheFile, err)
	default:
		var projects []Project
		if err := json.Unmarshal(raw, &projects); err != nil {
			return nil, fmt.Errorf("catalog: corrupt %s: %w", CacheFile, err)
		}
		s.setProjects(projects)
	}
	return s, nil
}

// List returns a copy of all projects.
func (s *Store) List() []Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Project(nil), s.projects...)
}

// Fingerprint hashes the score-relevant fields of every project so callers can
// tell whether previously derived matches are still valid. Order-independent.
func (s *Store) Fingerprint() string {
	projects := s.List()
	sort.Slice(projects, func(i, j int) bool { return projects[i].RepoID < projects[j].RepoID })
	h := sha256.New()
	for _, p := range projects {
		fmt.Fprintf(h, "%q|%q|%q|%q|%q|%q|%q|%q|%q\n", p.RepoID, p.Name, p.Description, p.Category, p.Subcategory, p.Maturity, strings.Join(p.Topics, ","), p.Language, p.Readme)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TopK ranks projects against query with BM25.
func (s *Store) TopK(query string, k int) []Candidate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.scorer == nil || k <= 0 {
		return []Candidate{}
	}
	return s.scorer.TopK(query, k)
}

// RefreshAsync refreshes the cache in the background when missing or stale.
// While a GitHub rate limit is in effect it is a no-op: a pass could not
// enrich anything, so re-downloading the landscape would only churn the cache.
func (s *Store) RefreshAsync() {
	if s == nil || s.limiter.WaitAt(s.now()) || !s.needsRefresh() || !s.markActive() {
		return
	}
	go func() {
		defer s.clearActive()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := s.Refresh(ctx)
		if s.OnRefresh != nil {
			s.OnRefresh(err)
		}
		if err != nil && s.Logf != nil {
			s.Logf("catalog refresh: %v", err)
		}
	}()
}

func (s *Store) needsRefresh() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.projects) == 0 {
		return true
	}
	oldest := s.projects[0].FetchedAt
	for _, p := range s.projects {
		// A never-enriched project (rate-limited pass) must trigger a retry.
		if p.FetchedAt.IsZero() {
			return true
		}
		if p.FetchedAt.Before(oldest) {
			oldest = p.FetchedAt
		}
	}
	return s.now().Sub(oldest) >= RefreshInterval
}

func (s *Store) markActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return false
	}
	s.active = true
	return true
}

func (s *Store) clearActive() { s.mu.Lock(); s.active = false; s.mu.Unlock() }

var errEmptyLandscape = errors.New("catalog: landscape parsed to zero projects; keeping existing catalog")

// Refresh fetches the landscape, enriches GitHub projects, and persists it.
func (s *Store) Refresh(ctx context.Context) error {
	projects, err := FetchLandscape(ctx, s.client())
	if err != nil {
		return err
	}
	fetchedAt := s.now().UTC()
	prev := make(map[string]Project)
	for _, p := range s.List() {
		prev[p.RepoID] = p
	}
	// A 200 that parses to nothing (empty body, renamed upstream keys) is a
	// degraded fetch, not a landscape that legitimately lost every project.
	if len(projects) == 0 && len(prev) > 0 {
		return errEmptyLandscape
	}
	// Enrich never-fetched and oldest projects first so each rate-limited
	// pass makes forward progress; the persisted slice stays sorted by RepoID.
	order := make([]int, len(projects))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return prev[projects[order[a]].RepoID].FetchedAt.Before(prev[projects[order[b]].RepoID].FetchedAt)
	})
	for _, i := range order {
		p := &projects[i]
		// Once GitHub rate-limits us, keep what we already had rather than
		// hammering the API until the reset.
		// After the deadline every call would fail instantly, so carry the
		// project forward without calling or logging per project.
		skipped := s.limiter.WaitAt(s.now()) || ctx.Err() != nil
		stamp := false
		if !skipped {
			err := s.enrich(ctx, p)
			if err != nil && s.Logf != nil {
				s.Logf("catalog enrich %s: %v", p.RepoID, err)
			}
			// Any failure (deadline, 5xx, transport) is "not enriched".
			skipped = err != nil
			// A permanent client error will not heal by retrying every pass;
			// stamp it so it does not keep the whole catalog stale.
			stamp = permanentClientError(err)
		}
		if !skipped {
			p.FetchedAt = fetchedAt
			continue
		}
		if old, ok := prev[p.RepoID]; ok {
			if strings.TrimSpace(old.Description) != "" {
				p.Description = old.Description
			}
			p.Topics, p.Language, p.Readme, p.FetchedAt = old.Topics, old.Language, old.Readme, old.FetchedAt
		}
		if stamp {
			p.FetchedAt = fetchedAt
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fsutil.AtomicWriteJSON(s.path, projects); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	s.projects = projects
	s.scorer = NewBM25(projects)
	return nil
}

func (s *Store) setProjects(projects []Project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projects = projects
	s.scorer = NewBM25(projects)
}

func (s *Store) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: requestTimeout}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// FetchLandscape downloads and parses CNCF landscape.yml.
func FetchLandscape(ctx context.Context, client *http.Client) ([]Project, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LandscapeURL, nil)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: landscape returned status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLandscapeBody))
	if err != nil {
		return nil, err
	}
	return ParseLandscape(raw)
}

type landscapeDoc struct {
	Landscape []struct {
		Name          string `yaml:"name"`
		Subcategories []struct {
			Name  string `yaml:"name"`
			Items []struct {
				Name        string `yaml:"name"`
				Description string `yaml:"description"`
				RepoURL     string `yaml:"repo_url"`
				Project     string `yaml:"project"`
			} `yaml:"items"`
		} `yaml:"subcategories"`
	} `yaml:"landscape"`
}

var maturities = map[string]bool{"sandbox": true, "incubating": true, "graduated": true}

// ParseLandscape extracts CNCF projects with GitHub repositories.
func ParseLandscape(raw []byte) ([]Project, error) {
	var doc landscapeDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("catalog: parsing landscape: %w", err)
	}
	var out []Project
	for _, cat := range doc.Landscape {
		category := strings.TrimSpace(cat.Name)
		for _, sub := range cat.Subcategories {
			subcategory := strings.TrimSpace(sub.Name)
			for _, it := range sub.Items {
				maturity := strings.ToLower(strings.TrimSpace(it.Project))
				if !maturities[maturity] {
					continue
				}
				repoID, ok := repoIDFromURL(it.RepoURL)
				if !ok {
					continue
				}
				out = append(out, Project{
					Name:        strings.TrimSpace(it.Name),
					Description: strings.TrimSpace(it.Description),
					RepoURL:     "https://github.com/" + repoID,
					RepoID:      repoID,
					Category:    category,
					Subcategory: subcategory,
					Maturity:    maturity,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RepoID < out[j].RepoID })
	return out, nil
}

func repoIDFromURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	return parts[0] + "/" + repo, true
}

// statusError is a non-OK GitHub response that is not a 404 or rate limit.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("%s status %d", e.msg, e.code) }

// permanentClientError reports a 4xx (other than timeout/rate-limit) that
// retrying on the next pass will not fix.
func permanentClientError(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code >= 400 && se.code < 500 &&
		se.code != http.StatusRequestTimeout && se.code != http.StatusTooManyRequests
}

var errSkip = errors.New("github enrichment skipped")

// errRateLimited is an errSkip caused by a GitHub rate limit (not a 404).
var errRateLimited = fmt.Errorf("%w: rate limited", errSkip)

func (s *Store) enrich(ctx context.Context, p *Project) error {
	var repo struct {
		Description string   `json:"description"`
		Topics      []string `json:"topics"`
		Language    string   `json:"language"`
	}
	if err := s.getJSON(ctx, "/repos/"+p.RepoID, &repo); err != nil {
		if errors.Is(err, errRateLimited) {
			return err
		}
		if errors.Is(err, errSkip) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(repo.Description) != "" {
		p.Description = strings.TrimSpace(repo.Description)
	}
	p.Topics = repo.Topics
	p.Language = strings.TrimSpace(repo.Language)
	readme, err := s.getReadme(ctx, p.RepoID)
	switch {
	case err == nil:
		p.Readme = readme
	case errors.Is(err, errSkip) && !errors.Is(err, errRateLimited):
		// A 404 readme is a genuine "nothing to fetch".
	default:
		// Let Refresh carry the prior enrichment forward and retry next pass
		// instead of stamping the project fresh with an empty readme.
		return err
	}
	return nil
}

func (s *Store) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := s.githubRequest(ctx, http.MethodGet, path)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if shouldSkip(resp) {
		s.limiter.ObserveAt(resp, s.now())
		return errRateLimited
	}
	if resp.StatusCode == http.StatusNotFound {
		return errSkip
	}
	if resp.StatusCode != http.StatusOK {
		return &statusError{resp.StatusCode, "github returned"}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxGitHubBody)).Decode(out)
}

func (s *Store) getReadme(ctx context.Context, repoID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := s.githubRequest(ctx, http.MethodGet, "/repos/"+repoID+"/readme")
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github.raw")
	resp, err := s.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if shouldSkip(resp) {
		s.limiter.ObserveAt(resp, s.now())
		return "", errRateLimited
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", errSkip
	}
	if resp.StatusCode != http.StatusOK {
		return "", &statusError{resp.StatusCode, "github readme returned"}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubBody))
	if err != nil {
		return "", err
	}
	// GitHub may ignore the raw Accept header in tests/proxies; handle JSON too.
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		var rr struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(raw, &rr) == nil && rr.Content != "" {
			decoded, decErr := base64.StdEncoding.DecodeString(strings.ReplaceAll(rr.Content, "\n", ""))
			if decErr == nil {
				raw = decoded
			}
		}
	}
	return readmeIntro(string(raw)), nil
}

func (s *Store) githubRequest(ctx context.Context, method, path string) (*http.Request, error) {
	base := s.BaseURL
	if base == "" {
		base = defaultGitHubAPI
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	return req, nil
}

func shouldSkip(resp *http.Response) bool { return ghrate.IsLimited(resp) }

var mdNoise = regexp.MustCompile(`(?m)^\s*(#|>|!\[|\[!\[|---|<[^>]+>|\[[^\]]+\]:)`)

func readmeIntro(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || trim == "" || mdNoise.MatchString(trim) {
			continue
		}
		trim = strings.Trim(trim, "*_` ")
		trim = regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`).ReplaceAllString(trim, "$1")
		kept = append(kept, trim)
		if len(strings.Join(kept, "\n")) >= readmeIntroLimit {
			break
		}
	}
	return truncateRunes(strings.TrimSpace(strings.Join(kept, "\n")), readmeIntroLimit)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

var splitRe = regexp.MustCompile(`[^a-z0-9]+`)

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true, "with": true,
	"are": true, "was": true, "you": true, "your": true, "have": true, "has": true,
	"can": true, "will": true, "would": true, "should": true, "could": true, "from": true,
	"into": true, "its": true, "our": true, "their": true, "them": true, "they": true,
	"not": true, "but": true, "all": true, "any": true, "more": true, "some": true,
	"what": true, "when": true, "where": true, "how": true, "why": true, "idea": true,
	"ideas": true, "repo": true, "repos": true, "project": true, "github": true,
}

func Tokenize(s string) []string {
	parts := splitRe.Split(strings.ToLower(s), -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) >= 3 && !stopwords[p] {
			out = append(out, p)
		}
	}
	return out
}

// BM25 is a compact in-memory scorer for catalog projects.
type BM25 struct {
	projects []Project
	tfs      []map[string]int
	lens     []int
	idf      map[string]float64
	avgLen   float64
}

func NewBM25(projects []Project) *BM25 {
	b := &BM25{projects: append([]Project(nil), projects...), idf: map[string]float64{}}
	df := map[string]int{}
	for _, p := range b.projects {
		toks := Tokenize(projectText(p))
		tf := map[string]int{}
		seen := map[string]bool{}
		for _, tok := range toks {
			tf[tok]++
			if !seen[tok] {
				df[tok]++
				seen[tok] = true
			}
		}
		b.tfs = append(b.tfs, tf)
		b.lens = append(b.lens, len(toks))
		b.avgLen += float64(len(toks))
	}
	if len(b.projects) > 0 {
		b.avgLen /= float64(len(b.projects))
	}
	n := float64(len(b.projects))
	for tok, freq := range df {
		b.idf[tok] = math.Log(1 + (n-float64(freq)+0.5)/(float64(freq)+0.5))
	}
	return b
}

func projectText(p Project) string {
	return p.Name + " " + p.Description + " " + strings.Join(p.Topics, " ") + " " + p.Language + " " + p.Readme
}

// TopK returns the k highest-scoring projects. Every project is scored and
// returned (zero-score projects rank last) so callers that need a full ranking,
// such as the hive-repo matcher, keep it; callers that want keyword hits only
// must drop zero scores themselves.
func (b *BM25) TopK(query string, k int) []Candidate {
	q := Tokenize(query)
	if len(q) == 0 || k <= 0 || len(b.projects) == 0 {
		return []Candidate{}
	}
	const k1 = 1.2
	const beta = 0.75
	out := make([]Candidate, 0, len(b.projects))
	for i, p := range b.projects {
		var score float64
		docLen := float64(b.lens[i])
		for _, tok := range q {
			tf := float64(b.tfs[i][tok])
			if tf == 0 {
				continue
			}
			den := tf + k1*(1-beta+beta*(docLen/b.avgLen))
			score += b.idf[tok] * (tf * (k1 + 1) / den)
		}
		out = append(out, Candidate{Project: p, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > k {
		out = out[:k]
	}
	return out
}

// logWarn is the default Logf: a constant message with the detail as an attr.
func logWarn(format string, args ...any) {
	slog.Warn("catalog", "detail", fmt.Sprintf(format, args...))
}
