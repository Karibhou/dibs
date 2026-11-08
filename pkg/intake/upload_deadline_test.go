package intake

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandleUploadReadDeadlineCutsTrickle pins the body read deadline: a
// client that sends valid multipart headers and then trickles the body
// must be cut off once uploadReadTimeout elapses, instead of holding the
// handler (and its goroutine and fd) for as long as it keeps the socket
// open. MaxBytesReader alone bounds bytes, not time.
func TestHandleUploadReadDeadlineCutsTrickle(t *testing.T) {
	prev := uploadReadTimeout
	uploadReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { uploadReadTimeout = prev })

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		HandleUpload(w, r)
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const boundary = "dibsdeadline"
	// Declare a body far longer than we will ever send.
	fmt.Fprintf(conn, "POST /api/intake HTTP/1.1\r\nHost: dibs\r\nContent-Type: multipart/form-data; boundary=%s\r\nContent-Length: 1048576\r\n\r\n", boundary)
	if _, err := fmt.Fprintf(conn, "--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"idea.txt\"\r\n\r\nslow", boundary); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return after the upload read deadline; a trickling client can hold it open indefinitely")
	}
	// The handler answered (an error status) without waiting on the client.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading response after deadline: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body read aborted by the deadline)", resp.StatusCode, http.StatusBadRequest)
	}
}

// TestHandleUploadWithoutDeadlineSupport pins the fallback: a ResponseWriter
// that cannot take a read deadline (httptest.ResponseRecorder, wrappers
// without Unwrap) must not break uploads; the byte cap still applies.
func TestHandleUploadWithoutDeadlineSupport(t *testing.T) {
	prev := uploadReadTimeout
	uploadReadTimeout = time.Nanosecond
	t.Cleanup(func() { uploadReadTimeout = prev })
	rec := httptest.NewRecorder()
	HandleUpload(rec, uploadRequest(t, "idea.txt", []byte("still works")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}
