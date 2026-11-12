// Package plaud ingests Plaud Note recordings into Dibs. It reads recordings
// through the official Plaud MCP server over stdio and submits each one as a
// private draft idea through the Dibs MCP endpoint (submit_idea).
//
// Plaud's OAuth token stays in the Plaud MCP server's own store
// (~/.plaud/tokens-mcp.json); this package never reads or copies it.
package plaud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultCommand launches the official (free) Plaud MCP server.
const DefaultCommand = "npx -y @plaud-ai/mcp@latest"

var clientImpl = &mcpsdk.Implementation{Name: "dibs-ingest-plaud", Version: "v1"}

// Tools is the subset of an MCP client session the ingester needs.
type Tools interface {
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
	Close() error
}

// Client is a minimal MCP client session: initialize handshake, then
// tools/call returning the text content.
type Client struct {
	session *mcpsdk.ClientSession
}

// Connect performs the MCP initialize handshake over t.
func Connect(ctx context.Context, t mcpsdk.Transport) (*Client, error) {
	session, err := mcpsdk.NewClient(clientImpl, nil).Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("plaud: connecting to MCP server: %w", err)
	}
	return &Client{session: session}, nil
}

// Dial spawns command (split on whitespace; no shell quoting) and connects
// to it over stdin/stdout. The server's stderr is forwarded to stderr.
func Dial(ctx context.Context, command string, stderr io.Writer) (*Client, error) {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return nil, errors.New("plaud: empty MCP command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = stderr
	return Connect(ctx, &mcpsdk.CommandTransport{Command: cmd})
}

// CallTool invokes a tool and returns its text content joined by newlines.
// A tool-level error (isError) is returned as an error.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	res, err := c.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("plaud: %s: %w", name, err)
	}
	text := resultText(res)
	if res.IsError {
		return "", fmt.Errorf("plaud: %s: %s", name, firstLine(text))
	}
	return text, nil
}

// Close ends the session (and stops a spawned server).
func (c *Client) Close() error { return c.session.Close() }

func resultText(res *mcpsdk.CallToolResult) string {
	var parts []string
	for _, content := range res.Content {
		if t, ok := content.(*mcpsdk.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
