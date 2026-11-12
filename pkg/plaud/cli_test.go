package plaud

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is a bytes.Buffer safe for the spawned server's stderr copier
// and the logger to write concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestMainUsageErrors(t *testing.T) {
	withToken := envMap(map[string]string{EnvToken: "hub-token"})
	cases := []struct {
		name   string
		args   []string
		getenv func(string) string
		code   int
		want   string
	}{
		{"help", []string{"-h"}, withToken, 0, "usage: dibs ingest plaud"},
		{"bad flag", []string{"--bogus"}, withToken, 2, "bogus"},
		{"extra arg", []string{"now"}, withToken, 2, "unexpected argument"},
		{"no token", nil, envMap(nil), 2, EnvToken},
		{"bad scheme", []string{"--server", "ftp://dibs.example"}, withToken, 2, "--server"},
		{"no host", []string{"--server", "https://"}, withToken, 2, "--server"},
		{"unparseable", []string{"--server", "http://[::1"}, withToken, 2, "--server"},
	}
	for _, tc := range cases {
		var stderr bytes.Buffer
		if code := Main(context.Background(), tc.args, tc.getenv, &stderr); code != tc.code || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: code=%d stderr=%q; want %d containing %q", tc.name, code, stderr.String(), tc.code, tc.want)
		}
	}
}

func TestMainNeedsHomeForDefaultCursor(t *testing.T) {
	t.Setenv("HOME", "")
	var stderr bytes.Buffer
	if code := Main(context.Background(), nil, envMap(map[string]string{EnvToken: "hub-token"}), &stderr); code != 1 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
}

func TestMainReportsRunFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var stderr bytes.Buffer
	env := envMap(map[string]string{EnvToken: "secret-hub-token", EnvMCPCommand: "/nonexistent/plaud-mcp"})
	if code := Main(context.Background(), nil, env, &stderr); code != 1 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "secret-hub-token") {
		t.Fatalf("stderr leaks the token: %s", stderr.String())
	}
}

func TestMainIngestsEndToEnd(t *testing.T) {
	srv, st := dibsServer(t)
	t.Setenv(fakeEnv, "1")
	cursor := filepath.Join(t.TempDir(), "state", "plaud-cursor.json")
	env := envMap(map[string]string{EnvToken: "hub-token", EnvServer: srv.URL + "/"})
	var stderr syncBuffer
	args := []string{"--mcp-command", os.Args[0], "--cursor", cursor, "--since-days", "3650", "--tags", " plaud, ,mine "}
	if code := Main(context.Background(), args, env, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr=%s", code, stderr.String())
	}
	ideas, err := st.ListByAuthor("alice")
	if err != nil || len(ideas) != 1 {
		t.Fatalf("ideas = %d, %v; stderr=%s", len(ideas), err, stderr.String())
	}
	idea := ideas[0]
	if idea.Title != "Standup idea" || !strings.Contains(idea.Body, "Build a voice inbox") || !strings.Contains(idea.Body, "Me: What if Dibs took voice notes?\nMe: Second page utterance.") || strings.Join(idea.Tags, ",") != "plaud,mine" {
		t.Fatalf("idea = %+v", idea)
	}
	if strings.Contains(stderr.String(), "hub-token") {
		t.Fatalf("stderr leaks the token: %s", stderr.String())
	}
	if code := Main(context.Background(), args, env, &stderr); code != 0 {
		t.Fatalf("second run code = %d", code)
	}
	if ideas, _ := st.ListByAuthor("alice"); len(ideas) != 1 {
		t.Fatalf("second run resubmitted: %d ideas", len(ideas))
	}
}
