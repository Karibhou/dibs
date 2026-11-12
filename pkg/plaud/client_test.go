package plaud

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestClientCallToolReturnsText(t *testing.T) {
	c := connectFake(t, defaultFake())
	got, err := c.CallTool(context.Background(), "get_note", map[string]any{"file_id": "rec-1"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(got, "voice inbox") {
		t.Fatalf("get_note = %q", got)
	}
	got, err = c.CallTool(context.Background(), "multi", nil)
	if err != nil || got != "one\ntwo" {
		t.Fatalf("multi = %q, %v; want text parts joined, non-text skipped", got, err)
	}
}

func TestClientCallToolErrors(t *testing.T) {
	c := connectFake(t, defaultFake())
	_, err := c.CallTool(context.Background(), "get_note", map[string]any{"file_id": "missing"})
	if err == nil || !strings.Contains(err.Error(), "file not found: missing") || strings.Contains(err.Error(), "more detail") {
		t.Fatalf("tool error = %v, want first line of the isError text", err)
	}
	if _, err := c.CallTool(context.Background(), "no_such_tool", nil); err == nil {
		t.Fatal("unknown tool: want error")
	}
}

func TestDialSpawnsStdioServer(t *testing.T) {
	t.Setenv(fakeEnv, "1")
	c, err := Dial(context.Background(), os.Args[0], io.Discard)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	got, err := c.CallTool(context.Background(), "list_files", map[string]any{"page": 1})
	if err != nil || !strings.Contains(got, "rec-1") {
		t.Fatalf("list_files = %q, %v", got, err)
	}
}

func TestDialErrors(t *testing.T) {
	if _, err := Dial(context.Background(), "   ", io.Discard); err == nil {
		t.Fatal("empty command: want error")
	}
	if _, err := Dial(context.Background(), "/nonexistent/plaud-mcp --flag", io.Discard); err == nil {
		t.Fatal("missing binary: want error")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("  a\nb "); got != "a" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine("single"); got != "single" {
		t.Fatalf("firstLine = %q", got)
	}
}
