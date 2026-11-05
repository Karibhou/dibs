package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// Test modes travel in the document bytes so the production worker does not
// need to forward a test-only environment variable.
func testWorkerInput(mode string) []byte { return []byte("test-worker-" + mode) }

type testWorkerEnvironment struct {
	Env            []string
	MaxUploadBytes int64
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == WorkerCommand {
		os.Exit(testWorkerMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

var bombSink [][]byte

func testWorkerMain(args []string) int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 1
	}
	switch string(data) {
	case string(testWorkerInput("environment")):
		if err := json.NewEncoder(os.Stdout).Encode(testWorkerEnvironment{os.Environ(), MaxUploadBytes()}); err != nil {
			return 1
		}
		return 0
	case string(testWorkerInput("exit")):
		return 3
	case string(testWorkerInput("hang")):
		time.Sleep(time.Minute)
		return 0
	case string(testWorkerInput("flood")):
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), int(maxWorkerOutputBytes())+1))
		return 0
	case string(testWorkerInput("bomb")):
		// What a decompression bomb does to the parser: keep live memory
		// growing far past the pod limit. The worker limits must kill it.
		applyWorkerLimits(workerMemoryLimit, workerDataBudget, os.Stderr)
		for range 64 {
			bombSink = append(bombSink, make([]byte, 32<<20))
		}
		_, _ = io.WriteString(os.Stdout, "survived")
		return 0
	default:
		return WorkerMain(args, bytes.NewReader(data), os.Stdout, os.Stderr)
	}
}

// useTestWorker routes extraction through the test binary re-exec'd as the
// worker. Ordinary document bytes run the real WorkerMain.
func useTestWorker(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(EnvExtractWorker, "")
	SetExtractWorker(exe)
	t.Cleanup(func() { SetExtractWorker("") })
}

func TestExtractWorkerRoundTrip(t *testing.T) {
	useTestWorker(t)
	got, err := extractInWorker(context.Background(), *extractWorkerExe.Load(), ".pdf", generatedPDF("Hello PDF idea"))
	if err != nil || !strings.Contains(got, "Hello PDF idea") {
		t.Fatalf("extractInWorker = %q, %v; want text containing Hello PDF idea", got, err)
	}
	resp, err := ProcessWithContext(context.Background(), "idea.md", "text/markdown", []byte("# Idea\nbody"))
	if err != nil || resp.Text != "# Idea\nbody" || resp.Kind != "document" {
		t.Fatalf("ProcessWithContext = %+v, %v; want the markdown text", resp, err)
	}
}

func TestExtractWorkerEnvironment(t *testing.T) {
	useTestWorker(t)
	for _, key := range []string{
		"DIBS_SESSION_SECRET", "IDEATE_SESSION_SECRET", "AWS_SECRET_ACCESS_KEY",
		"DIBS_UNKNOWN_SETTING", "DIBS_INTAKE_TEST_WORKER_MODE",
		"PATH", "HOME", "GODEBUG", "GOMEMLIMIT", "GOMAXPROCS", "LD_PRELOAD",
	} {
		t.Setenv(key, "must-not-reach-worker")
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		mb   int64
	}{
		{"unset", nil, DefaultMaxUploadMB},
		{"empty", map[string]string{"DIBS_INTAKE_MAX_MB": "", "IDEATE_INTAKE_MAX_MB": ""}, DefaultMaxUploadMB},
		{"primary", map[string]string{"DIBS_INTAKE_MAX_MB": "2"}, 2},
		{"legacy", map[string]string{"IDEATE_INTAKE_MAX_MB": "3"}, 3},
		{"precedence", map[string]string{"DIBS_INTAKE_MAX_MB": "2", "IDEATE_INTAKE_MAX_MB": "3"}, 2},
		{"empty primary", map[string]string{"DIBS_INTAKE_MAX_MB": "", "IDEATE_INTAKE_MAX_MB": "3"}, 3},
		{"invalid primary", map[string]string{"DIBS_INTAKE_MAX_MB": "invalid", "IDEATE_INTAKE_MAX_MB": "3"}, DefaultMaxUploadMB},
		{"whitespace primary", map[string]string{"DIBS_INTAKE_MAX_MB": " ", "IDEATE_INTAKE_MAX_MB": "3"}, DefaultMaxUploadMB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"DIBS_INTAKE_MAX_MB", "IDEATE_INTAKE_MAX_MB"} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			out, err := extractInWorker(context.Background(), *extractWorkerExe.Load(), ".txt", testWorkerInput("environment"))
			if err != nil {
				t.Fatal(err)
			}
			var got testWorkerEnvironment
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Env) != len(tc.env) {
				t.Fatalf("worker environment has %d entries, want %d", len(got.Env), len(tc.env))
			}
			for _, entry := range got.Env {
				key, value, _ := strings.Cut(entry, "=")
				if want, ok := tc.env[key]; !ok {
					t.Fatalf("unexpected worker environment variable %q", key)
				} else if value != want {
					t.Fatalf("worker %s = %q, want %q", key, value, want)
				}
			}
			if want := tc.mb << 20; got.MaxUploadBytes != want {
				t.Fatalf("worker MaxUploadBytes = %d, want %d", got.MaxUploadBytes, want)
			}
			text, err := extractInWorker(context.Background(), *extractWorkerExe.Load(), ".txt", []byte("plain idea"))
			if err != nil || text != "plain idea" {
				t.Fatalf("extractInWorker = %q, %v; want plain idea", text, err)
			}
		})
	}
}

func TestExtractWorkerFailureIsExtractionError(t *testing.T) {
	for _, mode := range []string{"exit", "hang", "flood", "bomb"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "bomb" && runtime.GOOS != "linux" {
				t.Skip("hard data limit is Linux-only")
			}
			useTestWorker(t)
			t.Setenv("DIBS_INTAKE_MAX_MB", "1")
			if mode == "hang" {
				old := workerTimeout
				workerTimeout = 300 * time.Millisecond
				t.Cleanup(func() { workerTimeout = old })
			}
			_, err := ProcessWithContext(context.Background(), "idea.txt", "text/plain", testWorkerInput(mode))
			var ie *Error
			if !errors.As(err, &ie) || ie.Status != http.StatusBadRequest || ie.Msg != "could not extract text from document" {
				t.Fatalf("err = %#v, want 400 could not extract text from document", err)
			}
		})
	}
}

func TestExtractWorkerOptOut(t *testing.T) {
	useTestWorker(t)
	for _, v := range []string{"off", "false", "0"} {
		t.Setenv(EnvExtractWorker, v)
		resp, err := ProcessWithContext(context.Background(), "idea.txt", "text/plain", testWorkerInput("exit"))
		if err != nil || resp.Text != string(testWorkerInput("exit")) {
			t.Fatalf("%s=%s: ProcessWithContext = %+v, %v; want in-process extraction", EnvExtractWorker, v, resp, err)
		}
	}
	SetExtractWorker("")
	if _, ok := extractWorkerEnabled(); ok {
		t.Fatal("extractWorkerEnabled() = true after SetExtractWorker(\"\")")
	}
}

func TestExtractWorkerStartFailure(t *testing.T) {
	_, err := extractInWorker(context.Background(), "/nonexistent/dibs-worker", ".txt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "extraction worker") {
		t.Fatalf("err = %v, want extraction worker error", err)
	}
}

func TestExtractWorkerWaitsForSlot(t *testing.T) {
	workerSlots <- struct{}{}
	defer func() { <-workerSlots }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := extractInWorker(ctx, "/nonexistent/dibs-worker", ".txt", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestExtractWorkerFailsFastWhenBusy(t *testing.T) {
	workerSlots <- struct{}{}
	defer func() { <-workerSlots }()
	old := workerSlotWait
	workerSlotWait = 20 * time.Millisecond
	t.Cleanup(func() { workerSlotWait = old })
	if _, err := extractInWorker(context.Background(), "/nonexistent/dibs-worker", ".txt", []byte("x")); !errors.Is(err, ErrWorkerBusy) {
		t.Fatalf("err = %v, want ErrWorkerBusy", err)
	}
}

func TestProcessReportsBusyWorkerAs503(t *testing.T) {
	SetExtractWorker("/nonexistent/dibs-worker")
	t.Cleanup(func() { SetExtractWorker("") })
	workerSlots <- struct{}{}
	defer func() { <-workerSlots }()
	old := workerSlotWait
	workerSlotWait = 20 * time.Millisecond
	t.Cleanup(func() { workerSlotWait = old })
	_, err := ProcessWithContext(context.Background(), "a.txt", "text/plain", []byte("hello"))
	var ie *Error
	if !errors.As(err, &ie) || ie.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want 503 Error", err)
	}
}

type failingIO struct{}

func (failingIO) Read([]byte) (int, error)  { return 0, errors.New("read failed") }
func (failingIO) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestServeWorker(t *testing.T) {
	t.Setenv("DIBS_INTAKE_MAX_MB", "1")
	var out, errw bytes.Buffer
	if code := serveWorker([]string{".txt"}, strings.NewReader("plain idea"), &out, &errw); code != 0 || out.String() != "plain idea" {
		t.Fatalf("serveWorker = %d, %q (stderr %q); want 0, plain idea", code, out.String(), errw.String())
	}
	for _, tc := range []struct {
		name  string
		args  []string
		in    io.Reader
		out   io.Writer
		code  int
		match string
	}{
		{"no ext", nil, strings.NewReader("x"), io.Discard, 2, "usage"},
		{"read error", []string{".txt"}, failingIO{}, io.Discard, 1, "reading document"},
		{"too large", []string{".txt"}, bytes.NewReader(make([]byte, MaxUploadBytes()+1)), io.Discard, 1, "upload cap"},
		{"bad ext", []string{".xyz"}, strings.NewReader("x"), io.Discard, 1, "extracting document"},
		{"write error", []string{".txt"}, strings.NewReader("x"), failingIO{}, 1, "writing text"},
	} {
		var errw bytes.Buffer
		if code := serveWorker(tc.args, tc.in, tc.out, &errw); code != tc.code || !strings.Contains(errw.String(), tc.match) {
			t.Fatalf("%s: serveWorker = %d, stderr %q; want %d, %q", tc.name, code, errw.String(), tc.code, tc.match)
		}
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 4}
	for _, p := range []string{"ab", "cde", "fg"} {
		if n, err := b.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", p, n, err, len(p))
		}
	}
	if b.buf.String() != "abcd" || !b.overflow {
		t.Fatalf("buffer = %q overflow=%v; want abcd overflow=true", b.buf.String(), b.overflow)
	}
}

func TestApplyWorkerLimits(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	restore := saveDataLimit(t)
	t.Cleanup(restore)
	var errw bytes.Buffer
	applyWorkerLimits(prev, 1<<50, &errw)
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("memory limit = %d, want %d", got, prev)
	}
	if errw.Len() != 0 {
		t.Fatalf("applyWorkerLimits stderr = %q, want empty", errw.String())
	}
}
