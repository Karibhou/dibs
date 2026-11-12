package plaud

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultServer is the public Dibs origin.
	DefaultServer = "https://dibs.hivecommons.dev"
	// DefaultTags are attached to every ingested idea.
	DefaultTags = "plaud,voice-note"
	// EnvToken holds the hub bearer token (preferred over --token, which is
	// visible in the process list).
	EnvToken = "DIBS_TOKEN"
	// EnvServer overrides the default --server.
	EnvServer = "DIBS_URL"
	// EnvMCPCommand overrides the default --mcp-command.
	EnvMCPCommand = "DIBS_PLAUD_MCP_COMMAND"
)

// Main is `dibs ingest plaud`: it returns a process exit code (0 ok, 1 run
// failure, 2 usage error). getenv is injected for tests.
func Main(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("dibs ingest plaud", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: dibs ingest plaud [flags]\n\nPolls the Plaud MCP server for new recordings and submits each as a private draft Dibs idea.\nThe hub bearer token is read from $%s (or --token).\n\n", EnvToken)
		fs.PrintDefaults()
	}
	server := fs.String("server", orDefault(getenv(EnvServer), DefaultServer), "Dibs base URL (including any base path); $"+EnvServer)
	token := fs.String("token", "", "hub bearer token; prefer $"+EnvToken+" since flags are visible to other local users")
	interval := fs.Duration("interval", 0, "poll interval (e.g. 15m); 0 runs once, for cron/launchd")
	cursor := fs.String("cursor", "", "cursor file (default ~/.dibs/plaud-cursor.json)")
	command := fs.String("mcp-command", orDefault(getenv(EnvMCPCommand), DefaultCommand), "Plaud MCP server command (whitespace-separated); $"+EnvMCPCommand)
	sinceDays := fs.Int("since-days", DefaultSinceDays, "how many days back the first run looks")
	tags := fs.String("tags", DefaultTags, "comma-separated tags for ingested ideas")
	perHour := fs.Int("max-per-hour", DefaultMaxPerHour, "submissions allowed per rolling hour (0 = unlimited)")
	perDay := fs.Int("max-per-day", DefaultMaxPerDay, "submissions allowed per rolling 24h (0 = unlimited)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "dibs ingest plaud: unexpected argument: %q\n", fs.Arg(0))
		return 2
	}
	tok := orDefault(*token, getenv(EnvToken))
	if tok == "" {
		fmt.Fprintf(stderr, "dibs ingest plaud: a hub bearer token is required: set $%s\n", EnvToken)
		return 2
	}
	u, err := url.Parse(*server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintf(stderr, "dibs ingest plaud: --server must be an http(s) URL, got %q\n", *server)
		return 2
	}
	path := *cursor
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(stderr, "dibs ingest plaud: locating home directory for the cursor file: %v\n", err)
			return 1
		}
		path = filepath.Join(home, ".dibs", "plaud-cursor.json")
	}
	in := &Ingester{
		Dial: func(ctx context.Context) (Tools, error) {
			c, err := Dial(ctx, *command, stderr)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		Sink:       &DibsSink{Endpoint: strings.TrimRight(*server, "/") + "/mcp", Token: tok},
		CursorPath: path,
		Tags:       splitTags(*tags),
		SinceDays:  *sinceDays,
		MaxPerHour: *perHour,
		MaxPerDay:  *perDay,
		Log:        slog.New(slog.NewTextHandler(stderr, nil)),
	}
	if err := in.Run(ctx, *interval); err != nil {
		fmt.Fprintf(stderr, "dibs ingest plaud: %v\n", err)
		return 1
	}
	return 0
}

func splitTags(s string) []string {
	var out []string
	for _, tag := range strings.Split(s, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
