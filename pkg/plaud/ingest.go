package plaud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// relistOverlap re-lists this far behind the cursor so recordings whose
	// transcript finished late are still picked up.
	relistOverlap = 24 * time.Hour
	// DefaultSinceDays bounds how far back the first run looks.
	DefaultSinceDays = 7
	// DefaultMaxPerHour and DefaultMaxPerDay match the intake budget per
	// identity (10/h, 30/24h).
	DefaultMaxPerHour = 10
	DefaultMaxPerDay  = 30
	// maxFilesPerRun caps recordings listed per run.
	maxFilesPerRun = 200
	submitTimeout  = time.Minute
)

// ErrRateLimited reports that Dibs (or a proxy in front of it) answered
// 429; the batch stops and resumes on the next run.
var ErrRateLimited = errors.New("dibs: rate limited (HTTP 429)")

// Sink submits one idea to Dibs.
type Sink interface {
	Submit(ctx context.Context, idea Idea) error
}

// Ingester moves new Plaud recordings into Dibs.
type Ingester struct {
	// Dial opens a Plaud MCP session; it is called once per run.
	Dial       func(ctx context.Context) (Tools, error)
	Sink       Sink
	CursorPath string
	Tags       []string
	SinceDays  int
	MaxPerHour int
	MaxPerDay  int
	Log        *slog.Logger
	Now        func() time.Time
}

// Result summarizes one run.
type Result struct {
	Submitted int
	Skipped   int
	// BudgetExhausted is set when the local --max-per-hour/--max-per-day
	// budget stopped the batch.
	BudgetExhausted bool
	// RateLimited is set when Dibs answered HTTP 429.
	RateLimited bool
}

func (in *Ingester) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

func (in *Ingester) log() *slog.Logger {
	if in.Log != nil {
		return in.Log
	}
	return slog.Default()
}

// RunOnce lists recordings since the cursor and submits each unseen one,
// oldest first. The cursor is saved after every submission, so a crash
// never resubmits. A 429 or an exhausted local budget stops the batch
// without error; the rest is picked up next run.
func (in *Ingester) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	cur, err := LoadCursor(in.CursorPath)
	if err != nil {
		return res, err
	}
	now := in.now()
	cur.prune(now)
	tools, err := in.Dial(ctx)
	if err != nil {
		return res, err
	}
	defer tools.Close()

	since := cur.Newest.Add(-relistOverlap)
	if cur.Newest.IsZero() {
		days := in.SinceDays
		if days <= 0 {
			days = DefaultSinceDays
		}
		since = now.Add(-time.Duration(days) * 24 * time.Hour)
	}
	recs, err := listRecordings(ctx, tools, since, maxFilesPerRun)
	if err != nil {
		return res, err
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].When().Before(recs[j].When()) })
	for _, r := range recs {
		if _, ok := cur.Seen[r.ID]; ok {
			continue
		}
		if !cur.allow(now, in.MaxPerHour, in.MaxPerDay) {
			in.log().Info("plaud ingest budget reached; resuming next run", "per_hour", in.MaxPerHour, "per_day", in.MaxPerDay)
			res.BudgetExhausted = true
			break
		}
		idea, err := buildIdea(ctx, tools, r, in.Tags)
		if err != nil {
			in.log().Warn("plaud recording skipped", "file_id", r.ID, "err", err)
			res.Skipped++
			continue
		}
		if err := in.Sink.Submit(ctx, idea); err != nil {
			if errors.Is(err, ErrRateLimited) {
				in.log().Warn("dibs rate limited the submission; resuming next run", "file_id", r.ID)
				res.RateLimited = true
				break
			}
			return res, err
		}
		cur.markSubmitted(r, now)
		if err := cur.Save(in.CursorPath); err != nil {
			return res, err
		}
		res.Submitted++
		in.log().Info("plaud recording submitted", "file_id", r.ID, "title", idea.Title)
	}
	return res, nil
}

// Run calls RunOnce, then every interval until ctx is done. With
// interval <= 0 it runs once and returns that run's error. In loop mode a
// failed run is logged and retried on the next tick.
func (in *Ingester) Run(ctx context.Context, interval time.Duration) error {
	for {
		res, err := in.RunOnce(ctx)
		if interval <= 0 {
			if err == nil {
				in.log().Info("plaud ingest done", "submitted", res.Submitted, "skipped", res.Skipped, "budget_exhausted", res.BudgetExhausted, "rate_limited", res.RateLimited)
			}
			return err
		}
		if err != nil {
			in.log().Warn("plaud ingest run failed", "err", err)
		} else {
			in.log().Info("plaud ingest run done", "submitted", res.Submitted, "skipped", res.Skipped, "budget_exhausted", res.BudgetExhausted, "rate_limited", res.RateLimited)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// DibsSink submits ideas with the Dibs MCP submit_idea tool (private
// draft owned by the token's identity).
type DibsSink struct {
	// Endpoint is the Dibs MCP URL, e.g. https://dibs.hivecommons.dev/mcp.
	Endpoint string
	// Token is the hub bearer token. It is only ever sent in the
	// Authorization header, never logged.
	Token string
	// Transport defaults to http.DefaultTransport.
	Transport http.RoundTripper
}

// Submit implements Sink.
func (s *DibsSink) Submit(ctx context.Context, idea Idea) error {
	ctx, cancel := context.WithTimeout(ctx, submitTimeout)
	defer cancel()
	base := s.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	rt := &bearerTransport{base: base, token: s.Token}
	session, err := mcpsdk.NewClient(clientImpl, nil).Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint:             s.Endpoint,
		HTTPClient:           &http.Client{Transport: rt},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return rt.wrap(err)
	}
	defer session.Close()
	tags := idea.Tags
	if tags == nil {
		tags = []string{}
	}
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "submit_idea",
		Arguments: map[string]any{"title": idea.Title, "description": idea.Description, "tags": tags},
	})
	if err != nil {
		return rt.wrap(err)
	}
	if res.IsError {
		return fmt.Errorf("dibs: submit_idea: %s", firstLine(resultText(res)))
	}
	return nil
}

// bearerTransport adds the Authorization header and remembers a 429.
type bearerTransport struct {
	base    http.RoundTripper
	token   string
	limited atomic.Bool
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		t.limited.Store(true)
	}
	return resp, err
}

func (t *bearerTransport) wrap(err error) error {
	if t.limited.Load() {
		return fmt.Errorf("%w: %v", ErrRateLimited, err)
	}
	return fmt.Errorf("dibs: submit_idea: %w", err)
}
