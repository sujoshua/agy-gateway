# agy-gateway

[![ci](https://github.com/sujoshua/agy-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/sujoshua/agy-gateway/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/sujoshua/agy-gateway)](https://github.com/sujoshua/agy-gateway/releases)
[![image](https://img.shields.io/badge/ghcr.io-agy--gateway-blue?logo=docker)](https://github.com/sujoshua/agy-gateway/pkgs/container/agy-gateway)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[English](README.md) | 简体中文

在官方 Antigravity ACP runtime（`agy_acp_server.par`）前面提供一层 OpenAI 兼容的 HTTP API（`/v1/chat/completions`、`/v1/models`）。任何支持 OpenAI Chat Completions 的客户端，都可以通过它使用已登录 Antigravity 账号的模型，包括函数调用和流式输出。

> [!IMPORTANT]
> 这是一个独立项目，与 Google 没有任何关联，也未获得 Google 的认可或支持。Antigravity runtime 是 Google 的专有软件，不包含在本仓库中，也不受本仓库许可证约束。你需要自己的 Antigravity 账号，使用 runtime 须遵守 [Antigravity 服务条款](https://antigravity.google/terms)。

## 目录

- [特性](#特性)
- [工作原理](#工作原理)
- [快速开始](#快速开始)
- [Docker](#docker)
- [生产环境运行](#生产环境运行)
- [配置](#配置)
- [API 兼容性](#api-兼容性)
- [在 OpenAI 客户端中使用](#在-openai-客户端中使用)
- [可选：更大的上下文窗口](#可选更大的上下文窗口)
- [限制](#限制)
- [开发](#开发)
- [许可证](#许可证)

## 特性

- **官方 runtime，标准协议。** 网关只通过 stdio 上的标准 ACP（[coder/acp-go-sdk](https://github.com/coder/acp-go-sdk)）与 runtime 通信。不修改 runtime，不伪装成其他客户端，也不接触 OAuth token。
- **原生工具调用。** 客户端定义的工具会变成原生 MCP 工具，由 runtime 通过本机回环地址上的 MCP server 调用。网关不在 prompt 里描述工具，也不从模型输出的文本中解析工具调用。
- **对客户端无状态。** 网关根据请求本身找到对应的 runtime 会话，客户端不需要自定义 header、cookie 或会话 ID。
- **流式输出与用量。** 支持 SSE 流式输出、`reasoning_content`，以及 runtime 报告的真实 token 用量。
- **故障恢复。** runtime 崩溃后会退避重启，空闲会话会被恢复。
- **一个实例一个账号。** 每个实例运行一个 runtime 进程，对应一个 Google 账号。两个账号就运行两个实例。

兼容性只以测试覆盖到的部分为准，见 [API 兼容性](#api-兼容性) 和 [限制](#限制)。

## 工作原理

```
client ──HTTP──▶ server ──▶ session.Manager ──ACP (stdio)──▶ agy_acp_server.par
  ▲  (OpenAI JSON / SSE)        │   ▲                                   │
  │                             ▼   │ tool results                      │ tools/call
  └──── tool_calls ◀──── bridge (MCP streamable HTTP, 127.0.0.1) ◀──────┘
```

| 包 | 职责 |
|---|---|
| `internal/acp` | 管理 runtime 进程：启动、`initialize`、模型发现、崩溃后退避重启，以及 `session/new`/`resume`/`prompt`/`cancel`/`close`。 |
| `internal/bridge` | 回环 MCP server，每个 runtime 会话一个租户，各有独立密钥。一次 `tools/call` 会一直阻塞，直到 HTTP 客户端提交结果，或因工具超时、会话关闭而以错误结束。 |
| `internal/session` | 把对话映射到 runtime 会话，负责归属、TTL、锁、淘汰、工具调用关联、崩溃后恢复和历史重放。 |
| `internal/harness` | 可选的 runtime harness 补丁副本，见 [可选：更大的上下文窗口](#可选更大的上下文窗口)。 |
| `internal/openai` | 请求校验、响应和 SSE chunk 类型、错误。 |
| `internal/server` | HTTP 路由、鉴权，以及错误到状态码的映射。 |

### 对话如何找到会话

网关为每个会话保存一份指纹：归属方、模型、工具定义的哈希，以及已经见过的消息。如果新请求的消息以这段对话开头、只在末尾追加了新消息，就匹配到该会话。只有新增的部分会发给 runtime，历史不会重复发送。

- **归属方。** 归属方是 API key 的哈希，如果请求带了 OpenAI `user` 字段，也一并计入。一个归属方永远匹配不到另一个归属方的会话。
- **没有匹配。** 如果没有匹配的会话，例如 runtime 崩溃、会话被淘汰或客户端修改了历史，就新建一个 runtime 会话，并把之前的历史重放一次（见下文）。

**历史重放。** 新的 runtime 会话对之前的对话一无所知，所以网关把之前的消息转成纯文本（`[user]: …`、`[assistant called tool …]`、`[result of tool …]: …`），包在 `<conversation>…</conversation>` 里，放在本次新输入前面，作为第一条 prompt 发出。它从最新的消息往前保留，总量不超过 `AGY_GW_REPLAY_BUDGET` 字节，更早的消息用 `[N earlier messages omitted]` 代替。这不是原样还原：图片变成 `[image]` 占位，历史中的工具调用变成文字描述。重放之后的新工具调用仍然走原生 MCP 桥。

### 工具调用

1. runtime 在桥上调用 `client_<tool>`。
2. 网关返回 `finish_reason: "tool_calls"`，附带稳定的 `call_…` id。流式输出时，参数以增量 delta 的形式下发。
3. HTTP 响应结束后，这次调用在 runtime 中保持挂起。
4. 下一个请求带上 `role: "tool"` 消息。网关按 `tool_call_id` 把它们对应到挂起的调用并交回结果，同一轮 runtime 对话继续执行。结束后客户端拿到最终回答。

边界情况：

- **缺少结果。** 挂起的调用如果在下一个请求中没有收到结果，会以错误结果作答，模型会看到这个错误。
- **重复结果。** 已经回答过的调用再次提交结果，返回 400，不会第二次交给模型。
- **工具超时。** 等待超过 `AGY_GW_TOOL_RESULT_TIMEOUT` 的调用以超时错误结束。如果结果之后才到，会以文本形式告诉模型，不会丢失。
- **用新消息代替结果。** 如果下一个请求在应该提交工具结果的位置带了新的用户消息，挂起的那一轮会被取消，新消息开启新的一轮。
- **客户端断开。** 客户端在一轮进行中断开时，网关发送 `session/cancel` 并释放会话。

为什么用回环 MCP server 而不是 ACP 本身：ACP 的 `tool_call` 更新只用来上报 agent 自己执行的工具，客户端无法通过它声明工具，也无法交回结果。ACP 客户端添加工具的稳定方式是在 `session/new` 里传 `mcpServers`。

> [!NOTE]
> **MCP-over-ACP。** acp-go-sdk v0.13.5 里有一组 MCP-over-ACP 接口：`mcp/connect`、`mcp/message`、`mcp/disconnect`，配合 `McpServerAcp`，让 MCP 消息直接走 ACP 连接，可以省掉回环 HTTP server。
>
> - SDK 文档把它标成 **UNSTABLE**，说明它还不在规范里，随时可能变或被删。
> - agy runtime 1.4.5 在 `initialize` 时只声明了 `mcpCapabilities` 的 `http` 和 `sse`，没有声明 `acp`。
>
> 所以网关目前使用 HTTP 桥。等以后的 runtime 声明支持 `mcpCapabilities.acp`，桥的传输方式可以切换过去，对外的 OpenAI 接口不用变。

## 快速开始

### 前置条件

- 对应平台的 Antigravity ACP runtime，解压后 `agy_acp_server.par` 和 `localharness_external` 要放在同一目录。Google 以 zip 形式发布，例如 `https://dl.google.com/agy-extensions/releases/linux/agy-acp-server-1.4.5-linux-x86_64.zip`。网关依赖的 runtime 行为见 [docs/runtime-notes.md](docs/runtime-notes.md)。
- 一个 Antigravity 账号。

### 安装

任选一种：

- **预编译二进制。** 从 [Releases](https://github.com/sujoshua/agy-gateway/releases) 下载对应平台的压缩包（Linux 或 macOS，amd64 或 arm64），并用 `checksums.txt` 校验。不支持 Windows。
- **容器镜像。** `docker pull ghcr.io/sujoshua/agy-gateway:latest`，见 [Docker](#docker)。
- **从源码构建。** 需要 Go 1.27+：

  ```bash
  make build
  ```

  二进制输出到 `bin/agy-gateway`，下面的示例都用这个路径。

### 登录

runtime 只需登录一次。这一步是交互式的：打开输出的 URL 并登录，然后把浏览器跳转到的 `http://127.0.0.1:…` 地址粘贴回来。如果 runtime 已经登录，这一步什么也不做。

```bash
AGY_GW_RUNTIME_CMD=/opt/agy/agy_acp_server.par ./bin/agy-gateway login
```

### 运行

```bash
AGY_GW_RUNTIME_CMD=/opt/agy/agy_acp_server.par AGY_GW_API_KEYS=change-me ./bin/agy-gateway serve
```

```bash
curl -s localhost:8080/v1/chat/completions -H 'Authorization: Bearer change-me' -H 'Content-Type: application/json' -d '{"model":"gemini-3.8-flash-low","messages":[{"role":"user","content":"hi"}]}'
```

runtime 的状态（登录信息和对话）保存在 `$AGY_GW_RUNTIME_HOME/.gemini/antigravity-acp`。`AGY_GW_RUNTIME_HOME` 默认是 `$HOME`。

## Docker

`linux/amd64` 和 `linux/arm64` 的镜像发布在 GitHub Container Registry：

| 标签 | 来源 |
|---|---|
| `latest`、`X.Y.Z`、`X.Y` | 发版标签。 |
| `edge` | `main` 分支的最新提交。 |

本仓库不分发 runtime，所以镜像里不包含它。把 runtime 目录以只读方式挂载到 `/opt/agy`，并在 `/data` 挂一个卷保存 runtime 状态。

```bash
docker run -it --rm -v /path/to/agy-runtime:/opt/agy:ro -v agy-data:/data ghcr.io/sujoshua/agy-gateway:latest login
```

```bash
docker run -d --name agy-gw -p 8080:8080 -v /path/to/agy-runtime:/opt/agy:ro -v agy-data:/data -e AGY_GW_API_KEYS=change-me -e AGY_GW_IGNORE_UNSUPPORTED_PARAMS=true ghcr.io/sujoshua/agy-gateway:latest
```

自己构建镜像：

```bash
docker build -t agy-gateway .
```

如果想把 runtime 打进私有镜像，把两个文件复制到 `runtime/`，再用 `docker build --target bundled` 构建。不要把这样的镜像推到公开仓库。

镜像说明：

- 基于 `debian:bookworm-slim`，因为 runtime 需要 glibc 和 CA 证书。
- 以 uid 10001 运行在 `tini` 之下，也支持任意 uid。
- 新容器第一次启动需要几十秒，用来解压 `.par`。

## 生产环境运行

- **健康检查。** 只要进程还在服务，`GET /healthz` 就返回 200。`GET /readyz` 只有在 runtime 完成初始化并拿到模型列表后才返回 200，否则返回 503 和状态说明。注意第一次启动较慢。
- **一个账号一个实例。** 会话保存在进程内，所以每个账号只运行一个实例，并配一个独立的持久化 `/data`。不要让两个实例共用同一个 `/data`。
- **关闭。** 收到 SIGTERM 后，网关停止接收新请求，最多等待 20 秒让进行中的请求完成，然后关闭会话并停止 runtime。强制终止前请至少给进程留 40 秒。
- **网络。** 请设置 `AGY_GW_API_KEYS`。留空会接受任何调用方，只适合在私有网络中使用。

## 配置

所有配置都通过环境变量设置。[.env.example](.env.example) 列出了全部变量及其默认值。

| 变量 | 默认值 | 含义 |
|---|---|---|
| `AGY_GW_LISTEN` | `:8080` | API 监听地址。 |
| `AGY_GW_API_KEYS` | 空 | 逗号分隔的 bearer key。留空会接受任何调用方，只适合私有网络。 |
| `AGY_GW_RUNTIME_CMD` | `agy_acp_server.par` | runtime 可执行文件。 |
| `AGY_GW_RUNTIME_ARGS` | `--uid=` | runtime 参数。 |
| `AGY_GW_RUNTIME_HOME` | `$HOME` | runtime 的 HOME，其中保存登录状态。 |
| `AGY_GW_WORKDIR` | `/tmp/agy-gateway` | 每个会话的空工作目录所在的父目录。 |
| `AGY_GW_BRIDGE_LISTEN` | `127.0.0.1:0` | 回环 MCP 桥的地址。 |
| `AGY_GW_MCP_SERVER_NAME` | `client` | 模型看到的工具名为 `<name>_<tool>`。 |
| `AGY_GW_SESSION_TTL` | `30m` | 会话空闲多久后被清理。 |
| `AGY_GW_MAX_SESSIONS` | `64` | 最大会话数。满了之后淘汰最久未使用的空闲会话；没有空闲会话时请求返回 429。 |
| `AGY_GW_TOOL_RESULT_TIMEOUT` | `170s` | 工具调用等待结果的时长。要小于 runtime 的 180 秒 MCP 上限。 |
| `AGY_GW_REQUEST_TIMEOUT` | `15m` | 单个请求的时长上限。 |
| `AGY_GW_TOOL_BATCH_WINDOW` | `250ms` | 收到第一个工具调用后，再等多久收集更多调用才返回。 |
| `AGY_GW_REPLAY_BUDGET` | `163840` | 重放到新会话中的历史字节数上限。 |
| `AGY_GW_HARNESS_PATH` | 空 | 使用指定的 `localharness_external`，而不是 runtime 旁边的那个。 |
| `AGY_GW_HARNESS_PATCH_CMD` | 空（关闭） | 启动前给 harness 副本打补丁，见 [可选：更大的上下文窗口](#可选更大的上下文窗口)。 |
| `AGY_GW_HARNESS_CACHE_DIR` | `$AGY_GW_RUNTIME_HOME/.cache/agy-gateway/harness` | 补丁副本的缓存目录。 |
| `AGY_GW_SCHEMA_BUDGET` | `1350` | 超过这个大小的工具 schema 会去掉嵌套的 description。 |
| `AGY_GW_IGNORE_UNSUPPORTED_PARAMS` | `false` | 接受并忽略采样参数，而不是返回 400。 |
| `AGY_GW_EMIT_REASONING` | `true` | 把 runtime 的思考过程作为 `reasoning_content` 输出。 |
| `AGY_GW_LOG_LEVEL` | `info` | 日志级别。日志中不会出现凭证或 prompt 内容。 |

## API 兼容性

| 功能 | 状态 |
|---|---|
| `GET /v1/models` | runtime 提供的模型，例如 `gemini-3.8-flash` 及其 `-low`/`-medium`/`-high` 思考档位。 |
| `POST /v1/chat/completions` | 支持，包括非流式和带 `[DONE]` 的 SSE。 |
| 角色 | `system`、`developer`、`user`、`assistant`（含 `tool_calls`）和 `tool`。 |
| 内容 | 文本，以及 `data:` base64 URL 形式的图片。 |
| `tools` | `function` 类型，映射为原生 MCP 工具。 |
| `tool_choice` | 只支持 `auto` 和 `none`。 |
| `parallel_tool_calls` | 设为 `false` 时每个响应只返回一个调用，下一个在下一轮返回。 |
| `stream_options.include_usage` | 支持。用量 chunk 是最后一个 chunk，`choices` 为空。 |
| `usage` | runtime 报告的真实数值，包括缓存和推理 token。runtime 没有报告时省略该字段，不会编造。 |
| `reasoning_effort` | `low`/`medium`/`high` 选择对应的思考档位。 |
| `finish_reason` | `stop`、`tool_calls`、`length` 和 `content_filter`。 |
| `user` | 在同一个 API key 内进一步隔离会话。 |
| `/v1/responses` | 未实现，返回 404 并附说明。 |

system 和 developer 消息会追加到 runtime 自带的系统提示词之后，不会替换它。

使用以下任一功能的请求会返回 400，错误信息中会指明字段：

- `n` > 1。
- `tool_choice` 设为 `required` 或指定某个函数。
- 远程图片 URL。
- 音频。
- assistant 预填充。
- 旧版 `functions` 字段。
- `logprobs`。
- 非 text 的 `response_format`。
- 采样参数：`temperature`、`top_p`、各种 penalty、`seed`、`stop`、`max_tokens`、`max_completion_tokens`。设置 `AGY_GW_IGNORE_UNSUPPORTED_PARAMS=true` 可以改为接受并忽略。

### 错误

错误使用 OpenAI 的格式 `{"error":{"message","type","code"}}`。

| 状态码 | 场景 |
|---|---|
| 400 | 请求无效或不支持；重复提交工具结果。 |
| 401 | 缺少 API key 或 key 错误。 |
| 404 | 未知的模型或路由。 |
| 409 | 该对话正在处理另一个请求。 |
| 429 | runtime 配额用尽，或所有会话槽位都在忙。 |
| 502 | runtime 返回了错误。 |
| 503 | runtime 未就绪，例如正在启动、重启或未登录。 |
| 504 | 超过 `AGY_GW_REQUEST_TIMEOUT`。 |

流式输出开始后如果出错，会以最后一个 `data: {"error":…}` 事件发出，并且不发送 `[DONE]`，客户端由此可以判断流被截断了。

## 在 OpenAI 客户端中使用

把任意 OpenAI Chat Completions 客户端指向网关即可：

- Base URL：`http://<gateway>:8080/v1`（有些客户端会自己加 `/v1`）。
- API key：`AGY_GW_API_KEYS` 中的任意一个。
- 模型：`/v1/models` 返回的 ID。

注意事项：

- 很多编程类客户端总是发送 `temperature` 或 `max_tokens`，对这类客户端请设置 `AGY_GW_IGNORE_UNSUPPORTED_PARAMS=true`。
- 设置了 `stream_options.include_usage` 时，用量 chunk 总是在 `[DONE]` 之前。
- 网关会尽量在写出任何字节之前返回 429 和 5xx，方便前面的代理或负载均衡在多个实例之间故障转移。
- 如果客户端在工具调用循环中途被切到另一个实例，那个实例会重放历史。第一个实例上挂起的调用会在会话过期时被清理。

集成测试使用官方的 [openai-go](https://github.com/openai/openai-go) SDK。

## 可选：更大的上下文窗口

官方 runtime 把一次对话限制在 256K token，并在 64K 处截断单条用户消息。第三方补丁如 [`agy-context-unlock`](https://github.com/lili-luo/dsh-plugin-bundle/tree/main/agy-context-unlock) 通过改写 Antigravity 二进制中的机器码，把上限提高到约 1M。网关可以应用这类补丁，但**默认关闭**。是否让 runtime 运行在打过补丁的 harness 上，由你自己决定，风险也由你自己承担。

设置 `AGY_GW_HARNESS_PATCH_CMD` 后的流程：

1. 网关把官方的 `localharness_external`（或 `AGY_GW_HARNESS_PATH` 指定的文件）复制到 `AGY_GW_HARNESS_CACHE_DIR`。原文件永远不会被写入，所以只读挂载没有问题。
2. 运行该命令，副本路径作为最后一个参数追加。命令按空格拆分，不做 shell 引号解析。
3. 启动 runtime，并用 `ANTIGRAVITY_HARNESS_PATH` 指向打过补丁的副本。

副本按原文件和命令的哈希缓存，重启时直接复用。runtime 升级后哈希变化，会重新打补丁。

如果命令失败，或者文件没有任何变化，网关会记录错误并回退到官方 harness，继续以 256K 提供服务。`/readyz` 的 `harness` 字段会显示当前使用的 harness：`official`、`custom`、`patched` 或 `patch failed`。

```bash
AGY_GW_HARNESS_PATCH_CMD="python3 /opt/agy-patch/agy-patch.py --in-place --no-backup --binary" ./bin/agy-gateway serve
```

镜像里没有 Python。在容器中，可以在宿主机上给副本打好补丁再通过 `AGY_GW_HARNESS_PATH` 挂载，或者自己构建一个加了 `python3` 和脚本的镜像。窗口变大后，也请调大 `AGY_GW_REPLAY_BUDGET`，让重放的历史能利用这些空间。

验证环境：runtime 1.4.5，agy-context-unlock 截至 2026-11-08 的版本，模型 `gemini-3.8-flash-low`。网关应用了 3A、3B、4A 和 4B 四处补丁（只属于 CLI 的 1、2 和 bonus 在 harness 中不存在）。每个请求都是一条用户消息，开头和结尾各藏一个暗号：

| 消息大小 | 官方 harness | 打补丁的 harness |
|---|---|---|
| ≈135K token | 消息在 64K 处被截断。模型没看到问题，在内部绕了好几步，用了 818,048 个 prompt token。 | 两个暗号都正确，148,918 个 prompt token。 |
| ≈310K token | 未测试 | 两个暗号都正确，324,543 个 prompt token，超过了官方 256K 的上限。 |

针对未来 runtime 版本的补丁可能失败。这时网关会回退到官方 harness，`/readyz` 显示 `patch failed`。

## 限制

- **并行调用。** runtime 会把"并行"的工具调用逐个执行，所以它们通常以连续多轮、每轮一个调用的形式到达客户端。
- **用量时机。** runtime 只在一轮结束时报告用量。停在 `tool_calls` 的响应不带用量，这一轮的用量随最终响应返回。其中的 prompt token 包含 runtime 自带的系统提示词，大约 13–45k token。
- **采样参数。** 采样参数和强制工具选择无法传给 runtime。
- **修改过的历史。** 客户端修改或截断过的历史无法应用到已有会话，会新建会话并重放历史，这不是原样还原。
- **崩溃恢复。** runtime 崩溃后，空闲会话通过 ACP `session/resume` 恢复。进行中的那一轮会丢失，客户端收到错误，重试时会重放历史。
- **关闭会话。** `session/close` 尽力而为。网关不会清理 `/data` 下 runtime 的对话文件。
- **清理间隔。** 每分钟清理一次过期会话。

## 开发

| 命令 | 内容 |
|---|---|
| `make bootstrap` | 把固定版本的 golangci-lint 安装到 `bin/`。 |
| `make check` | 格式检查、golangci-lint、`go vet`（含 `integration` tag）、单元和端到端测试，以及开启 `-race` 的同一组测试。 |
| `AGY_GW_RUNTIME_CMD=/path/to/agy_acp_server.par make test-integration` | 通过官方 `openai-go` SDK 对真实 runtime 测试：模型列表、带追问的 `get_weather` 工具循环、两个城市的流式工具调用，以及流式 "hi"。 |

`make check` 不需要 runtime。`internal/app` 中的端到端测试把整个网关跑在一个模拟 ACP agent（`internal/acptest`）上，其工具调用经过真实的 MCP 桥。

集成测试需要一个已经登录的 runtime。测试不会登录或登出，CI 也不运行它们，所以 CI 不需要任何凭证。

### CI 与发版

| Workflow | 触发 | 内容 |
|---|---|---|
| [ci](.github/workflows/ci.yml) | 推送到 `main`、pull request | golangci-lint、`go vet`、Linux 和 macOS 上的 race 测试、govulncheck，以及 GoReleaser 快照构建。 |
| [docker](.github/workflows/docker.yml) | 推送到 `main`、`v*` 标签、pull request | 构建多架构镜像。`main` 推送为 `edge`，发版标签推送为对应版本，附带 provenance、SBOM 和 attestation。 |
| [release](.github/workflows/release.yml) | `v*` 标签 | GoReleaser 构建压缩包和校验和、生成 changelog、发布 GitHub Release，并对构建产物做 attestation。 |
| [codeql](.github/workflows/codeql.yml) | 推送、pull request、每周 | 对 Go 代码和 workflow 做 CodeQL 分析。 |

Dependabot 负责更新 Go 模块、actions 和基础镜像。

发版时，在 `main` 的某个提交上打语义化版本标签并推送：

```bash
git tag -a v0.1.0 -m v0.1.0
```

```bash
git push origin v0.1.0
```

欢迎贡献。提交 pull request 前请先运行 `make check`，不要在 issue 或 commit 中包含 token、登录状态或 runtime 文件。

## 许可证

agy-gateway 以 [MIT 许可证](LICENSE) 发布。

许可证只覆盖本仓库。Antigravity runtime（`agy_acp_server.par`、`localharness_external`）和 `agy` CLI 是 Google LLC 的专有软件，适用 Google 自己的条款。
