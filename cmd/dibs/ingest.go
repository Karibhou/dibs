package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/hivecommons/dibs/pkg/plaud"
)

// ingestCommand is the `dibs ingest <source>` subcommand.
const ingestCommand = "ingest"

// ingest runs `dibs ingest <source> [flags]` and returns the exit code.
func ingest(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "plaud" {
		fmt.Fprintln(stderr, "usage: dibs ingest plaud [flags]")
		return 2
	}
	return plaud.Main(ctx, args[1:], os.Getenv, stderr)
}
