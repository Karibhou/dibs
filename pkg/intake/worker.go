package intake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/dibs/pkg/envutil"
)

// WorkerCommand is the hidden first argument that makes a dibs binary act as
// a one-shot document extraction worker (see WorkerMain).
const WorkerCommand = "__extract-worker"

const (
	// EnvExtractWorker opts out of the isolated extraction worker when set
	// to "off", "false" or "0"; extraction then runs in-process.
	EnvExtractWorker = "DIBS_INTAKE_EXTRACT_WORKER"
	// workerMemoryLimit is the Go soft memory limit inside the worker: the
	// GC works hard to stay under it, well below the pod's 256Mi.
	workerMemoryLimit = 96 << 20
	// workerDataBudget is the hard cap (RLIMIT_DATA, on top of what the
	// worker has already mapped at startup) on the worker's writable memory.
	// Past it the Go runtime cannot grow the heap and the worker dies, which
	// the parent reports as an extraction failure.
	workerDataBudget  = 160 << 20
	workerStderrBytes = 4 << 10
)

var (
	// workerTimeout bounds one extraction worker run.
	workerTimeout = 30 * time.Second
	// extractWorkerExe is the executable re-exec'd as the extraction worker;
	// nil or empty means extraction runs in-process.
	extractWorkerExe atomic.Pointer[string]
	// workerSlots serializes workers so concurrent uploads cannot stack
	// several worker memory budgets inside the pod's limit.
	workerSlots = make(chan struct{}, 1)
)

// SetExtractWorker makes ProcessWithContext extract documents in a
// short-lived child process: exe is re-executed as
// `exe __extract-worker <ext>`, and that process must dispatch to WorkerMain.
// The document parsers allocate in proportion to decompressed — not uploaded
// — bytes, so a crafted file can demand far more memory than the pod has
// (hivecommons/dibs#590); the child runs under a memory limit and a timeout,
// and a child that dies is just a failed extraction. An empty exe restores
// in-process extraction.
func SetExtractWorker(exe string) {
	extractWorkerExe.Store(&exe)
}

func extractWorkerEnabled() (string, bool) {
	p := extractWorkerExe.Load()
	if p == nil || *p == "" {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(envutil.Get(EnvExtractWorker))) {
	case "off", "false", "0":
		return "", false
	}
	return *p, true
}

// extractDocument runs ExtractDocument in the extraction worker when one is
// configured, in-process otherwise.
func extractDocument(ctx context.Context, ext string, data []byte) (string, error) {
	if exe, ok := extractWorkerEnabled(); ok {
		return extractInWorker(ctx, exe, ext, data)
	}
	return ExtractDocument(ext, data)
}

// maxWorkerOutputBytes caps the text read back from a worker. No extractor
// legitimately returns more than twice the upload cap.
func maxWorkerOutputBytes() int64 { return 2 * MaxUploadBytes() }

func extractInWorker(ctx context.Context, exe, ext string, data []byte) (string, error) {
	select {
	case workerSlots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-workerSlots }()
	ctx, cancel := context.WithTimeout(ctx, workerTimeout)
	defer cancel()
	stdout := &cappedBuffer{max: maxWorkerOutputBytes()}
	stderr := &cappedBuffer{max: workerStderrBytes}
	cmd := exec.CommandContext(ctx, exe, WorkerCommand, ext)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("extraction worker: %w", ctx.Err())
		}
		return "", fmt.Errorf("extraction worker: %w: %s", err, strings.TrimSpace(stderr.buf.String()))
	}
	if stdout.overflow {
		return "", fmt.Errorf("extraction worker output exceeds %d bytes", stdout.max)
	}
	return stdout.buf.String(), nil
}

// cappedBuffer keeps the first max bytes written to it and discards (while
// still accepting, so the writer never blocks) the rest.
type cappedBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	max      int64
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.max - int64(b.buf.Len())
	if int64(len(p)) > room {
		b.overflow = true
		b.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// WorkerMain is the body of the extraction worker process started by
// SetExtractWorker: it applies the worker memory limits, then reads a
// document from stdin, extracts its text with ExtractDocument(args[0], …)
// and writes the text to stdout. It returns the process exit code.
func WorkerMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	applyWorkerLimits(workerMemoryLimit, workerDataBudget, stderr)
	return serveWorker(args, stdin, stdout, stderr)
}

func applyWorkerLimits(memoryLimit, dataBudget int64, stderr io.Writer) {
	debug.SetMemoryLimit(memoryLimit)
	if err := limitDataSegment(dataBudget); err != nil {
		fmt.Fprintf(stderr, "extraction worker: data limit not applied: %v\n", err)
	}
}

func serveWorker(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: %s <ext>\n", WorkerCommand)
		return 2
	}
	data, err := io.ReadAll(io.LimitReader(stdin, MaxUploadBytes()+1))
	if err == nil && int64(len(data)) > MaxUploadBytes() {
		err = errors.New("document exceeds the upload cap")
	}
	if err != nil {
		fmt.Fprintf(stderr, "reading document: %v\n", err)
		return 1
	}
	text, err := ExtractDocument(args[0], data)
	if err != nil {
		fmt.Fprintf(stderr, "extracting document: %v\n", err)
		return 1
	}
	if _, err := io.WriteString(stdout, text); err != nil {
		fmt.Fprintf(stderr, "writing text: %v\n", err)
		return 1
	}
	return 0
}
