# Official ACP runtime: observed behavior

Probed against agy-acp-server 1.4.5 (`agy_acp_server.par --uid=`), personal
OAuth, Google AI Pro tier. These are observations, not a spec; re-check them
when the runtime version changes.

## State and sign-in

- State lives in `~/.gemini/antigravity-acp/`: `settings.json`
  (`auth.type`), `acp_token.json` (mode 0600, credentials, never read or log
  it), `conversations/<sessionId>.db`, `brain/<sessionId>/` (artifacts dir).
  Mounting this directory as a volume keeps sign-in across container restarts.
- `authenticate {methodId: "oauth-personal"}` prints a Google authorize URL on
  stderr and listens on a random `127.0.0.1` port for the redirect. Once
  signed in, later processes need no `authenticate` call.
- One non-JSON line (37 bytes) appears on stdout after `initialize`; the
  JSON-RPC reader must skip non-JSON lines rather than fail.

## Capabilities (`initialize`)

`loadSession`, prompt image/audio/embeddedContext, MCP `http` and `sse`,
session `list`/`resume`/`additionalDirectories`, `auth.logout`,
`_meta.antigravity.systemPrompt.append`, `_meta.antigravity.tier`.

## Models (`session/new`)

- `configOptions`: `model` (only `gemini-3.8-flash` on this account),
  `thinking_level` (`low|medium|high`, default `high`), `mode`
  (`default|auto_edit|yolo`).
- `models.availableModels`: `gemini-3.8-flash-{high,medium,low}`. These are
  what `/v1/models` should expose.

## Native MCP tool calls

- With `mcpServers: [{type: "http", name: "gw", url, headers}]` the runtime
  sends `server/discover`, `initialize` (clientInfo `antigravity-client`,
  protocol `2025-11-25`), `notifications/initialized`, then `tools/list`.
- The model sees tool title `gw_get_weather`. ACP `tool_call` updates carry
  `_meta.mcp.{server,tool}`, `_meta.is_mcp_tool_call`, and `rawInput.arguments`.
- In the default mode, the first call raises `session/request_permission`
  with options `allow_always|allow|deny`. The gateway should allow only
  `is_mcp_tool_call` on its own server name and reject everything else.
- `tools/call` `_meta` carries `antigravity.google/conversation_id` (equal to
  the ACP sessionId) and `progressToken` `<sessionId>:<n>`. It does not
  carry the ACP `toolCallId`, so correlation is by session plus FIFO order of
  `tool_call` updates vs. arriving `tools/call` requests.
- **Parallel calls run sequentially**: when asked for two calls in parallel,
  the runtime issued `tools/call` for Paris, waited for the result, then
  issued Tokyo. The gateway can therefore surface at most the calls that are
  actually in flight; "parallel" OpenAI tool_calls usually arrive as
  consecutive rounds.
- `usage` from `session/prompt`: `inputTokens, outputTokens, thoughtTokens,
  cachedReadTokens, cachedWriteTokens, totalTokens`. A trivial turn costs
  about 45k input tokens because of the runtime's built-in system prompt.

## Distribution and startup

- The release archive holds two files that must stay side by side:
  `agy_acp_server.par` (about 826 MB, dynamically linked against glibc) and
  `localharness_external`. Without the second the runtime fails with
  "Could not find default localharness binary" unless
  `ANTIGRAVITY_HARNESS_PATH` points at it. The container image therefore
  mounts the whole directory at `/opt/agy`.
- The first start in a fresh container is slow (tens of seconds) while the
  `.par` unpacks; `/readyz` stays 503 until `initialize` and model discovery
  finish, so health checks should allow for that delay.

## Context window

- Default limits: 256K tokens per conversation and a 64K-token cut on one
  user message. Both are enforced in `localharness_external`, a static Go
  binary built with go1.28. It contains `utils.GetCheckpointConfig`,
  `config.applyFeatureProviderCheckpointOverrideConfig`,
  `chatconverters.(*TrajectoryChatConverter).getMaxTokensForStep` and
  `serializers.DefaultSerializer.stepToMessage`.
- The `agy` CLI's TUI functions (`offersContextWindow`, `Store.ContextWindow`)
  are not in it, and ACP `configOptions` offer no context-window option.
- The runtime honors `ANTIGRAVITY_HARNESS_PATH`. Verified: the harness process
  ran from that path.
- agy-context-unlock (`agy-patch.py`) recognizes this harness as a
  "standalone language_server" and applies gates 3A, 3B, 4A and 4B. Harness
  sha256 `6c6d28b61a70…` becomes `6ed860c0b1ea…`.
- That patch does not lift the 256K limit. At request time
  `localagent.makeCascadeConfigWithSkillsRunnerAndFS` reads the AgentConfig's
  `compaction_config`; when it is unset (the ACP runtime never sets it, and
  only ever sends `token_threshold`) it defaults to `token_threshold` 140000
  and `checkpoint_config.max_token_limit` 256000. The model's own 1M limit is
  then capped at 256000, and larger requests fail with "exceeding
  cascade_config.checkpoint_config.max_token_limit (256000)". The
  per-model limits in the embedded model_profiles flags do not affect it.
- `scripts/agy-limit-patch.py` rewrites that immediate to 983040: on x86_64
  `mov esi,0x3e800` (file offset 0x5a55764), on arm64
  `mov x4,#0xe800; movk x4,#0x3,lsl#16` (file offset 0x5628fe0). With both
  patches, single prompts of 378,566 and 824,346 tokens were answered; the
  first recalled both codes. The limit change alone is not enough: the
  input is then truncated before it reaches the model.
- Without it, a 135K-token user message is cut. The model then loses the
  question and may spend many internal steps: one such turn reported 818K
  prompt tokens.

## Built-in system prompt

- Each generation request is recorded in
  `conversations/<sessionId>.db`, in the `gen_metadata` table. It is a
  protobuf whose field 1.1 is the assembled system prompt and whose field
  1.16 lists the sections the prompt is built from.
- Sections, in order: `identity` ("You are Antigravity, a powerful agentic
  AI assistant designed by the Google DeepMind team"), `user_information`
  (OS, workspace = the session's working directory, app data and artifact
  paths), `skills` (`run-workflow`), `subagents` (`research`, `self`),
  `messaging`, `conversation_transcript`, `artifacts`, `guidelines` (with
  `reporting-integrity`), and `communication_style`.
- For a plain "hi" with no client tools, the prompt is 17,876 characters,
  and the runtime still declares 15 built-in tools:
  `ask_question`, `define_subagent`, `generate_image`, `invoke_subagent`,
  `manage_subagents`, `manage_task`, `read_url_content`,
  `replace_file_content`, `run_command`, `schedule`, `search_web`,
  `send_message`, `view_file`, `write_to_file`, `run_workflow`.
  Together these account for most of the roughly 13.6K prompt tokens of a
  trivial turn.
- Every user turn is wrapped in `<USER_REQUEST>…</USER_REQUEST>`, followed by
  `<ADDITIONAL_METADATA>` that holds the local time.
- The gateway passes client `system` and `developer` messages through
  `_meta.antigravity.systemPrompt.append`, which adds them to this prompt
  rather than replacing it.

## Quota

- An exhausted quota does not fail `session/prompt`. The runtime sends one
  `agent_message_chunk` with `Usage Limit Reached\n\nYou have reached your
  current quota for this period.`, optionally followed by `Your limit will
  reset in 1 hour, 19 minutes.` (two largest units, rounded down) or `Your
  limit will reset on Nov 9, 2026 10:32 UTC.`, and ends the turn with
  `end_turn`. Other model errors arrive the same way, as their message text.
- A prompt that is exactly `/usage` is answered by the runtime without a
  model call: a Markdown report with a `### Quota Limits` section listing
  groups (`Gemini Models`, `Claude and GPT models`) and buckets such as
  `Five Hour Limit Remaining: 0% remaining (resets Nov 09, 2026, 10:32 UTC)`.
  It took about 3 s on a fresh session.
- Observed on 2026-11-09: the Gemini five-hour bucket at 0% while the weekly
  bucket had 56% left; the delay in the chat message matched the bucket's
  reset time.
