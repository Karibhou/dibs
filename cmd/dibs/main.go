// Command dibs serves the Dibs marketplace: JSON API + embedded static UI
// in a single process, under DIBS_BASE_PATH (default "/" — Dibs is served
// at its own subdomain, dibs.hivecommons.dev).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/dibs/pkg/api"
	"github.com/hivecommons/dibs/pkg/auth"
	"github.com/hivecommons/dibs/pkg/catalog"
	"github.com/hivecommons/dibs/pkg/deps"
	"github.com/hivecommons/dibs/pkg/envutil"
	"github.com/hivecommons/dibs/pkg/history"
	"github.com/hivecommons/dibs/pkg/intake"
	"github.com/hivecommons/dibs/pkg/match"
	"github.com/hivecommons/dibs/pkg/news"
	"github.com/hivecommons/dibs/pkg/notify"
	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/server"
	"github.com/hivecommons/dibs/pkg/settle"
	"github.com/hivecommons/dibs/pkg/store"
)

// Stamped at build time via -ldflags (see Dockerfile). The freshness probe
// reads `dibs --version`, so keep the output format stable.
var (
	gitHash  = "unknown"
	gitShort = "unknown"
)

const (
	defaultAddr    = ":8080"
	defaultHubURL  = "https://hive.hivecommons.dev"
	defaultDataDir = "/data"
	// registrySyncInterval is how often the hub's repo list is re-pulled.
	registrySyncInterval = 5 * time.Minute
	hubSyncTimeout       = 30 * time.Second
	// shutdownTimeout stays under the Kubernetes default 30s termination
	// grace period.
	shutdownTimeout = 20 * time.Second
	// readHeaderTimeout bounds how long a connection may take to send its
	// request line and headers; idleTimeout bounds how long a keep-alive
	// connection may sit between requests. Without the latter an idle
	// client holds a goroutine and a file descriptor for as long as it
	// stays connected.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

// newHTTPServer is the one place both listeners get their connection
// timeouts, so neither can lose them. ReadTimeout and WriteTimeout are
// deliberately left unset: /mcp serves long-lived Streamable HTTP (SSE)
// responses, and Go cancels the request context when a server-wide
// ReadTimeout expires mid-handler, which would cut every MCP stream off at
// the deadline. Routes that read a large body bound that read themselves
// (see intake.HandleUpload).
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// envOr reads key, honoring the legacy IDEATE_-prefixed name (the product's
// pre-rename env prefix) as a fallback before the default.
func envOr(key, def string) string {
	return envutil.GetOr(key, def)
}

// githubTokenFromEnv returns DIBS_GITHUB_TOKEN with surrounding whitespace
// removed; a trailing newline would make the Authorization header invalid.
func githubTokenFromEnv() string {
	return strings.TrimSpace(envOr(settle.EnvGitHubToken, ""))
}

// hubURLFromEnv returns HUB_URL as a bare origin: surrounding whitespace and
// trailing slashes removed, so the hub clients' BaseURL+path concatenation
// never yields "//api/..." when the operator configured "https://hub/".
func hubURLFromEnv() string {
	u := strings.TrimSpace(envOr("HUB_URL", defaultHubURL))
	if u == "" {
		u = defaultHubURL
	}
	return strings.TrimRight(u, "/")
}

// listenMetrics serves the Prometheus /metrics endpoint on addr until it
// fails. It is a separate listener so ingress, which only targets the
// application port, can never expose it.
func listenMetrics(addr string) error {
	srv := newHTTPServer(addr, server.MetricsHandler())
	return srv.ListenAndServe()
}

// displayBasePath renders the normalized base path ("" means root) for logs.
func displayBasePath(base string) string {
	if base == "" {
		return "/"
	}
	return base
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == intake.WorkerCommand {
		os.Exit(intake.WorkerMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if exe, err := os.Executable(); err == nil {
		intake.SetExtractWorker(exe)
	} else {
		slog.Warn("intake extraction worker disabled", "err", err)
	}
	err := run(os.Args[1:], os.Stdout, os.Stderr, func(srv *http.Server) error {
		return serveUntil(ctx, srv, shutdownTimeout)
	})
	if err != nil {
		stop()
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// serveUntil runs srv until ctx is cancelled (SIGTERM during a rollout), then
// drains in-flight requests for up to timeout so a replaced pod does not cut
// off requests or a store write midway.
func serveUntil(ctx context.Context, srv *http.Server, timeout time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// run is main without the process exit: serve is injected so tests can
// exercise startup wiring without binding a port.
func run(args []string, stdout, stderr io.Writer, serve func(*http.Server) error) error {
	fs := flag.NewFlagSet("dibs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the embedded commit and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dibs: unexpected argument: %q\n", fs.Arg(0))
		fs.Usage()
		return fmt.Errorf("unexpected argument: %q", fs.Arg(0))
	}
	if *showVersion {
		fmt.Fprintf(stdout, "dibs %s (%s)\n", gitShort, gitHash)
		return nil
	}

	addr := envOr("DIBS_ADDR", defaultAddr)
	basePath := server.NormalizeBasePath(envOr("DIBS_BASE_PATH", server.DefaultBasePath))
	hubURL := hubURLFromEnv()
	dataDir := envOr("DATA_DIR", defaultDataDir)

	st, err := store.New(dataDir)
	if err != nil {
		return fmt.Errorf("opening idea store: %w", err)
	}
	reg, err := registry.New(dataDir)
	if err != nil {
		return fmt.Errorf("opening repo registry: %w", err)
	}
	hist, err := history.NewStore(dataDir)
	if err != nil {
		return fmt.Errorf("opening repo history store: %w", err)
	}
	newsStore, err := news.NewStore(dataDir)
	if err != nil {
		return fmt.Errorf("opening repo news store: %w", err)
	}
	ghToken := githubTokenFromEnv()
	cncfCatalog, err := catalog.New(dataDir, ghToken)
	if err != nil {
		return fmt.Errorf("opening CNCF catalog: %w", err)
	}
	cncfCatalog.OnRefresh = func(err error) { server.RecordJob(server.JobCatalogRefresh, err) }
	cncfCatalog.RefreshAsync()

	notifications, err := notify.New(dataDir)
	if err != nil {
		return fmt.Errorf("opening notification store: %w", err)
	}

	// Match engine: LLM via hive's litellm gateway when DIBS_LLM_BASE_URL
	// is set, deterministic keyword fallback otherwise — Dibs fully works
	// without a gateway.
	llm := match.LLMFromEnv()
	if llm != nil {
		slog.Info("match engine llm gateway", "base_url", llm.BaseURL, "model", llm.Model)
	} else {
		slog.Info("match engine deterministic fallback", "unset_env", match.EnvLLMBaseURL)
	}
	engine := &match.Engine{Store: st, Registry: reg, Catalog: cncfCatalog, LLM: llm, Notifier: &api.MatchNotifier{Notify: notifications}}

	// Settlement: by default Dibs is just the matchmaker — the ideator
	// files the credited issue via a prefilled GitHub URL. Setting
	// DIBS_GITHUB_TOKEN enables the demoted legacy mode where Dibs opens
	// the issue server-side on accept.
	settler := &settle.Settler{}
	if gh := settle.FromEnv(); gh != nil {
		settler.GitHub = gh
		slog.Info("settlement legacy issue creation enabled", "env", settle.EnvGitHubToken)
	} else {
		slog.Info("settlement matchmaker mode")
	}
	// Confirmed issue URLs are checked against GitHub before an idea settles;
	// the token is optional (public repos read anonymously).
	issueLookup := &settle.HTTPClient{Token: ghToken}
	backfiller := history.NewBackfiller(hist, ghToken)
	newsGen := news.NewGenerator(newsStore, backfiller, llm)

	if seed := os.Getenv("REPOS_SEED_FILE"); seed != "" {
		if err := reg.LoadSeedFile(seed); err != nil {
			return fmt.Errorf("loading REPOS_SEED_FILE: %w", err)
		}
		slog.Info("seeded repo registry", "path", seed)
	}

	// Background hub→registry sync. Failures are logged, never fatal: the
	// registry keeps serving its last-known (or seeded) state.
	hubRepos := &registry.HTTPHubClient{BaseURL: hubURL, Token: ghToken}
	// The loop is tied to run's lifetime so it cannot outlive the server.
	appCtx, cancelApp := context.WithCancel(context.Background())
	defer cancelApp()
	go func() {
		for {
			ctx, cancel := context.WithTimeout(appCtx, hubSyncTimeout)
			err := reg.Sync(ctx, hubRepos)
			if appCtx.Err() != nil {
				cancel()
				return
			}
			server.RecordJob(server.JobRegistrySync, err)
			// Gated by catalog.needsRefresh, so this retries a failed or
			// empty catalog and honors RefreshInterval regardless of hub sync.
			cncfCatalog.RefreshAsync()
			if err != nil {
				slog.Warn("registry sync failed", "job", server.JobRegistrySync, "err", err)
			} else {
				repos := reg.List(false)
				backfiller.RefreshAsync(repos)
				newsGen.RefreshAsync(repos)
			}
			cancel()
			select {
			case <-appCtx.Done():
				return
			case <-time.After(registrySyncInterval):
			}
		}
	}()

	handler := server.New(server.Config{
		BasePath: basePath,
		HubURL:   hubURL,
		Hub:      &auth.HTTPHubClient{BaseURL: hubURL},
		Deps: deps.Deps{
			Store:    st,
			Registry: reg,
			History:  hist,
			News:     newsStore,
			Engine:   engine,
			Settler:  settler,
			Issues:   issueLookup,
			Notify:   notifications,
		},
		Version: gitHash,
	})

	// DIBS_METRICS_ADDR (e.g. ":9090") enables the internal /metrics
	// listener; unset keeps it off.
	if metricsAddr := envOr("DIBS_METRICS_ADDR", ""); metricsAddr != "" {
		go func() { slog.Error("metrics listener stopped", "addr", metricsAddr, "err", listenMetrics(metricsAddr)) }()
	}

	slog.Info("listening", "version", gitShort, "addr", addr, "base_path", displayBasePath(basePath), "hub", hubURL, "data_dir", dataDir)
	srv := newHTTPServer(addr, handler)
	if err := serve(srv); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}
