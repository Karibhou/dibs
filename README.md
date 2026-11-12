<p align="center">
  <img src="docs/assets/dibs-logo.svg" alt="Dibs" width="120" height="120">
</p>

# Dibs

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

A place to offer software ideas to projects and retain credit for them.

> **Your idea, your credit, their code.**
>
> The missing contributor layer for the AI-agent era.

Dibs is a two-sided marketplace that connects **ideators** — people who call
dibs on ideas: great insight, but no time, tooling, or inclination to write
code — with **hive-managed repositories**: projects that use Hive to run
coding agents under maintainer-chosen rules.

1. **Ideators post ideas** — public or private — in plain language / markdown.
2. **Repos opt in** to receive ideas, declaring topics and appetite.
3. **A language model (LLM) matches** ideas to repos that want them; both the
   idea contributor and the receiving project must accept the match.
4. On **mutual acceptance**, Dibs hands the ideator a **prefilled GitHub
   issue** — they file it themselves, under their own GitHub account, so the
   issue is natively attributed to them (label `ideated`). The receiving project
   can then have its agents work on it under its own review rules. In this
   default flow Dibs is just the matchmaker: it never files the
   issue for you.

## Ideators are first-class contributors

Open source has always had a quiet contributor class with no outlet:
researchers, operators, designers, power users — people who know exactly what
a project needs but will never open a pull request. Open source has also
always known ideas are contributions — the all-contributors spec has a 💡
"ideas" emoji, and the academic CRediT taxonomy credits "Conceptualization"
as a first-class role. Dibs makes that credit real, machine-readable, and
public.

**Creators get credit. Agents do the work. Projects take the bow.**

Every implemented idea traces back to its ideator: the GitHub issue credits
the idea's author, the resulting work carries that provenance, and the public
**credit wall** (`/api/credits`, "Credit wall" in the UI) lists every ideator
with a settled idea: a GitHub issue URL has been recorded. This credits the
proposal, not proof that its implementation shipped or its issue was closed.
Settled ideas appear regardless of their visibility setting: settling a private
idea makes its credit facts (author, title/TLDR, target, issue URL, and time)
public, but not its body or private detail endpoint.

## Try Dibs in your browser

No local installation or Go toolchain is needed. Open
[dibs.hivecommons.dev](https://dibs.hivecommons.dev) to browse the public
surface; posting needs a Hive Hub sign-in and filing the issue needs a GitHub account.

1. Sign in and describe your idea in plain language. Choose public or private
   deliberately; unsettled private ideas are not included in the public wall.
2. Review suggested repositories and offer the idea to a suitable project.
   For Hive-managed targets, both sides accept before issue filing.
3. Open the prefilled GitHub issue, review it, and file it yourself.
4. Confirm the resulting issue URL in Dibs. This step is **settlement**:
   it records the handoff, not completion of the proposed work. Confirming also
   publishes the credit facts above, even for an idea marked private.

Non-Hive GitHub targets can skip the receiving-project acceptance step.
For settlement and privacy details, see the [consumer API](docs/credit-wall-api.md).
To submit from a coding agent instead, see [MCP setup](#submit-ideas-from-your-agent).

## Maintainer and developer reference

The following sections cover running Dibs, its API, and local development,
not prerequisites for using the hosted site.

## Architecture

Single Go binary (`cmd/dibs`) serving both the JSON API and an embedded
static UI, deployed at its own subdomain: **dibs.hivecommons.dev**. The base
path is configurable (`DIBS_BASE_PATH`), so path-prefixed reverse-proxy
deployments also work.

| Package | Purpose |
|---|---|
| `pkg/server` | HTTP server, base-path routing, embedded UI |
| `pkg/auth` | Auth bridge — validates the hive hub session cookie against the hub |
| `pkg/store` | Idea model + JSON file store (atomic writes, no external DB), offer/settlement state machine |
| `pkg/fsutil` | Shared atomic JSON persistence helper (temp file + rename, mode 0600) used by the file stores |
| `pkg/api` | Idea CRUD + matching/offer/feed/decide/notification API (author-scoped, private ideas never leak) |
| `pkg/registry` | Hive-managed repo profiles + "accepting ideas" opt-in |
| `pkg/match` | LLM idea↔repo scoring via litellm gateway, cached TLDRs, "✨ Embellish" draft refinement, deterministic keyword fallback |
| `pkg/settle` | Settlement — prefilled GitHub new-issue URL the ideator files themselves (`ideated` label, 🐝 Dibs footer, URL-length budget) + issue-URL confirmation; legacy token mode. Ideas can target ANY GitHub repo: non-hive targets skip acceptance and their issues carry a "request a hive" growth CTA |
| `pkg/notify` | In-app notification feed (bell): matches, offers, decisions, issues |
| `pkg/mcpserver` | Streamable HTTP MCP endpoint exposing Dibs tools (see [Submit ideas from your agent](#submit-ideas-from-your-agent)) |
| `pkg/intake` | Extracts idea text from uploaded documents or transcribed audio |
| `pkg/game` | Gamification layer — points, levels, badges — recomputed from stored idea state, never a mutable counter |
| `pkg/catalog` | Builds and searches a separate CNCF project catalog |
| `pkg/history` | Backfills public GitHub activity for hive-managed repos; persists daily counts for repo index charts |
| `pkg/indexformula` | Weighted composite behind the repo market index |
| `pkg/news` | Turns recent merged PRs into repo news cards |
| `pkg/deps` | Single place declaring the subsystem dependencies the HTTP server, JSON API, and MCP endpoint are wired with |

### Configuration

| Env var | Default | Meaning |
|---|---|---|
| `DIBS_BASE_PATH` | `/` | Base path every route/asset is served under (set e.g. `/ideas` for path-prefixed proxying) |
| `DIBS_ADDR` | `:8080` | Listen address |
| `DIBS_METRICS_ADDR` | (unset) | Internal Prometheus `/metrics` listener (e.g. `:9090`); unset keeps it off. See [`deploy/`](deploy/README.md) |
| `HUB_URL` | `https://hive.hivecommons.dev` | Hive hub for session validation & repo sync |
| `DATA_DIR` | `/data` | JSON store directory |
| `REPOS_SEED_FILE` | (unset) | Static JSON seed of repo profiles for dev/demo |
| `DIBS_LLM_BASE_URL` | (unset) | OpenAI-compatible gateway (hive litellm), e.g. `http://litellm:4000/v1`. Unset → deterministic keyword matcher |
| `DIBS_LLM_API_KEY` | (unset) | Bearer token for the LLM gateway |
| `DIBS_LLM_MODEL` | `gpt-4o-mini` | Model name routed by the gateway |
| `DIBS_GITHUB_TOKEN` | (unset) | **Legacy mode only** for issue creation: when set, Dibs opens the credited issue server-side on accept. Unset (default), the ideator files a prefilled GitHub issue themselves — native attribution. Also used, when present, to authenticate the GitHub lookup that verifies a confirmed issue URL (anonymous otherwise) |
| `DIBS_ADMINS` | (unset) | Comma-separated, case-insensitive GitHub logins granted admin access (idea-moderation endpoints) |
| `DIBS_CLANKER_MARKERS` | `hive: agent=,hivecommons-hive[bot],kubestellar-hive[bot]` | Comma-separated, case-insensitive markers matched against PR branch/labels/body/author to classify agent activity |
| `DIBS_INTAKE_MAX_MB` | `25` | Max upload size (MB) accepted for idea-intake file uploads |
| `DIBS_INTAKE_EXTRACT_WORKER` | _(on)_ | Set to `off` to extract uploaded documents in-process instead of in a memory-limited, time-limited child worker (not recommended: crafted documents can then exhaust the server's memory) |
| `DIBS_STT_URL` | (unset) | Speech-to-text gateway URL; when set, enables voice/audio idea submission |
| `DIBS_STT_KEY` | (unset) | API key for the STT gateway |
| `DIBS_STT_MODEL` | (unset) | Model name routed by the STT gateway |

Legacy `IDEATE_*` names (the pre-rename prefix) are still honored as
fallbacks for every `DIBS_*` variable.

### Roadmap

See [ROADMAP.md](ROADMAP.md) for what is planned next. Delivery history:

- **Wave 1 (done):** scaffold, hub auth bridge, idea CRUD, repo registry.
- **Wave 2 (done):** LLM matching + TLDRs, swipe UX ("offer" / "pass"), issue settlement (credited GitHub issues), notifications.
- **Wave 3 (done):** deployment (GHCR image + [`deploy/` manifests](deploy/README.md) for dibs.hivecommons.dev), public landing page, public credit wall, ideator profile stats.
- **Wave 4 (done):** matchmaker settlement — the ideator files a prefilled GitHub issue themselves (native attribution, `ideated` label) and confirms the issue URL; LLM-assisted idea refinement on posting and repo-tailored expansion on submission; token-based settlement demoted to legacy.
- **Later:** cookie-domain/hub-OAuth session sharing, non-hive receptor repos, idea economics.

## Deployment

Every push to `main` publishes `ghcr.io/hivecommons/dibs` (`latest` +
commit sha) via `.github/workflows/docker.yml`; the image is distroless and
runs as non-root. Plain Kubernetes manifests for **dibs.hivecommons.dev**
(Deployment, Service, cert-manager TLS Ingress, PVC, ConfigMap/Secret) live
in [`deploy/`](deploy/README.md), including the required DNS record and
hub-cookie prerequisites.

## Public surface (no sign-in required)

| Route | Purpose |
|---|---|
| `GET /` | Landing page (pitch + sign-in) — signed-in users get the full app |
| `GET /api/credits` | Credit wall data: settled ideas only (handle, title, TLDR, repo, issue URL) |
| `GET /api/leaderboard` | Aggregate ideator scores/levels/badges |
| `GET /api/ticker` | Public idea ticker tape for the logged-out landing page |
| `GET /api/board` | Live-markets board shown on the logged-out landing page |
| `GET /api/stats` | Aggregate, non-identifying counts |
| `GET /api/repos/{org}/{repo}/index` | Repo value-index chart series |
| `GET /api/repos/{org}/{repo}/news` | Repo news feed |
| `GET /api/repos/{org}/{repo}/qr.png` | Repo QR code |
| `GET /healthz` | Liveness check + embedded git version |
| `GET /readyz` | Readiness check — verifies the data store is accessible/writable |

Everything else requires a hive hub session; signed-in ideators additionally
get `GET /api/me/stats` (ideas posted / offered / accepted / settled).

For response schemas, eligibility/privacy rules, settlement actions, rewards,
and stability levels, see the [credit-wall consumer API](docs/credit-wall-api.md).

## Submit ideas from your agent

Dibs exposes a stateless Streamable HTTP MCP endpoint at
`https://dibs.hivecommons.dev/mcp`, so Claude Code, Copilot CLI, Cursor,
Gemini CLI, Codex, and other MCP clients can submit ideas without leaving the agent.

Dibs is listed in the official MCP Registry as `dev.hivecommons/dibs`
([registry entry](https://registry.modelcontextprotocol.io/v0.1/servers?search=dev.hivecommons%2Fdibs)).
Search for that full name: a plain `dibs` search also returns unrelated
servers with the same name, including the 1stDibs marketplace and other
coding-agent tools called dibs. How the listing is published is covered in
[MCP Registry publishing](docs/mcp-registry.md).

Available tools:

| Tool | Auth | Purpose |
|---|---|---|
| `submit_idea(title, description, tags?)` | Required | Creates a private draft idea owned by you and returns its id and API URL |
| `list_my_ideas()` | Required | Lists your ideas with lifecycle phase/status |
| `get_idea(id)` | Optional for public ideas; required for private ideas (author, or owner of a repo holding a pending or accepted offer) | Shows detail, matches, offers, and settlement state |
| `list_repos()` | Not required | Lists registered repo venues |

### Token

Use `Authorization: Bearer <token>`. Dibs first asks the Hive Hub
`/api/saas/whoami` endpoint to verify the bearer token server-to-server; the
hub already accepts GitHub bearer tokens for registered hub users. If that is
not practical, copy the raw `hive_hub_user` cookie value from your browser
after signing in at `https://hive.hivecommons.dev`; Dibs falls back to verifying
that value through the same hub `whoami` cookie flow used by the web UI.

In Chrome/Edge: open DevTools → Application → Cookies →
`https://hive.hivecommons.dev` (or `https://dibs.hivecommons.dev`) → copy the
`hive_hub_user` value.

### Claude Code

```sh
claude mcp add --transport http dibs https://dibs.hivecommons.dev/mcp \
  --header "Authorization: Bearer <your-token-or-hive_hub_user-cookie-value>"
```

### Claude Desktop and ChatGPT connectors

Both accept a remote MCP server over Streamable HTTP, so the same endpoint
works without a local process.

Claude Desktop: Settings -> Connectors -> Add custom connector, with the URL
`https://dibs.hivecommons.dev/mcp`. Where the connector UI allows a custom
header, set `Authorization: Bearer <token>`; the read-only tools work without
one, and `submit_idea` needs it.

ChatGPT: Settings -> Connectors -> Add custom connector (available on plans
where custom MCP connectors are enabled), using the same URL and header.

### GitHub Copilot CLI and VS Code

**Copilot CLI** has a one-line command to add a remote MCP server:

```sh
copilot mcp add --transport http \
  --header "Authorization: Bearer <your-token-or-hive_hub_user-cookie-value>" \
  dibs https://dibs.hivecommons.dev/mcp
```

This writes to `~/.copilot/mcp-config.json` using the same `mcpServers` / `type: "http"` shape as the Generic MCP JSON block below.

**VS Code** reads `~/.copilot/mcp-config.json` (shared with Copilot CLI) and workspace `.mcp.json` files. Both use the same `mcpServers` format. For more details, see the [Copilot CLI documentation](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers) and [VS Code MCP server documentation](https://code.visualstudio.com/docs/agent-customization/mcp-servers).

### Cursor

Cursor reads MCP configuration from `~/.cursor/mcp.json` (global) or `.cursor/mcp.json` (project-level). Use the `mcpServers` format with `url` and `headers`:

```json
{
  "mcpServers": {
    "dibs": {
      "url": "https://dibs.hivecommons.dev/mcp",
      "headers": {
        "Authorization": "Bearer ${env:DIBS_TOKEN}"
      }
    }
  }
}
```

Cursor supports `${env:NAME}` interpolation in headers, so you can set `DIBS_TOKEN` as an environment variable rather than storing it in the file. For details, see the [Cursor MCP documentation](https://cursor.com/docs/mcp).

### Gemini CLI

```sh
gemini mcp add --scope user --transport http \
  --header "Authorization: Bearer <your-token-or-hive_hub_user-cookie-value>" \
  dibs https://dibs.hivecommons.dev/mcp
```

This adds the server to `~/.gemini/settings.json`; drop `--scope user` to add
it to the project's `.gemini/settings.json` instead. For details, see the
[Gemini CLI MCP documentation](https://github.com/google-gemini/gemini-cli/blob/main/docs/tools/mcp-server.md).

### Codex

Codex (CLI and IDE extension) reads MCP servers from `~/.codex/config.toml`.
`bearer_token_env_var` keeps the token out of the file and sends it as
`Authorization: Bearer <token>`:

```toml
[mcp_servers.dibs]
url = "https://dibs.hivecommons.dev/mcp"
bearer_token_env_var = "DIBS_TOKEN"
```

Then `export DIBS_TOKEN=<your-token-or-hive_hub_user-cookie-value>` before
starting Codex. For details, see the [Codex MCP documentation](https://developers.openai.com/codex/mcp).

### Generic MCP JSON

```json
{
  "mcpServers": {
    "dibs": {
      "type": "http",
      "url": "https://dibs.hivecommons.dev/mcp",
      "headers": {
        "Authorization": "Bearer <your-token-or-hive_hub_user-cookie-value>"
      }
    }
  }
}
```

## Recall ideas you never acted on

Dibs ships an MCP prompt, `recall_ideas`, for the ideas that never made it to
a form: the ones mentioned in passing and forgotten. Dibs cannot find those
itself - only the assistant holding your history can - so the prompt asks it
to.

It instructs the assistant to search its memory and your past conversations
for product ideas, feature wishes, requirements and specs you described but
never acted on, report each with enough context to act on, and then ask which
ones to list. It submits only what you pick.

| Argument | Purpose |
|---|---|
| `scope` | Optional focus - a project, a repo, a period like "the last year". Omit to search everything available. |
| `limit` | Maximum candidates to report. Defaults to 10, capped at 50. |

In Claude Code and Claude Desktop the prompt appears as a slash command once
the connector is added. Clients that do not surface prompts in their UI can
still call `prompts/get` directly.

## Capture voice notes from a Plaud recorder

`dibs ingest plaud` turns Plaud Note / Note Pro recordings into private draft
ideas, unattended and at no cost beyond the Plaud subscription. It talks to the
official, free Plaud MCP server (`npx -y @plaud-ai/mcp@latest`, over stdio),
lists recordings since its cursor, fetches each one's Plaud summary and
transcript, and submits them with the `submit_idea` tool (tags `plaud,voice-note`).

```sh
# Once: sign in to Plaud through its MCP server (the token stays in ~/.plaud).
# Then, with your hub token (see "Token" above):
export DIBS_TOKEN=...
dibs ingest plaud                    # one pass, for cron/launchd
dibs ingest plaud --interval 15m     # or keep polling
```

| Flag | Default | Purpose |
|---|---|---|
| `--server` | `$DIBS_URL` or `https://dibs.hivecommons.dev` | Dibs base URL (including any base path) |
| `--token` | `$DIBS_TOKEN` | Hub bearer token; prefer the env var, flags show up in `ps`. Never logged |
| `--interval` | `0` | Poll interval; `0` runs once |
| `--cursor` | `~/.dibs/plaud-cursor.json` | Submitted Plaud ids, newest recording time, recent submission times |
| `--mcp-command` | `$DIBS_PLAUD_MCP_COMMAND` or `npx -y @plaud-ai/mcp@latest` | Plaud MCP server command (whitespace-separated, no shell quoting) |
| `--since-days` | `7` | How far back the first run looks |
| `--tags` | `plaud,voice-note` | Comma-separated tags |
| `--max-per-hour` / `--max-per-day` | `10` / `30` | Submission budget per rolling hour / 24h (`0` = unlimited) |

Each recording is submitted once: its Plaud id is recorded in the cursor file
right after the submission succeeds. Recordings with no summary or transcript
yet are retried on the next run. Long transcripts are fetched page by page
(following the server's `next_cursor`). When the budget is used up the run
logs `budget_exhausted=true`; when Dibs answers HTTP 429 it logs
`rate_limited=true`; either way the run stops and the remaining recordings
wait for the next run. Descriptions are cut to the 64 KiB idea body limit. The helper never reads or
copies Plaud's OAuth token (`~/.plaud/tokens-mcp.json`).

Cron (every 15 minutes):

```cron
*/15 * * * * DIBS_TOKEN=... /usr/local/bin/dibs ingest plaud >>"$HOME/.dibs/plaud.log" 2>&1
```

launchd (`~/Library/LaunchAgents/dev.hivecommons.dibs-plaud.plist`, load with
`launchctl load` on that file):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.hivecommons.dibs-plaud</string>
  <key>ProgramArguments</key><array>
    <string>/usr/local/bin/dibs</string><string>ingest</string><string>plaud</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>DIBS_TOKEN</key><string>...</string>
    <key>PATH</key><string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>
  </dict>
  <key>StartInterval</key><integer>900</integer>
</dict></plist>
```

## Operations and further reading

- [MCP Registry publishing](docs/mcp-registry.md): listing manifest and operator steps for DNS authentication, publication, and verification.
- [Runbooks](runbooks/): [incident response](runbooks/incident-response.md), [release rollback](runbooks/release-rollback.md), [data recovery](runbooks/data-recovery.md), [SLOs](runbooks/slo.md), and a [postmortem template](runbooks/postmortem-template.md).
- [CONTRIBUTING.md](CONTRIBUTING.md): DCO sign-off, CI requirements, and code conventions.
- [Idea intake trust and abuse controls](docs/idea-intake-trust.md): a proposed design, not yet implemented.

## Development

Requires Git and Go 1.27 or newer. Clone
`https://github.com/hivecommons/dibs.git`, then run these commands from the
checkout's top-level `dibs` folder. For authenticated local use, follow
[Running locally with sign-in](#running-locally-with-sign-in); fakehub is
for development only, not a production authentication service.

```sh
go test -race -count=1 ./...
go run ./cmd/dibs            # serves on :8080 at /
go build -o dibs ./cmd/dibs  # builds the binary (plain `go build ./...` writes none)
./dibs --version             # prints the embedded commit
```

### Running locally with sign-in

Every route except the [public surface](#public-surface-no-sign-in-required)
needs a hive hub session. For local work, [`hack/fakehub`](hack/fakehub/main.go)
is a dev-only stand-in for the hub: it accepts any non-empty `hive_hub_user`
cookie as that user and serves a small static repo list. Run it in one
terminal, then point Dibs at it in another:

```sh
go run ./hack/fakehub        # listens on :9999 (override with FAKEHUB_ADDR)
HUB_URL=http://127.0.0.1:9999 DATA_DIR=./data go run ./cmd/dibs
curl -H 'Cookie: hive_hub_user=dev' http://127.0.0.1:8080/api/me
```

To load richer repo profiles than fakehub's two entries, also set
`REPOS_SEED_FILE=hack/seed-repos.example.json`.

### Smoke-testing the container image

`docker build` only proves the image compiles. [`hack/smoke.sh`](hack/smoke.sh)
runs it the way [`deploy/deployment.yaml`](deploy/deployment.yaml) does
(uid 65532, read-only root filesystem, only `/data` writable, all
capabilities dropped) and checks `--version` is stamped, `/healthz` and
`/readyz` answer 200, the landing page serves with `HUB_URL` substituted
and the security headers set, the auth wall answers 401/502 around the
public API, `/metrics` exposes the `dibs_*` series the alerts query
(labelled by the route groups just exercised), and SIGTERM drains to
exit 0:

```sh
docker build --build-arg GIT_HASH="$(git rev-parse HEAD)" -t dibs:smoke .
hack/smoke.sh dibs:smoke "$(git rev-parse HEAD)"
```

## Contributing & Community

Dibs is part of [Hive Commons](https://hivecommons.dev), a family of tools for working with AI agents.

- Want to contribute? See [CONTRIBUTING.md](CONTRIBUTING.md) for DCO sign-off, the CI and coverage gates, and code conventions.
- Found a bug? Open a [bug report](https://github.com/hivecommons/dibs/issues/new?template=bug_report.yml). Have an idea for Dibs itself? Open a [feature request](https://github.com/hivecommons/dibs/issues/new?template=feature_request.yml).
- Reporting a security vulnerability? Follow [SECURITY.md](SECURITY.md) to report it privately, not through public issues.
- Questions? Join the [Hive Commons Discord](https://hivecommons.dev/discord), or watch the hive live on [Twitch](https://www.twitch.tv/hivecommons). To talk to the maintainers live, join the biweekly [Hive Commons community meeting](https://docs.hivecommons.dev/docs/community/meetings).
- Please follow the [Hive Commons Code of Conduct](https://github.com/hivecommons/.github/blob/main/CODE_OF_CONDUCT.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).

Handshake icon: [Twemoji](https://github.com/twitter/twemoji), [CC-BY 4.0](https://creativecommons.org/licenses/by/4.0/).
