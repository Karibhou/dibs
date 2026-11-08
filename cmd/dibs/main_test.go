package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/dibs/pkg/registry"
	"github.com/hivecommons/dibs/pkg/server"
)

func TestEnvOr(t *testing.T) {
	t.Run("prefers the DIBS_ key when set", func(t *testing.T) {
		t.Setenv("DIBS_ADDR", ":9999")
		t.Setenv("IDEATE_ADDR", ":1111")
		if got := envOr("DIBS_ADDR", defaultAddr); got != ":9999" {
			t.Fatalf("envOr = %q, want %q", got, ":9999")
		}
	})

	t.Run("falls back to the legacy IDEATE_ prefix", func(t *testing.T) {
		t.Setenv("DIBS_ADDR", "")
		t.Setenv("IDEATE_ADDR", ":1111")
		if got := envOr("DIBS_ADDR", defaultAddr); got != ":1111" {
			t.Fatalf("envOr = %q, want %q", got, ":1111")
		}
	})

	t.Run("returns the default when neither is set", func(t *testing.T) {
		t.Setenv("DIBS_ADDR", "")
		t.Setenv("IDEATE_ADDR", "")
		if got := envOr("DIBS_ADDR", defaultAddr); got != defaultAddr {
			t.Fatalf("envOr = %q, want %q", got, defaultAddr)
		}
	})

	t.Run("non-DIBS keys never consult the legacy prefix", func(t *testing.T) {
		t.Setenv("HUB_URL", "")
		t.Setenv("IDEATE_HUB_URL", "https://legacy.example")
		if got := envOr("HUB_URL", defaultHubURL); got != defaultHubURL {
			t.Fatalf("envOr = %q, want %q", got, defaultHubURL)
		}
	})
}

func TestDisplayBasePath(t *testing.T) {
	if got := displayBasePath(""); got != "/" {
		t.Fatalf("displayBasePath(\"\") = %q, want %q", got, "/")
	}
	if got := displayBasePath("/dibs"); got != "/dibs" {
		t.Fatalf("displayBasePath(\"/dibs\") = %q, want %q", got, "/dibs")
	}
}

func testServe(got **http.Server, err error) func(*http.Server) error {
	return func(srv *http.Server) error {
		if got != nil {
			*got = srv
		}
		return err
	}
}

func TestRunVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	called := false
	err := run([]string{"--version"}, &out, &errOut, func(*http.Server) error { called = true; return nil })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if called {
		t.Fatal("--version must not start the server")
	}
	if want := "dibs " + gitShort + " (" + gitHash + ")\n"; out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestRunHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	called := false
	err := run([]string{"-h"}, &out, &errOut, func(*http.Server) error { called = true; return nil })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if called {
		t.Fatal("-h must not start the server")
	}
	if !strings.Contains(errOut.String(), "Usage of dibs") {
		t.Fatalf("stderr = %q, want it to contain %q", errOut.String(), "Usage of dibs")
	}
}

func TestRunRejectsPositionalArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	called := false
	err := run([]string{"serve"}, &out, &errOut, func(*http.Server) error { called = true; return nil })
	if err == nil {
		t.Fatal("expected error for unexpected positional argument")
	}
	if called {
		t.Fatal("server must not start when unexpected positional args are present")
	}
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("err = %q, want it to mention %q", err.Error(), "unexpected argument")
	}
	if !strings.Contains(errOut.String(), "Usage of dibs") {
		t.Fatalf("stderr = %q, want it to contain %q", errOut.String(), "Usage of dibs")
	}
}

func TestRunRejectsTrailingPositionalArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	called := false
	err := run([]string{"--version", "extra"}, &out, &errOut, func(*http.Server) error { called = true; return nil })
	if err == nil {
		t.Fatal("expected error for unexpected positional argument after --version")
	}
	if called {
		t.Fatal("server must not start when unexpected positional args are present")
	}
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("err = %q, want it to mention %q", err.Error(), "unexpected argument")
	}
	if out.String() != "" {
		t.Fatalf("stdout = %q, want empty (version not printed)", out.String())
	}
}

func TestRunBadFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"--nope"}, &out, &errOut, testServe(nil, nil)); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func setupEnv(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("IDEATE_ADDR", "")
	t.Setenv("DIBS_ADDR", "")
	t.Setenv("DIBS_METRICS_ADDR", "")
	t.Setenv("IDEATE_METRICS_ADDR", "")
	t.Setenv("DIBS_BASE_PATH", "")
	t.Setenv("IDEATE_BASE_PATH", "")
	t.Setenv("HUB_URL", "http://127.0.0.1:1")
	t.Setenv("REPOS_SEED_FILE", "")
}

func TestRunWiring(t *testing.T) {
	setupEnv(t, t.TempDir())
	t.Setenv("DIBS_ADDR", ":0")
	var srv *http.Server
	var out, errOut bytes.Buffer
	if err := run(nil, &out, &errOut, testServe(&srv, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if srv == nil || srv.Handler == nil {
		t.Fatal("server was not built")
	}
	if srv.Addr != ":0" {
		t.Fatalf("Addr = %q, want :0", srv.Addr)
	}
}

func TestRunDefaultAddr(t *testing.T) {
	setupEnv(t, t.TempDir())
	var srv *http.Server
	if err := run(nil, io.Discard, io.Discard, testServe(&srv, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if srv.Addr != defaultAddr {
		t.Fatalf("Addr = %q, want %q", srv.Addr, defaultAddr)
	}
}

func TestRunServeError(t *testing.T) {
	setupEnv(t, t.TempDir())
	want := errors.New("boom")
	err := run(nil, io.Discard, io.Discard, testServe(nil, want))
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrapping %v", err, want)
	}
}

func TestRunStoreOpenFailure(t *testing.T) {
	// A regular file where the data directory should be makes store open fail.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	setupEnv(t, f)
	called := false
	err := run(nil, io.Discard, io.Discard, func(*http.Server) error { called = true; return nil })
	if err == nil {
		t.Fatal("expected error when DATA_DIR is not a directory")
	}
	if called {
		t.Fatal("server must not start after a store failure")
	}
}

// TestRunStoreOpenFailures corrupts each persisted file in turn so every
// store constructor after the idea store fails, and asserts run surfaces the
// matching "opening ..." context instead of starting the server.
func TestRunStoreOpenFailures(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"repos.json", "opening repo registry"},
		{"repo-history.json", "opening repo history store"},
		{"repo-news.json", "opening repo news store"},
		{"cncf-catalog.json", "opening CNCF catalog"},
		{"notifications.json", "opening notification store"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			dataDir := t.TempDir()
			setupEnv(t, dataDir)
			if err := os.WriteFile(filepath.Join(dataDir, tc.file), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			err := run(nil, io.Discard, io.Discard, func(*http.Server) error { called = true; return nil })
			if err == nil {
				t.Fatalf("expected error for corrupt %s", tc.file)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.want)
			}
			if called {
				t.Fatal("server must not start after a store failure")
			}
		})
	}
}

// TestRunOptionalIntegrationsLogged exercises the startup arms taken when the
// LLM gateway, the legacy settlement token, and a repo seed file are all
// configured, and checks each announces itself in the startup log.
func TestRunOptionalIntegrationsLogged(t *testing.T) {
	dataDir := t.TempDir()
	setupEnv(t, dataDir)
	t.Setenv("DIBS_LLM_BASE_URL", "http://127.0.0.1:1/v1/")
	t.Setenv("DIBS_LLM_MODEL", "test-model")
	t.Setenv("DIBS_GITHUB_TOKEN", "ghp_test")
	seed := filepath.Join(t.TempDir(), "seed.json")
	seedBody := `[{"repoID":"example/repo","hiveID":"h1","owner":"someone","topics":["go"],"acceptingIdeas":true}]`
	if err := os.WriteFile(seed, []byte(seedBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPOS_SEED_FILE", seed)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var srv *http.Server
	if err := run(nil, io.Discard, io.Discard, testServe(&srv, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if srv == nil || srv.Handler == nil {
		t.Fatal("server was not built")
	}
	for _, want := range []string{
		`"msg":"match engine llm gateway","base_url":"http://127.0.0.1:1/v1","model":"test-model"`,
		`"msg":"settlement legacy issue creation enabled","env":"DIBS_GITHUB_TOKEN"`,
		`"msg":"seeded repo registry","path":"` + seed + `"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("startup log missing %q:\n%s", want, logs.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "repos.json")); err != nil {
		t.Fatalf("seeded registry was not persisted: %v", err)
	}
}

func TestRunBadSeedFile(t *testing.T) {
	setupEnv(t, t.TempDir())
	t.Setenv("REPOS_SEED_FILE", filepath.Join(t.TempDir(), "missing.json"))
	if err := run(nil, io.Discard, io.Discard, testServe(nil, nil)); err == nil {
		t.Fatal("expected error for missing seed file")
	}
}

func TestServeUntilDrainsOnCancel(t *testing.T) {
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, srv, time.Second) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveUntil: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntil did not return after cancel")
	}
}

func TestServeUntilListenError(t *testing.T) {
	srv := &http.Server{Addr: "bad-address-no-port"}
	if err := serveUntil(context.Background(), srv, time.Second); err == nil {
		t.Fatal("expected listen error")
	}
}

func TestListenMetricsBadAddr(t *testing.T) {
	if err := listenMetrics("not-an-addr"); err == nil {
		t.Fatal("expected listen error for invalid address")
	}
}

func TestRunStartsMetricsListener(t *testing.T) {
	setupEnv(t, t.TempDir())
	t.Setenv("DIBS_METRICS_ADDR", "127.0.0.1:0")
	if err := run(nil, io.Discard, io.Discard, testServe(nil, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
}

// TestServeUntilShutdownDeadline pins the drain-timeout arm: a request still
// in flight when the context is cancelled must surface Shutdown's deadline
// error rather than hang past the Kubernetes termination grace period.
func TestServeUntilShutdownDeadline(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	addrCh := make(chan net.Addr, 1)
	srv := &http.Server{
		Addr:    "127.0.0.1:0",
		Handler: mux,
		BaseContext: func(l net.Listener) context.Context {
			addrCh <- l.Addr()
			return context.Background()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUntil(ctx, srv, 20*time.Millisecond) }()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server never started listening")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		resp, err := client.Get("http://" + addr.String() + "/hold")
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never received the in-flight request")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serveUntil = %v, want context.DeadlineExceeded from Shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntil did not return after the drain timeout")
	}
	close(release)
	<-reqDone
}

// TestRunRegistrySyncSuccess pins the success branch of the background hub
// sync: a reachable hub records a registry_sync last-success sample on
// /metrics (the failure branch is what every other run test exercises via
// the unreachable HUB_URL in setupEnv).
func TestRunRegistrySyncSuccess(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != registry.ReposPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer hub.Close()
	setupEnv(t, t.TempDir())
	t.Setenv("HUB_URL", hub.URL)
	const want = `dibs_background_job_last_success_timestamp_seconds{job="registry_sync"}`
	// serve blocks until the sync is recorded: run cancels the sync loop on
	// return, which would otherwise abort the in-flight first sync.
	var last string
	serve := func(*http.Server) error {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			rec := httptest.NewRecorder()
			server.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			last = rec.Body.String()
			if strings.Contains(last, want) {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return errors.New("registry_sync never recorded a success")
	}
	if err := run(nil, io.Discard, io.Discard, serve); err != nil {
		t.Fatalf("%v; /metrics:\n%s", err, last)
	}
}

// TestRunCancelsRegistrySyncOnReturn pins that the background sync is tied to
// run's lifetime: an in-flight hub request is cancelled once run returns.
func TestRunCancelsRegistrySyncOnReturn(t *testing.T) {
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != registry.ReposPath {
			http.NotFound(w, r)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		close(cancelled)
	}))
	defer hub.Close()
	setupEnv(t, t.TempDir())
	t.Setenv("HUB_URL", hub.URL)
	serve := func(*http.Server) error {
		select {
		case <-started:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("hub never called")
		}
	}
	if err := run(nil, io.Discard, io.Discard, serve); err != nil {
		t.Fatalf("run: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("hub request not cancelled after run returned")
	}
}

// TestNewHTTPServerTimeouts pins the connection timeouts both listeners
// share. IdleTimeout is what reaps keep-alive connections that never send
// another request; ReadTimeout and WriteTimeout must stay unset because a
// server-wide read deadline cancels in-flight /mcp SSE handlers.
func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.Addr != ":0" || srv.Handler == nil {
		t.Fatalf("server = %+v, want Addr :0 with handler", srv)
	}
	if srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, readHeaderTimeout)
	}
	if srv.IdleTimeout != idleTimeout {
		t.Fatalf("IdleTimeout = %v, want %v", srv.IdleTimeout, idleTimeout)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("ReadTimeout/WriteTimeout = %v/%v, want both unset (would cancel /mcp SSE streams)", srv.ReadTimeout, srv.WriteTimeout)
	}
}

// TestRunServerIdleTimeout checks run wires the application listener
// through newHTTPServer rather than building a bare http.Server.
func TestRunServerIdleTimeout(t *testing.T) {
	setupEnv(t, t.TempDir())
	var srv *http.Server
	if err := run(nil, io.Discard, io.Discard, testServe(&srv, nil)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if srv.IdleTimeout != idleTimeout || srv.ReadHeaderTimeout != readHeaderTimeout {
		t.Fatalf("IdleTimeout/ReadHeaderTimeout = %v/%v, want %v/%v", srv.IdleTimeout, srv.ReadHeaderTimeout, idleTimeout, readHeaderTimeout)
	}
}
