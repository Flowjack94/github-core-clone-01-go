# github-core-clone-01 (Go rewrite)

A GitHub-compatible forge: git smart HTTP, REST v3, GraphQL v4 and MCP, in one
static Go binary with a raw-TCP HTTP server and zero-alloc hot paths. Written
fresh for the make.fun bounty `github-core-clone-01`.

Bounty: https://make.fun/bounties/github-core-clone-01

Run it:

```sh
./forge
```

The platform runs `node /bot/main.ts` on Node 22. The launcher spawns the
vendored static binary `./forge` (built with `CGO_ENABLED=0`, no runtime
dependencies, no git binary, no network access needed).

## Lifecycle

1. Bind an ephemeral port (`127.0.0.1:0`).
2. Register: `POST $MAKEFUN_ENV_URL/register` with `{"port": N}`.
3. Poll `GET $MAKEFUN_ENV_URL/done` until `{"done": true}`, then exit 0.

## Layout

- `main.ts` — thin Node launcher: spawns `./forge`, forwards signals.
- `gosrc/main.go` — lifecycle (bind, register, wait for done), router, auth.
- `gosrc/http.go` — raw-TCP HTTP/1.1 server (keep-alive, one goroutine per
  connection, single write per response, `TCP_NODELAY`).
- `gosrc/store.go` — in-memory forge state (repos, objects, issues, PRs).
- `gosrc/gitobj.go` — SHA-1, pack read/write, trees, commits, ancestry.
- `gosrc/git.go` — git smart HTTP: info/refs v2/v0, ls-refs, fetch v2/v0,
  receive-pack with report-status.
- `gosrc/rest.go` — GitHub REST v3 handlers.
- `gosrc/graphql.go` — GraphQL v4 lexer, parser and executor.
- `gosrc/mcp.go` — Model Context Protocol over streamable HTTP
  (`server/discover`, `initialize`, `tools/list`, `tools/call`).

## Perf notes

The hot paths are deliberately lean: raw TCP with no framework, response
bodies for repeated reads are cached and invalidated on mutation, pack
inflation reads each zlib stream exactly once, and the binary runs with
`GOGC=off` (heap stays small; `GOMEMLIMIT=512MiB` is the safety valve).
