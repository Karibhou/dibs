# Dibs Roadmap

Dibs connects idea contributors to projects. A project can accept an idea;
**settlement** records the resulting GitHub issue URL, not a completed implementation.
This roadmap gives maintainers, ideators, and adopters a shared view of where
the project is headed next.

## Recently shipped

- End-to-end ideation, matching, and settlement flows for GitHub issue-backed
  work.
- Idea intake from common document formats, with limits on PDF text extraction
  and recovery when an uploaded document is malformed.
- Repository registry, credit-wall mechanics, notifications, and guided server
  flows covered by automated tests.
- A remote MCP endpoint at `https://dibs.hivecommons.dev/mcp`, so coding agents
  can submit and track ideas (`submit_idea`, `list_my_ideas`, `get_idea`,
  `list_repos`) and run the `recall_ideas` prompt. It is listed in the official
  MCP Registry as `dev.hivecommons/dibs`; see
  [Submit ideas from your agent](README.md#submit-ideas-from-your-agent) and
  [MCP Registry publishing](docs/mcp-registry.md).

## Near term

1. **Trust and abuse controls:** improve spam resistance for public idea intake,
   including clearer moderation states and rate/quality signals for idea contributors.
   See the [proposed idea-intake trust design](docs/idea-intake-trust.md);
   implementation follows in separate issues.
2. **Forge and repo coverage:** define what it takes to support issue hosts other than GitHub
   targets while growing the repository registry beyond hive-managed projects.
3. **Credit-wall integrations:** the [consumer API reference](docs/credit-wall-api.md)
   documents eligibility, settlement state, rewards, and current experimental
   stability levels. Next, define compatibility guarantees and stabilize the
   promises made to applications that consume this API.

## Deferred / non-goals

- Custodial payments or token issuance.
- Replacing project maintainers' review and merge authority.
- Accepting arbitrary unauthenticated uploads or anonymous settlement claims.
