# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
This working copy is a **local fork** of https://github.com/router-for-me/CLIProxyAPI. Four names, four fixed roles — do not blur them:

| Name | What it is | Fetch from | Push to |
| --- | --- | --- | --- |
| `upstream` | the author's repo (router-for-me) | yes — the only sync source | **never** |
| `origin` | our GitHub fork (CastorFlow), a backup mirror | no | yes |
| `main` | a clean mirror of `upstream/main` | `upstream` | `origin` |
| `feat/local-fork` | **the only branch we write code on** | — | `origin` (force-with-lease after a rebase) |

- `main` is deploy-only and mirrors upstream exactly: it never holds a fork commit, so the only thing to do with it is fast-forward it to `upstream/main`.
- `origin` is a GitHub fork of `upstream`, not a sync source. It is pushed to, never pulled from — pulling it would drag history backwards.
- `feature/local-fork` is private. A rebase rewrites its SHAs, so pushing it needs `--force-with-lease`. That is expected, not a problem.
- `rerere.enabled` and `rerere.autoupdate` are on. Upstream lands ~16 commits/day and the same two files conflict every sync; rerere replays the earlier resolution instead of re-deriving it. Keep them on.
- Day-to-day commands below are unchanged for the fork; syncing and deployment are in "Working on the fork".

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/home/` — CLIProxyAPIHome control plane integration (bootstrap, RESP communication, dispatch coordination)
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `plugins/` — gitignored local workspace for `deploy.sh` and the KeyManageCenter plugin (its own git repo); not part of the upstream build
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Payload configuration MUST be the final semantic barrier before sending requests in every executor, including streaming, WebSocket, continuation, retry/fallback, image, and token-count paths where applicable. Complete all built-in payload translation, normalization, injection, and cleanup first, then evaluate and apply user payload rules exactly once to the final business payload. No subsequent logic may overwrite configured values or restore filtered fields. Only necessary transport framing, serialization, signature calculation, and read-only validation may follow without changing business payload semantics. Add regression tests when introducing or changing request-building paths to enforce this ordering.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, the operator-configured `requests.timeouts` bounds enforced in `sdk/cliproxy/auth/request_timeout.go` (see "Local fork conventions"), and the `cmd/fetch_antigravity_models` utility timeouts
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
- Note: if modifying features that involve CLIProxyAPIHome, check if corresponding updates are needed in the CLIProxyAPIHome repository.
- Endpoints under the `/v0/management` base URL are deprecated and no longer maintained. For any feature changes, do not modify endpoints under `/v0/management` unless necessary to fix compilation errors.

## Working on the fork

Fork commits must stay a small, self-contained set of patches so a rebase onto upstream stays cheap.

### Sync loop

Three steps, in this order. Never `git pull` on either branch: on `main` it would fetch from the wrong place, and on `feat/local-fork` it would merge instead of rebase, which drags upstream's 300+ commits into a merge bubble and makes the fork history unreadable.

```bash
# 1. sync — read what upstream landed
git fetch upstream --prune

# 2. mirror main (it holds no fork commits, so this is always a fast-forward)
git checkout main && git merge --ff-only upstream/main && git checkout feat/local-fork

# 3. replay the fork patches onto the new base
git rebase upstream/main            # or `git rebase main`, now equivalent
```

Step 3 rewrites the SHAs of the fork commits, so after a rebase:

```bash
git push --force-with-lease origin feat/local-fork
```

There are no script wrappers for this. The rebase is driven by hand (or by Claude) because a conflict here needs judgment, not a shell one-liner; the two files that conflict every time are `internal/config/sdk_config.go` (adjacent inserts next to upstream's `tls-handshake-timeout-seconds` block) and `sdk/cliproxy/auth/conductor_stream.go` (upstream keeps rewriting the bootstrap read loop that the fork wraps with `timeouts.read`). On that second file, **keep upstream's semantics and layer the timeout on top** — upstream's "prefer an already-ready chunk so a terminal error is not lost to cancellation" is the same class of hazard the tracker's `delivered` flag guards, and both must survive.

### Patch rules

- Keep fork-only code in **new files** (`internal/config/request_timeouts.go`, `sdk/cliproxy/auth/request_timeout.go`) rather than restructuring upstream files.
- Upstream-file changes should be **additive one-liners** (a new struct field, a call site). Avoid reindenting, reformatting, or moving upstream code — that turns every later rebase into a rewrite.
- Table-driven work that can be expressed as config (like `requests.timeouts` rules) is preferred over per-provider code paths.
- Rebase-then-deploy is one flow, but they are separate decisions: a sync only has to pass the checks below, and deployment is a deliberate second step.
- After a rebase always run the checks in "Verification before deploying", then redeploy.

### Deploy points

Deployment builds exactly the working tree of `feat/local-fork`, and `deploy.sh` refuses to run unless that tree is clean and on that branch. Tag a deploy when it matters so a rollback and an "which upstream commit is this based on" question are both answerable:

```bash
git tag deploy/2026-10-24 && git push origin deploy/2026-10-24
```

The binary's version string (`deploy.sh` injects `git describe --tags --always`) already records the base; the tag records the fork state on top of it.

## Local fork conventions

- **`requests.timeouts`** — the three operator-configurable upstream request bounds described in `config.example.yaml`: stream first-byte, stream idle, and non-streaming total, each with optional per-model/protocol/provider rules. Resolution and clamping live in `internal/config/request_timeouts.go`; enforcement lives in `sdk/cliproxy/auth/request_timeout.go` and its call sites in `conductor_stream.go` / `conductor_execution.go` / `conductor_home_execution.go`.
  - These bounds are wall-clock and **request-scoped**: a timeout must never cool down or rotate the credential.
  - Only data chunks the client can observe count as activity. Upstream keep-alives and control frames must not reset a bound.
  - Enforce timeouts inside the existing read loop (see `streamTimeoutTracker`); do not insert an extra relay channel or goroutine into the streaming path — an extra hop perturbs retry/websocket behavior in ways the upstream tests catch.
  - Config ranges are part of the contract: clamps on load in core, hard validation in the panel, and both must move together.
- **Config hot reload** — the watcher watches the config *file* and its *directory* (`internal/watcher/events.go`). The directory watch is required because atomic replace (temp file + rename) detaches a file watch from the replaced inode. Keep it; without it the panel and CPA's own saves stop reloading after the first write.
- **KeyManageCenter plugin** — a separate git repo at `plugins/key-manage-center/` (gitignored by this repo, so upstream pulls never touch it). It is not built by `go build ./...`; use `make check` inside that directory.
- **`plugins/deploy.sh`** build+deploy entry point. It cross-compiles `cmd/server` (linux/arm64, CGO) and the plugin, copies both to the deployment host, then restarts the service. Run it only from `feat/local-fork` with a clean tree.

## Verification before deploying

```bash
gofmt -l . && go build ./... && go vet ./internal/config/ ./internal/watcher/ ./sdk/cliproxy/auth/
go test ./internal/... ./sdk/...          # full suite; new fork code has its own tests
(cd plugins/key-manage-center && make check)
bash plugins/deploy.sh                    # refuses to run unless the tree is clean
```
