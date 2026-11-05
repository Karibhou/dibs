package intake

import (
	"bytes"
	"context"
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

// testWorkerModeEnv makes the re-exec'd test binary misbehave as a worker
// instead of running WorkerMain, so failure paths can be exercised.
const testWorkerModeEnv = "DIBS_INTAKE_TEST_WORKER_MODE"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == WorkerCommand {
		os.Exit(testWorkerMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

var bombSink [][]byte

func testWorkerMain(args []string) int {
	switch os.Getenv(testWorkerModeEnv) {
	case "exit":
		return 3
	case "hang":
		time.Sleep(time.Minute)
		return 0
	case "flood":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), int(maxWorkerOutputBytes())+1))
		return 0
	case "bomb":
		// What a decompression bomb does to the parser: keep live memory
		// growing far past the pod limit. The worker limits must kill it.
		applyWorkerLimits(workerMemoryLimit, workerDataBudget, os.Stderr)
		for range 64 {
			bombSink = append(bombSink, make([]byte, 32<<20))
		}
		_, _ = io.WriteString(os.Stdout, "survived")
		return 0
	default:
		return WorkerMain(args, os.Stdin, os.Stdout, os.Stderr)
	}
}

// useTestWorker routes extraction through the test binary re-exec'd as the
// worker, in the given test mode ("" runs the real WorkerMain).
func useTestWorker(t *testing.T, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(testWorkerModeEnv, mode)
	t.Setenv(EnvExtractWorker, "")
	SetExtractWorker(exe)
	t.Cleanup(func() { SetExtractWorker("") })
}

func TestExtractWorkerRoundTrip(t *testing.T) {
	useTestWorker(t, "")
	got, err := extractInWorker(context.Background(), *extractWorkerExe.Load(), ".pdf", generatedPDF("Hello PDF idea"))
	if err != nil || !strings.Contains(got, "Hello PDF idea") {
		t.Fatalf("extractInWorker = %q, %v; want text containing Hello PDF idea", got, err)
	}
	resp, err := ProcessWithContext(context.Background(), "idea.md", "text/markdown", []byte("# Idea\nbody"))
	if err != nil || resp.Text != "# Idea\nbody" || resp.Kind != "document" {
		t.Fatalf("ProcessWithContext = %+v, %v; want the markdown text", resp, err)
	}
}

func TestExtractWorkerFailureIsExtractionError(t *testing.T) {
	for _, mode := range []string{"exit", "hang", "flood", "bomb"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "bomb" && runtime.GOOS != "linux" {
				t.Skip("hard data limit is Linux-only")
			}
			useTestWorker(t, mode)
			t.Setenv("DIBS_INTAKE_MAX_MB", "1")
			if mode == "hang" {
				old := workerTimeout
				workerTimeout = 300 * time.Millisecond
				t.Cleanup(func() { workerTimeout = old })
			}
			_, err := ProcessWithContext(context.Background(), "idea.txt", "text/plain", []byte("plain idea"))
			var ie *Error
			if !errors.As(err, &ie) || ie.Status != http.StatusBadRequest || ie.Msg != "could not extract text from document" {
				t.Fatalf("err = %#v, want 400 could not extract text from document", err)
			}
		})
	}
}

func TestExtractWorkerOptOut(t *testing.T) {
	useTestWorker(t, "exit")
	for _, v := range []string{"off", "false", "0"} {
		t.Setenv(EnvExtractWorker, v)
		resp, err := ProcessWithContext(context.Background(), "idea.txt", "text/plain", []byte("plain idea"))
		if err != nil || resp.Text != "plain idea" {
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
