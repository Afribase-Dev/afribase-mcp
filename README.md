# Afribase MCP

A standalone [Model Context Protocol](https://modelcontextprotocol.io) server
for [Afribase](https://useafribase.app) — an open-source backend-as-a-service
platform. It lets an assistant (Claude, or any MCP client) list your
projects, read your tables, tail an app's logs and ship a deploy, using your
own Afribase account.

This is a bridge, not a copy of Afribase's internals: every tool call turns
into a plain HTTPS request against Afribase's public REST API
(`api.useafribase.app` by default), using the same access token the
[Afribase CLI](https://github.com/Afribase-Dev/afribase-cli) uses. There is
no database connection, no internal service import, nothing that only works
inside Afribase's own infrastructure — you can read every line of what a
tool call actually does.

Afribase also runs its own hosted, multi-tenant connector at
`mcp.afribase.dev`, with a full OAuth 2.1 authorization flow so you can add
it to Claude in one click with no token to manage. This repo is the
self-hosted alternative: one process, one token, run it yourself.

## Install

```bash
go install github.com/afribase/mcp@latest
```

Or build from source:

```bash
git clone https://github.com/Afribase-Dev/afribase-mcp
cd afribase-mcp && go build -o afribase-mcp .
```

## Run

```bash
export AFRIBASE_ACCESS_TOKEN=...   # from `afribase login` (CLI), or the dashboard
export AFRIBASE_API_URL=https://api.useafribase.app   # optional, this is the default
export PORT=8090                                       # optional, this is the default

afribase-mcp
```

Point your MCP client at `http://localhost:8090/mcp`.

### Scoping a connection

Query parameters on the URL limit what a connection can do — the same
scheme Afribase's hosted connector uses:

| Parameter | Effect |
| --- | --- |
| `?read_only=true` | No tool that changes anything is offered. |
| `?project=my-shop` | Pinned to one project; account-level tools (listing/creating projects) are hidden. |
| `?features=database,hosting` | Only tools in those groups. Groups: `account`, `database`, `hosting`, `storage`, `functions`, `branching`. |

Combine them: `http://localhost:8090/mcp?read_only=true&project=my-shop`.

## Tools

25 tools across six groups — projects and account (`account`), tables and
migrations (`database`), apps and deploys (`hosting`), buckets and objects
(`storage`), edge functions (`functions`), database branches (`branching`),
plus `search`/`fetch` for clients that only support those two. Every tool's
full description and input schema is in [`tools.go`](./tools.go); ask your
MCP client to list them, or read the source — it's short.

Two of the twenty-five write anything: `trigger_deploy` and
`apply_migration`. `run_sql` is read-only by construction — it wraps
whatever you send in a `SELECT ... LIMIT`, so a write statement is a syntax
error, not a mistake that reaches the database. `apply_migration` refuses
anything that touches a schema the platform itself owns (`auth`, `storage`,
`realtime`, and so on) — see `touchesPlatformSchema` in
[`tools.go`](./tools.go).

## Why a separate repo from the hosted connector

Afribase's own `mcp.afribase.dev` is implemented inside the orchestrator
service itself, sharing its database access and internal service layer
directly — fast, but not something you could run standalone without
shipping most of the orchestrator with it. This bridge is the other shape:
slower (one HTTP hop further), but every byte of it is here, and it works
against any Afribase deployment your token can reach, including a
self-hosted one.

## License

Apache 2.0 — see [LICENSE](./LICENSE).
