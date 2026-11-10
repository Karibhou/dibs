# Contributing to Dibs

Thanks for contributing! A few ground rules:

## Finding something to work on

- Issues labelled [`good first issue`](https://github.com/hivecommons/dibs/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) are scoped for a first PR; [`help wanted`](https://github.com/hivecommons/dibs/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22) issues are open to anyone.
- An improvement or new capability for Dibs starts with a [feature request](https://github.com/hivecommons/dibs/issues/new?template=feature_request.yml); [ROADMAP.md](ROADMAP.md) lists the near-term direction.
- Not sure where to start, or want to check an approach before writing code? Ask in the [Hive Commons Discord](https://hivecommons.dev/discord) or bring it to the biweekly [Hive Commons community meeting](https://docs.hivecommons.dev/docs/community/meetings).

## Developer Certificate of Origin (DCO)

All commits **must** be signed off, certifying the
[Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s -m "your message"
```

This adds a `Signed-off-by: Your Name <you@example.com>` trailer. PRs with
unsigned commits will not pass CI.

## Workflow

1. Fork / branch from `main`.
2. Make your change with tests.
3. Ensure `go build ./...`, `go vet ./...`, and
   `go test -race -count=1 -coverpkg=./... -coverprofile=coverage.out ./...`
   are green. CI also requires:
   - `gofmt -l .` prints nothing.
   - `go mod tidy` leaves `go.mod` and `go.sum` unchanged.
   - Total coverage (`go tool cover -func=coverage.out`) is at least 97%.
     The floor is enforced by the `Coverage summary` step in
     `.github/workflows/ci.yml`; when it is ratcheted, update this line too.
   - The `go` version in `go.mod` matches the `golang:` tag in the `Dockerfile`.
4. Open a PR with a clear description. Prow manages approvals via
   `/lgtm` and `/approve`.

## Code conventions

- Standard library first; no external DB — the JSON file store is deliberate.
- Every HTTP route must work behind the `DIBS_BASE_PATH` prefix.
- Private ideas must never appear in any listing other than the author's own —
  add a test if you touch listing code.

## Security

Do not file public issues for security vulnerabilities. See
[SECURITY.md](SECURITY.md) for how to report them privately.
