package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestIngestRequiresKnownSource(t *testing.T) {
	for _, args := range [][]string{nil, {"zapier"}} {
		var stderr bytes.Buffer
		if code := ingest(context.Background(), args, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage: dibs ingest plaud") {
			t.Fatalf("ingest(%q) = %d, stderr=%q", args, code, stderr.String())
		}
	}
}

func TestIngestDispatchesPlaud(t *testing.T) {
	var stderr bytes.Buffer
	if code := ingest(context.Background(), []string{"plaud", "-h"}, &stderr); code != 0 || !strings.Contains(stderr.String(), "-mcp-command") {
		t.Fatalf("ingest plaud -h = %d, stderr=%q", code, stderr.String())
	}
}
