# agy-gateway

[![ci](https://github.com/sujoshua/agy-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/sujoshua/agy-gateway/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/sujoshua/agy-gateway)](https://github.com/sujoshua/agy-gateway/releases)
[![image](https://img.shields.io/badge/ghcr.io-agy--gateway-blue?logo=docker)](https://github.com/sujoshua/agy-gateway/pkgs/container/agy-gateway)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

English | [简体中文](README.zh-CN.md)

An OpenAI-compatible HTTP API (`/v1/chat/completions`, `/v1/models`) in front
of the official Antigravity ACP runtime (`agy_acp_server.par`). Any client
that speaks OpenAI Chat Completions can use the models of a signed-in
Antigravity account, including function calling and streaming.

> [!IMPORTANT]
> This is an independent project. It is not affiliated with, endorsed by, or
> supported by Google. The Antigravity runtime is proprietary Google software;
> it is not part of this repository and is not covered by its license. You
> need your own Antigravity account, and your use of the runtime is governed
> by the [Antigravity terms](https://antigravity.google/terms).

## Contents

- [Features](#features)
- [How it works](#how-it-works)
- [Getting started](#getting-started)
- [Docker](#docker)
- [Running in production](#running-in-production)
- [Configuration](#configuration)
- [API compatibility](#api-compatibility)
- [Using it with OpenAI clients](#using-it-with-openai-clients)
- [Optional: larger context window](#optional-larger-context-window)
- [Limitations](#limitations)
- [Development](#development)
- [License](#license)

## Features

- **Official runtime, standard protocol.** The gateway talks to the runtime
  only through standard ACP over stdio
  ([coder/acp-go-sdk](https://github.com/coder/acp-go-sdk)). It does not
  modify the runtime, imitate another client, or touch OAuth tokens.
- **Native tool calls.** Client tools become native MCP tools that the
  runtime calls through a loopback MCP server. The gateway does not describe
  tools in the prompt or parse tool calls out of model text.
- **Stateless HTTP for clients.** The gateway finds the runtime session from
  the request itself, so clients need no custom headers, cookies, or session
  IDs.
- **Streaming and usage.** SSE streaming, `reasoning_content`, and real token
  usage reported by the runtime.
- **Recovery.** The runtime is restarted with backoff after a crash, and idle
  sessions resume.
- **One account per instance.** One runtime process, and so one Google
  account, per instance. For two accounts, run two instances.

Compatibility is claimed only for what the test suites exercise. See
[API compatibility](#api-compatibility) and [Limitations](#limitations).

## How it works

```
client ──HTTP──▶ server ──▶ session.Manager ──ACP (stdio)──▶ agy_acp_server.par
  ▲  (OpenAI JSON / SSE)        │   ▲                                   │
  │                             ▼   │ tool results                      │ tools/call
  └──── tool_calls ◀──── bridge (MCP streamable HTTP, 127.0.0.1) ◀──────┘
```

| Package | Role |
|---|---|
| `internal/acp` | Supervises the runtime process: start, `initialize`, model discovery, restart with backoff after a crash, `session/new`/`resume`/`prompt`/`cancel`/`close`. |
| `internal/bridge` | Loopback MCP server, one tenant per runtime session with its own secret. A `tools/call` blocks until the HTTP client posts the result, or until the tool timeout or session close resolves it as an error. |
| `internal/session` | Maps conversations to runtime sessions and handles ownership, TTL, locks, eviction, tool-call correlation, resume after a crash, and transcript replay. |
| `internal/harness` | Optional patched copy of the runtime's harness; see [Optional: larger context window](#optional-larger-context-window). |
| `internal/openai` | Request validation, response and SSE chunk types, and errors. |
| `internal/server` | HTTP routes, auth, and the mapping from errors to status codes. |

### How a conversation finds its session

The gateway keeps a fingerprint of each session: the owner, the model, a hash
of the tool definitions, and the messages seen so far. A new request matches
a session when its messages begin with that conversation and add messages at
the end. Only the new tail goes to the runtime, so history is never sent
twice.

- **Owner.** The owner is a hash of the API key, plus the OpenAI `user` field
  when present. One owner can never match another owner's session.
- **No match.** If no session matches, for example after a crash, an
  eviction, or an edited history, a new runtime session starts and the
  earlier transcript is replayed once (see below).

**Transcript replay.** A new runtime session knows nothing of the
conversation, so the gateway renders the earlier messages as plain text
(`[user]: …`, `[assistant called tool …]`, `[result of tool …]: …`) and sends
them, inside `<conversation>…</conversation>`, ahead of the new input in the
first prompt. It keeps the newest messages within `AGY_GW_REPLAY_BUDGET`
bytes and replaces older ones with `[N earlier messages omitted]`. This is not
an exact reproduction: images become `[image]` placeholders, and past tool
calls become text. New tool calls after the replay still go through the
native MCP bridge.

### Tool calls

1. The runtime calls `client_<tool>` on the bridge.
2. The gateway returns `finish_reason: "tool_calls"` with a stable
   `call_…` id. When streaming, arguments arrive as incremental deltas.
3. The call stays pending in the runtime after the HTTP response ends.
4. The next request carries `role: "tool"` messages. The gateway matches them
   to pending calls by `tool_call_id`, resolves them, and the same runtime
   turn continues. When it finishes, the client gets the final answer.

Edge cases:

- **Missing result.** A pending call that gets no result in the next request
  is answered with an error result, and the model sees that error.
- **Duplicate result.** A result for a call that was already answered
  returns 400 instead of reaching the model a second time.
- **Tool timeout.** A call that waits longer than `AGY_GW_TOOL_RESULT_TIMEOUT`
  resolves as a timeout error. If its result arrives later, it is reported to
  the model as text instead of being lost.
- **New message instead of results.** If the next request carries a new user
  message where tool results were expected, the pending turn is cancelled and
  the new message starts a fresh turn.
- **Client disconnect.** If the client disconnects mid-turn, the gateway sends
  `session/cancel` and frees the session.

Why a loopback MCP server and not ACP itself: ACP `tool_call` updates only
report tools the agent runs on its own; a client cannot declare a tool or
return a result through them. The stable way for an ACP client to add tools
is `mcpServers` in `session/new`.

> [!NOTE]
> **MCP-over-ACP.** acp-go-sdk v0.13.5 has MCP-over-ACP methods
> (`mcp/connect`, `mcp/message`, `mcp/disconnect`, with `McpServerAcp`) that
> carry MCP messages over the ACP connection itself, which would remove the
> loopback HTTP server.
>
> - The SDK marks them **UNSTABLE**: they are not in the spec yet and may
>   change or be removed at any time.
> - agy runtime 1.4.5 advertises only `http` and `sse` under
>   `mcpCapabilities` in its `initialize` response, not `acp`.
>
> The gateway therefore uses the HTTP bridge. If a later runtime advertises
> `mcpCapabilities.acp`, the bridge transport could switch without changing
> the OpenAI-facing API.

## Getting started

### Prerequisites

- The Antigravity ACP runtime for your platform, unpacked so that
  `agy_acp_server.par` and `localharness_external` sit side by side. Google
  publishes it as a zip, for example
  `https://dl.google.com/agy-extensions/releases/linux/agy-acp-server-1.4.5-linux-x86_64.zip`.
  See [docs/runtime-notes.md](docs/runtime-notes.md) for what the gateway
  relies on.
- An Antigravity account.

### Install

Pick one:

- **Prebuilt binary.** Download the archive for your platform (Linux or
  macOS, amd64 or arm64) from
  [Releases](https://github.com/sujoshua/agy-gateway/releases) and verify it
  against `checksums.txt`. Windows is not supported.
- **Container image.** `docker pull ghcr.io/sujoshua/agy-gateway:latest`;
  see [Docker](#docker).
- **From source.** With Go 1.27+:

  ```bash
  make build
  ```

  The binary is written to `bin/agy-gateway`. The examples below use this
  path.

### Sign in

Sign the runtime in once. This is interactive: open the printed URL, sign in,
then paste the `http://127.0.0.1:…` address the browser was redirected to.
If the runtime is already signed in, this does nothing.

```bash
AGY_GW_RUNTIME_CMD=/opt/agy/agy_acp_server.par ./bin/agy-gateway login
```

### Run

```bash
AGY_GW_RUNTIME_CMD=/opt/agy/agy_acp_server.par AGY_GW_API_KEYS=change-me ./bin/agy-gateway serve
```

```bash
curl -s localhost:8080/v1/chat/completions -H 'Authorization: Bearer change-me' -H 'Content-Type: application/json' -d '{"model":"gemini-3.8-flash-low","messages":[{"role":"user","content":"hi"}]}'
```

The runtime keeps its state (sign-in and conversations) in
`$AGY_GW_RUNTIME_HOME/.gemini/antigravity-acp`. `AGY_GW_RUNTIME_HOME`
defaults to `$HOME`.

## Docker

Images for `linux/amd64` and `linux/arm64` are published to GitHub Container
Registry:

| Tag | Source |
|---|---|
| `latest`, `X.Y.Z`, `X.Y` | Release tags. |
| `edge` | The latest commit on `main`. |

The runtime is not redistributed here, so the image does not include it.
Mount the runtime directory read-only at `/opt/agy`, and use a volume at
`/data` for the runtime's state.

```bash
docker run -it --rm -v /path/to/agy-runtime:/opt/agy:ro -v agy-data:/data ghcr.io/sujoshua/agy-gateway:latest login
```

```bash
docker run -d --name agy-gw -p 8080:8080 -v /path/to/agy-runtime:/opt/agy:ro -v agy-data:/data -e AGY_GW_API_KEYS=change-me -e AGY_GW_IGNORE_UNSUPPORTED_PARAMS=true ghcr.io/sujoshua/agy-gateway:latest
```

To build the image yourself:

```bash
docker build -t agy-gateway .
```

To bake the runtime into a private image instead, copy both files into
`runtime/` and build with `docker build --target bundled`. Do not push such
an image to a public registry.

Image details:

- The image is based on `debian:bookworm-slim`, because the runtime needs
  glibc and CA certificates.
- It runs as uid 10001, under `tini`. Arbitrary uids work too.
- The first start in a new container takes tens of seconds while the `.par`
  unpacks.

## Running in production

- **Health checks.** `GET /healthz` answers 200 as long as the process
  serves. `GET /readyz` answers 200 only when the runtime is initialized and
  has models, and otherwise 503 with a status string. Allow for the slow
  first start.
- **One instance per account.** Sessions live in the process, so run a single
  instance per account, with its own persistent `/data`. Do not run two
  instances on the same `/data`.
- **Shutdown.** On SIGTERM the gateway stops accepting requests, waits up to
  20 s for open requests, then closes sessions and stops the runtime. Give
  the process at least 40 s to stop before it is killed.
- **Network.** Set `AGY_GW_API_KEYS`. An empty value accepts any caller and
  is only sensible on a private network.

## Configuration

Every setting is an environment variable. [.env.example](.env.example) lists
them all with their defaults.

| Variable | Default | Meaning |
|---|---|---|
| `AGY_GW_LISTEN` | `:8080` | API address. |
| `AGY_GW_API_KEYS` | empty | Comma-separated bearer keys. Empty accepts any caller; use that only on a private network. |
| `AGY_GW_RUNTIME_CMD` | `agy_acp_server.par` | Runtime binary. |
| `AGY_GW_RUNTIME_ARGS` | `--uid=` | Runtime arguments. |
| `AGY_GW_RUNTIME_HOME` | `$HOME` | HOME for the runtime, which holds its sign-in state. |
| `AGY_GW_WORKDIR` | `/tmp/agy-gateway` | Parent of the empty per-session working directories. |
| `AGY_GW_BRIDGE_LISTEN` | `127.0.0.1:0` | Loopback MCP bridge. |
| `AGY_GW_MCP_SERVER_NAME` | `client` | The model sees tools as `<name>_<tool>`. |
| `AGY_GW_SESSION_TTL` | `30m` | Idle time before a session is swept. |
| `AGY_GW_MAX_SESSIONS` | `64` | Maximum number of sessions. When full, the least recently used idle session is evicted. If none is idle, the request gets 429. |
| `AGY_GW_TOOL_RESULT_TIMEOUT` | `170s` | How long a tool call waits for its result. Keep it below the runtime's 180 s MCP limit. |
| `AGY_GW_REQUEST_TIMEOUT` | `15m` | Upper bound for one request. |
| `AGY_GW_TOOL_BATCH_WINDOW` | `250ms` | How long to wait after the first tool call for more calls before answering. |
| `AGY_GW_REPLAY_BUDGET` | `163840` | Bytes of transcript replayed into a replacement session. |
| `AGY_GW_HARNESS_PATH` | empty | Use this `localharness_external` instead of the one next to the runtime. |
| `AGY_GW_HARNESS_PATCH_CMD` | empty (off) | Patch a copy of the harness before starting; see [Optional: larger context window](#optional-larger-context-window). |
| `AGY_GW_HARNESS_CACHE_DIR` | `$AGY_GW_RUNTIME_HOME/.cache/agy-gateway/harness` | Where patched copies are kept. |
| `AGY_GW_SCHEMA_BUDGET` | `1350` | Tool schemas larger than this lose nested description keys. |
| `AGY_GW_IGNORE_UNSUPPORTED_PARAMS` | `false` | Accept and ignore sampling parameters instead of returning 400. |
| `AGY_GW_EMIT_REASONING` | `true` | Stream runtime thoughts as `reasoning_content`. |
| `AGY_GW_LOG_LEVEL` | `info` | Log level. Logs never contain credentials or prompt text. |

## API compatibility

| Feature | Status |
|---|---|
| `GET /v1/models` | The runtime's models, for example `gemini-3.8-flash` and `-low`/`-medium`/`-high` thinking variants. |
| `POST /v1/chat/completions` | Supported, non-streaming and SSE, with `[DONE]`. |
| Roles | `system`, `developer`, `user`, `assistant` (including `tool_calls`), and `tool`. |
| Content | Text parts, plus images as `data:` base64 URLs. |
| `tools` | Type `function`, mapped to native MCP tools. |
| `tool_choice` | `auto` and `none` only. |
| `parallel_tool_calls` | `false` answers with one call per response; the next call follows in the next round. |
| `stream_options.include_usage` | Supported. The usage chunk is the last chunk and has empty `choices`. |
| `usage` | Real runtime counts, including cached and reasoning tokens. If the runtime reports none, the field is omitted rather than invented. |
| `reasoning_effort` | `low`/`medium`/`high` selects the matching thinking variant. |
| `finish_reason` | `stop`, `tool_calls`, `length`, and `content_filter`. |
| `user` | Further isolates sessions within one API key. |
| `/v1/responses` | Not implemented. Returns 404 with an explanation. |

System and developer messages are appended to the runtime's own built-in
system prompt; they do not replace it.

Requests that use any of the following are rejected with 400 and a message
naming the field:

- `n` > 1.
- `tool_choice` set to `required` or to a specific function.
- Remote image URLs.
- Audio.
- Assistant prefill.
- The legacy `functions` field.
- `logprobs`.
- `response_format` other than text.
- Sampling parameters: `temperature`, `top_p`, penalties, `seed`, `stop`,
  `max_tokens`, `max_completion_tokens`. Set
  `AGY_GW_IGNORE_UNSUPPORTED_PARAMS=true` to accept and ignore these instead.

### Errors

Errors use the OpenAI shape `{"error":{"message","type","code"}}`.

| Status | When |
|---|---|
| 400 | Invalid or unsupported request; duplicate tool result. |
| 401 | Missing or wrong API key. |
| 404 | Unknown model or route. |
| 409 | The conversation is busy with another request. |
| 429 | Runtime quota exhausted, or every session slot is busy. |
| 502 | The runtime returned an error. |
| 503 | The runtime is not ready, for example while starting, restarting, or signed out. |
| 504 | `AGY_GW_REQUEST_TIMEOUT` expired. |

Once a stream has started, a failure is sent as a final `data: {"error":…}`
event, without `[DONE]`, so clients see that the stream was truncated.

## Using it with OpenAI clients

Point any OpenAI Chat Completions client at the gateway:

- Base URL: `http://<gateway>:8080/v1` (some clients add `/v1` themselves).
- API key: one of `AGY_GW_API_KEYS`.
- Model: an ID from `/v1/models`.

Notes:

- Set `AGY_GW_IGNORE_UNSUPPORTED_PARAMS=true` for clients that always send
  `temperature` or `max_tokens`, which most coding clients do.
- When `stream_options.include_usage` is set, the usage chunk always comes
  before `[DONE]`.
- 429 and 5xx are returned before any bytes are written whenever possible,
  so proxies and load balancers in front of several instances can fail over.
- If a client is moved to another instance in the middle of a tool loop, that
  instance replays the transcript. The pending call on the first instance is
  cleaned up when its session expires.

The integration tests use the official
[openai-go](https://github.com/openai/openai-go) SDK.

## Optional: larger context window

The official runtime limits a conversation to 256K tokens and cuts a single
user message at 64K. Third-party patches such as
[`agy-context-unlock`](https://github.com/lili-luo/dsh-plugin-bundle/tree/main/agy-context-unlock)
raise this to about 1M by rewriting machine code in Antigravity binaries.
The gateway can apply such a patch, but **it is off by default**. Running
the runtime on a patched harness is your own decision and your own risk.

How it works when `AGY_GW_HARNESS_PATCH_CMD` is set:

1. The gateway copies the official `localharness_external` (or
   `AGY_GW_HARNESS_PATH`) into `AGY_GW_HARNESS_CACHE_DIR`. The original file
   is never written, so a read-only mount is fine.
2. It runs the command with the copy's path appended as the last argument.
   The command is split on spaces, without shell quoting.
3. It starts the runtime with `ANTIGRAVITY_HARNESS_PATH` pointing at the
   patched copy.

The copy is cached under a hash of the original file and the command, so a
restart reuses it. A runtime upgrade changes the hash and patches again.

If the command fails, or leaves the file unchanged, the gateway logs an
error and falls back to the official harness, so it keeps serving at 256K.
`/readyz` reports which harness is in use in its `harness` field: `official`,
`custom`, `patched`, or `patch failed`.

```bash
AGY_GW_HARNESS_PATCH_CMD="python3 /opt/agy-patch/agy-patch.py --in-place --no-backup --binary" ./bin/agy-gateway serve
```

The image does not include Python. In containers, either patch a copy on the
host and mount it with `AGY_GW_HARNESS_PATH`, or build your own image that
adds `python3` and the script. With a larger window, raise
`AGY_GW_REPLAY_BUDGET` too, so a replayed transcript can use the space.

Verified with runtime 1.4.5 and agy-context-unlock as of 2026-11-08, using
model `gemini-3.8-flash-low`. The gateway applied gates 3A, 3B, 4A and 4B
(the CLI-only gates 1, 2 and the bonus gate do not exist in the harness).
Each request was one user message with a code at its very start and another
at its very end:

| Message size | Official harness | Patched harness |
|---|---|---|
| ≈135K tokens | The message was cut at 64K. The model never saw the question, wandered through several internal steps, and used 818,048 prompt tokens. | Both codes correct, 148,918 prompt tokens. |
| ≈310K tokens | not run | Both codes correct, 324,543 prompt tokens. That is past the official 256K limit. |

A patch that targets a future runtime version may fail. In that case the
gateway falls back to the official harness and `/readyz` reports
`patch failed`.

## Limitations

- **Parallel calls.** The runtime runs "parallel" tool calls one at a time, so
  they usually reach the client as consecutive rounds, each with one call.
- **Usage timing.** The runtime reports usage only at the end of a turn. A
  response that stops at `tool_calls` therefore carries no usage; that turn's
  usage arrives with its final response. Its prompt tokens include the
  runtime's built-in system prompt, roughly 13–45k tokens.
- **Sampling.** Sampling parameters and forced tool choice cannot be passed to
  the runtime.
- **Edited history.** History that the client edited or truncated cannot be
  applied to an existing session. It starts a new session with a transcript
  replay, which is not an exact reproduction.
- **Crash recovery.** After a runtime crash, idle sessions resume through ACP
  `session/resume`. A turn that was in flight is lost; the client gets an
  error, and its retry is replayed.
- **Session close.** `session/close` is best effort. Runtime conversation
  files under `/data` are not pruned by the gateway.
- **Sweep interval.** Sessions are swept once a minute.

## Development

| Command | What it runs |
|---|---|
| `make bootstrap` | Installs the pinned golangci-lint into `bin/`. |
| `make check` | Format check, golangci-lint, `go vet` (also with the `integration` tag), unit and end-to-end tests, and the same tests with `-race`. |
| `AGY_GW_RUNTIME_CMD=/path/to/agy_acp_server.par make test-integration` | Real runtime tests through the official `openai-go` SDK: models, the `get_weather` tool loop with a follow-up turn, streaming tools for two cities, and a streamed "hi". |

`make check` does not need the runtime. The end-to-end tests in
`internal/app` run the whole gateway against a mock ACP agent
(`internal/acptest`), whose tools go through the real MCP bridge.

The integration tests need a runtime that is already signed in. They never
sign in or out, and CI does not run them, so CI needs no credentials.

### CI and releases

| Workflow | Runs on | What it does |
|---|---|---|
| [ci](.github/workflows/ci.yml) | Pushes to `main`, pull requests | golangci-lint, `go vet`, race tests on Linux and macOS, govulncheck, and a GoReleaser snapshot build. |
| [docker](.github/workflows/docker.yml) | Pushes to `main`, `v*` tags, pull requests | Builds the multi-arch image. Pushes `edge` from `main` and version tags from releases, with provenance, SBOM, and an attestation. |
| [release](.github/workflows/release.yml) | `v*` tags | GoReleaser builds the archives and checksums, writes the changelog, publishes the GitHub release, and attests the build. |
| [codeql](.github/workflows/codeql.yml) | Pushes, pull requests, weekly | CodeQL analysis of the Go code and the workflows. |

Dependabot keeps Go modules, actions, and base images up to date.

To release, tag a commit on `main` with a semantic version and push the tag:

```bash
git tag -a v0.1.0 -m v0.1.0
```

```bash
git push origin v0.1.0
```

Contributions are welcome. Please run `make check` before opening a pull
request, and never include tokens, sign-in state, or runtime files in an
issue or commit.

## License

agy-gateway is released under the [MIT License](LICENSE).

The license covers this repository only. The Antigravity runtime
(`agy_acp_server.par`, `localharness_external`) and the `agy` CLI are
proprietary software owned by Google LLC and are subject to Google's own
terms.
