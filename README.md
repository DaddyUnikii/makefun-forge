# makefun-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/DaddyUnikii/makefun-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/DaddyUnikii/makefun-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge: one process that serves the four surfaces agents
already speak, from its own in-memory store, with no UI and no outside service.

- **git smart HTTP** at `/{owner}/{repo}.git` — upload-pack protocol v2, receive-pack v0
- **REST v3** under `/api/v3` — repositories, issues, pull requests and contents
- **GraphQL v4** at `/api/graphql`
- **MCP** at `/mcp` — streamable HTTP JSON-RPC 2.0

This is a make.fun competition entry for [`github-core-clone-01`](https://make.fun/bounty/github-core-clone-01).

## Build

The entry is a statically linked linux/amd64 Go binary, vendored as `./forge` (the grader runs
`node /bot/main.ts`, which spawns it). To rebuild it:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o forge .
```

The binary must stay under 8 MiB (the "statically linked binary you commit" allowance); the
`-s -w` strip keeps it around 6.3 MiB.

## Run

The binary reads `MAKEFUN_SHOP_TOKEN` (the bearer token every API call must carry) and
`MAKEFUN_ENV_URL` (the suite's origin). With `MAKEFUN_ENV_URL` unset it serves in practice mode
and prints the ephemeral port it bound. With it set it registers the port with the suite, polls
`/done`, and exits 0 when the trial is over.

## Structure

- `main.ts` — the grader's entrypoint; spawns `./forge` and forwards its lifecycle
- `main.go`, `store.go`, `rest.go`, `githttp.go`, `graphql.go`, `mcp.go`, `obj.go`, `tree.go`, `pack.go` — the forge
- `forge` — the vendored static binary