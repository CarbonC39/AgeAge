# AgeAge

A lightweight, modular AI agent framework written in Go. AgeAge connects any OpenAI-compatible language model to a set of tools, wraps the whole thing in a conversation loop, and delivers it through a CLI, HTTP API, IM channels, or an MCP server — all from a single binary.

---

## Architecture overview

```
┌────────────────────────────────────────────────────────────────┐
│                         Entry points                           │
│   CLI (ageage cli)   HTTP (ageage serve)   IM (ageage connect) │
│                       MCP (ageage mcp)                         │
└──────────────────────────────┬─────────────────────────────────┘
                               │
                     ┌─────────▼──────────┐
                     │   AgentFactory     │
                     │  (config + skills  │
                     │   + LLM client     │
                     │   + tool registry) │
                     └─────────┬──────────┘
                               │  CreateAgent()
                 ┌─────────────▼────────────────┐
                 │            Agent             │
                 │  ┌─────────────────────────┐ │
                 │  │  Router (optional)      │ │
                 │  │  classifies intent,     │ │
                 │  │  selects tools & model  │ │
                 │  └───────────┬─────────────┘ │
                 │              │               │
                 │  ┌───────────▼─────────────┐ │
                 │  │     Execution loop      │ │
                 │  │  LLM call → tool calls  │ │
                 │  │  → LLM call → …         │ │
                 │  └───────────┬─────────────┘ │
                 │              │               │
                 │  ┌───────────▼─────────────┐ │
                 │  │  Summarizer (optional)  │ │
                 │  │  compresses old history │ │
                 │  └─────────────────────────┘ │
                 └──────────────────────────────┘
                               │
              ┌────────────────┼────────────────┐
              │                │                │
        ┌─────▼─────┐   ┌──────▼──────┐  ┌─────▼──────┐
        │ Tool Reg. │   │   LLM API   │  │  Skills    │
        │ (tools/*) │   │ (llm/client)│  │ (*.md files│
        └───────────┘   └─────────────┘  └────────────┘
```

---

## Core concepts

### AgentFactory

`AgentFactory` is the single initialization point. It loads config, establishes the LLM client, compiles skills, and wires up all shared resources (cron store, MCP sessions, security checker). Every agent instance is created from the factory via `CreateAgent()` or `CreateAgentFiltered()`, so they all share the same underlying config and tool implementations without duplicating state.

The factory also runs a **skill watcher goroutine** (`WatchSkills`) that polls the skills directory every 2 seconds and hot-reloads `.md` files when any of them change — no restart required.

### Agent

The `Agent` struct is the conversation engine. It holds:

- `messages []llm.Message` — the full conversation history (system + user + assistant + tool results)
- `registry *tools.Registry` — the set of tools available this turn
- `router *Router` — optional intent classifier
- `summarizer *Summarizer` — optional history compressor
- `skills []skills.Skill` — matched skill definitions (refreshed on every `Run()` call)
- `todoStore *tools.TodoStore` — active task list (non-nil when `update_todos` is injected)
- `browserSess *tools.BrowserSession` — lazy browser session (non-nil when any `browser_*` tool is injected; closed on turn end)
- `tmpMgr *TmpManager` — manages temporary files produced by file converters; garbage-collected after each turn
- `pendingTurns []turnRecord` — recent uncompressed tool-call turns, used for in-place compression
- `InjectSoul bool` — when `true`, SOUL.md is appended to the system prompt (set by the factory in `serve`/`connect` modes, or via `cli --soul`)

A single `Agent` instance persists across turns; its `messages` slice grows with each exchange and is compressed or summarized when it grows too large.

### Tool Registry

`tools.Registry` is a simple `map[string]Tool` with thread-safe register/unregister/execute methods. The `Tool` interface requires four methods:

```go
type Tool interface {
    Name()        string
    Description() string
    Parameters()  map[string]interface{}   // OpenAI JSON Schema
    Execute(args json.RawMessage) (string, error)
}
```

Tools registered in the factory are **global** — available to every agent. **Skill-only tools** (`grep`, `glob`, `update_todos`, `escalate`, `browser_navigate`, `browser_action`, `browser_content`) are injected into the registry at the start of a `Run()` call when a matched skill declares them, then removed on return via `defer`.

---

## Request flow (one user turn)

```
User message
    │
    ▼
① Skill matching
    Agent scans all loaded skills for word-boundary name matches.
    Matched skills are collected; their required_tools and prompts
    are merged for this turn.
    │
    ▼
② Skill-only tool injection
    Any tool listed in a matched skill's required_tools that belongs
    to skillOnlyToolFactories (grep, glob, update_todos, escalate)
    is instantiated and registered in the agent's registry.
    Removed via defer at end of Run().
    │
    ▼
③ Router (optional, skipped for sub-agents)
    If a matched skill sets complexity: (or is a pipeline skill), the router
    LLM call is skipped and a RouterResult is synthesised directly.
    Otherwise the router model receives the user message + recent
    history and returns:
      - complexity: simple | medium | complex
      - required_tools: [...] (what the agent needs)
      - direct_answer: "..." (for simple tasks, no tool call needed)
    Router always uses response_format: json_object to prevent
    parse failures.
    │
    ▼
④ Model & tool selection
    simple  → return direct_answer immediately, no loop
    medium  → use [router.medium] model, no delegate tool
    complex → use [router.strong] model, delegate tool injected
    No router → all tools available, base model used
    web_search always implies web_fetch (auto-injected).
    │
    ▼
⑤ Execution loop  (up to max_iterations)
    ┌──────────────────────────────────────────────────┐
    │ buildCallMessages()                              │
    │   Copy a.messages + append one ephemeral user    │
    │   message:                                       │
    │   <context>Time|Workspace|OS|Arch</context>      │
    │   + <todos> if update_todos is active.           │
    │   The stored a.messages is never modified →      │
    │   all prior messages stay byte-identical for     │
    │   KV cache hits.                                 │
    │                  ↓                               │
    │   LLM call (stream or blocking)                  │
    │                  ↓                               │
    │   Strip <think>…</think> reasoning blocks        │
    │                  ↓                               │
    │   No tool calls? → return content as final answer│
    │                  ↓                               │
    │   For each tool call:                            │
    │     Execute tool → store result in history       │
    │     finish_task called? → conditional todo clear │
    │                           → summarize → return   │
    │                  ↓                               │
    │   Compress oldest pending turn (keeps last 2     │
    │   turns verbatim; older ones become a one-line   │
    │   narrative to save tokens)                      │
    └──────────────────────────────────────────────────┘
    │
    ▼
⑥ Post-turn summarization (optional)
    If message count exceeds summarize.threshold,
    older pairs are condensed into a [Previous conversation
    summary] system message; keep_recent pairs are preserved
    verbatim.
```

---

## Token optimisation

AgeAge applies several strategies to keep token usage low without sacrificing context quality.

| Strategy | Where | Effect |
|---|---|---|
| **KV cache stability** | `buildCallMessages()` | System prompt is byte-identical every turn. Dynamic info (time, todos) appended as an ephemeral message at the very end, never stored in history. |
| **In-place turn compression** | `compressOldestTurn()` | Tool-call turns older than the 2 most recent are collapsed into a single narrative assistant message, replacing N messages with 1. |
| **Tool result capping** | `compactToolResult()` | Tool outputs stored in history are capped at 4 000 runes; the full result is still used by the current iteration. |
| **Conversation summarization** | `Summarizer` | After a configurable threshold, all but the most recent N message pairs are replaced by an LLM-generated summary. |
| **Router direct answer** | `Router` | Simple factual queries are answered by the router itself without ever entering the execution loop. |

---

## Skills

Skills are Markdown files in `{workspace}/skills/`. Each file has a YAML frontmatter block and a prompt body.

```markdown
---
name: code_review
version: "1.0"
description: "Review code for bugs, style, and security."
complexity: medium          # skip router, use medium model
required_tools:
  - file_read
  - grep
---

Read the file(s) and provide structured feedback...
```

**Frontmatter fields:**

| Field | Purpose |
|---|---|
| `name` | Word-boundary regex matched against user input (case-insensitive). |
| `description` | Shown to the router to aid complexity classification. |
| `complexity` | `simple` / `medium` / `complex` — skips the router LLM call entirely. |
| `required_tools` | Tool whitelist for this turn. Union of all matched skills' lists. |

Multiple skills can match simultaneously; their tool lists are merged and the highest complexity wins.

Skills are **hot-reloaded** — edit a file and the change applies within 2 seconds.

### Pipeline Skills
Skills can also define structured execution pipelines in YAML instead of a single prompt body. This allows for iterating over lists (`foreach`), isolating tasks into sub-agents, and running tools directly without LLM overhead (`auto` nodes). See [docs/skills.md](docs/skills.md) for full details on pipelines. Pipeline skills appear with a `[pipeline]` tag in the UI and are always routed as `complex`.

---

## Sub-agents and delegation

The `delegate` global tool and `escalate` skill-only tool each spawn an isolated sub-agent with its own registry and iteration budget. Sub-agents:

- Do **not** run the router (they execute directly with the specified model)
- Do **not** load AGENT.md or SOUL.md (no personality inheritance)
- **Can** use skill-only tools (like `grep`, `glob`) if explicitly specified in the tools list
- Strip `delegate` and `escalate` from their own tool list (prevents recursion)

`delegate` uses `[subagent.model]` (falls back to base LLM if that model fails).
`escalate` always uses `[router.strong]`.

The pre-tool mechanism lets the parent agent fetch context and inject it into the sub-agent's first message before the sub-agent's loop starts, reducing the sub-agent's required iterations.

---

## Multimodal attachments

The agent's message format supports both plain-text and multimodal content. Each `llm.Message` can carry either a `Content string` or a `Parts []ContentPart` slice (OpenAI multimodal format). Custom `MarshalJSON`/`UnmarshalJSON` handles both forms transparently.

**CLI mode — `@path` attachment syntax:**

```
You ▸ Summarize @report.pdf and check @screenshot.png
```

Each `@path` token is resolved to an absolute path and processed based on file type:

| File type | `vision = true` | `vision = false` |
|-----------|-----------------|------------------|
| Image (jpg/png/gif/webp) | Base64 `image_url` content part | Converter output (if configured) or text placeholder |
| Document with converter | Converter output as text part | Converter output as text part |
| Other text/source files | Raw file content as text part | Raw file content as text part |

Non-file `@tokens` (e.g. email addresses) are left as-is in the message text.

**HTTP API (serve mode):** multimodal `content` arrays in the request body are forwarded directly to the agent. Image parts are stripped if `vision = false`.

---

## LLM client

`llm.Client` wraps any OpenAI-compatible endpoint. It provides:

- `ChatCompletion` — blocking, returns full response + usage stats
- `ChatCompletionStream` — streaming, calls a callback per token; returns final message + usage stats
- `ChatCompletionJSON` — blocking with `response_format: json_object` (used by the router)

**Gemini compatibility layer** — when `base_url` contains `generativelanguage.googleapis.com`:

- Multiple system messages are merged into one (Gemini requires exactly one)
- Consecutive same-role plain-text messages are merged
- `reasoning_content` (thought signatures) is stripped from all outbound messages
- `thought_signature` on tool calls is preserved verbatim so Gemini can verify them on the return trip

---

## Delivery modes

| Mode | Command | Description |
|---|---|---|
| **Interactive CLI** | `ageage cli` | REPL with `/clear`, `/stop`, `/summarize`, `/session` commands. Lipgloss UI with token usage display. Use `--soul` to enable SOUL.md persona. Attach files with `@path` syntax. |
| **HTTP API** | `ageage serve <dir>` | OpenAI-compatible `/v1/chat/completions` endpoint (streaming + CORS). Compatible with SillyTavern, OpenWebUI, and similar clients. |
| **IM channels** | `ageage connect` | Telegram, Discord, Matrix. Per-chat session management; typing indicators and emoji status reactions; Matrix thread sessions; `/session`, `/sessions`, `/cred` commands in-chat. |
| **MCP server** | `ageage mcp` | Exposes all registered tools as MCP tools over stdio. AgeAge itself becomes a tool provider for other AI systems. |

---

## Security model

Every bash command and file path passes through `security.Checker` before execution:

- **`blocked_commands`** — substring blocklist (e.g. `rm -rf /`)
- **`allowed_roots`** — if non-empty, file ops are restricted to these directories
- **`forbidden_roots`** — always-denied path prefixes
- **Hardcoded blocked files** — `credentials.toml` is unconditionally blocked regardless of any config (added via `sec.BlockFile()` in the factory at startup)
- **Supervised mode** — destructive tools prompt for `y / n / a` (always) confirmation before executing; `bash.auto_allow_commands` defines prefix patterns that skip confirmation

### Credential system

AgeAge stores named credentials encrypted with AES-256-GCM alongside `config.toml`. The master key is auto-generated on first use and stored at `os.UserConfigDir()/ageage/master.key` (separate from the workspace so the credential file cannot be decrypted even if the workspace is leaked).

The agent never sees credential values. Instead, it uses `{{cred:name}}` placeholders in tool call arguments; substitution happens in-memory just before each tool executes and tool results are scrubbed before being stored in conversation history.

Three independent layers prevent the agent from reading `credentials.toml` directly:
1. The security checker's hardcoded blocked-file list (covers `file_read`, `file_write`, `file_edit`)
2. The `dispatchTool` pre-check on raw JSON args (covers `bash` and any other tool)
3. A system prompt declaration telling the agent the file is off-limits

**CLI management:**

```sh
ageage cred keygen          # Show where the auto-generated master key is stored
ageage cred list            # List credential names (never values)
ageage cred add <name>      # Prompt for value (no terminal echo)
ageage cred set <name> val  # Inline set (use 'add' for sensitive input)
ageage cred remove <name>   # Remove a credential
```

**IM management** (via `/cred` channel commands):

```
/cred list          — list stored names
/cred remove <name> — remove a credential
/cred reload        — hot-reload credentials from disk after CLI changes
```

`/cred add` and `/cred set` are hardcoded to fail in IM — passwords must never travel over chat logs.

**Agent usage:**

```
Run the deployment script using the server password in {{cred:deploy_pass}}
```

---

## Directory layout

```
ageage/
├── agent/
│   ├── agent.go        # Core agent loop, RunWithParts, skill injection
│   ├── attachment.go   # CLI @path file attachment processing + converter runner
│   ├── credentials.go  # Session manager (part of agent package)
│   ├── factory.go      # AgentFactory — config, LLM client, tool registry, CredMgr
│   ├── router.go       # Intent router (complexity classification + tool selection)
│   ├── session.go      # Session manager — history persistence, Trash()
│   ├── skill_tools.go  # Skill-only tool factories (grep, glob, browser_*, …)
│   ├── subagent.go     # delegate/escalate tool implementations
│   ├── summarizer.go   # Conversation summarizer
│   └── tmpmanager.go   # Temp file lifecycle manager for converter output
├── channel/        # IM connectors (Telegram, Discord, Matrix) + optional interfaces
├── config/         # TOML config loading and structs
├── creds/
│   ├── encrypt.go  # AES-256-GCM encrypt/decrypt helpers
│   ├── key.go      # Master key auto-generation and loading
│   └── manager.go  # CredentialManager — store, Substitute, Scrub, PromptHint
├── docs/           # config.md, skills.md, tools.md
├── jsonutil/       # Robust JSON parser for LLM-generated tool arguments
├── llm/            # OpenAI-compatible HTTP client + Gemini compatibility layer
├── security/       # Command and path safety checker (BlockFile support)
├── server/         # HTTP API server (OpenAI-compatible) and MCP server
├── skills/         # Skill loader and hot-reload watcher
├── tools/
│   ├── bash.go
│   ├── browser.go      # browser_navigate/action/content + playwright/agent-browser backends
│   ├── file.go
│   ├── web.go          # web_fetch (Readability + Jina + Crawl4AI) + web_search
│   ├── memory.go
│   ├── cron.go
│   ├── finish.go
│   ├── glob.go
│   ├── grep.go
│   ├── todos.go        # update_todos + TodoStore
│   ├── mcp.go
│   └── registry.go
├── workspace/      # Default workspace (config, skills, data)
├── cliui.go        # Lipgloss CLI UI (blue-pink palette, token display)
└── main.go         # CLI entrypoint (cobra)
```

---

## Configuration quick-start

```toml
[llm]
api_key  = "sk-..."
base_url = "https://api.openai.com/v1"
model    = "gpt-4o-mini"

[agent]
max_iterations = 20
mode           = "supervised"   # or "full"

[router]
enabled = true

[router.router]                 # Fast model for classification
model    = "gemini-2.0-flash"
api_key  = "AIza..."
base_url = "https://generativelanguage.googleapis.com/v1beta/openai/"

[router.strong]                 # Strong model for complex tasks
model = "gpt-4o"
```

Run `ageage init` for an interactive setup wizard.

Full reference: [docs/config.md](docs/config.md) · [docs/skills.md](docs/skills.md) · [docs/tools.md](docs/tools.md)

---

## Dependencies

| Package | Purpose |
|---|---|
| `github.com/BurntSushi/toml` | Config file parsing |
| `github.com/PuerkitoBio/goquery` | HTML extraction fallback |
| `codeberg.org/readeck/go-readability/v2` | Mozilla Readability content extraction |
| `github.com/modelcontextprotocol/go-sdk` | MCP client and server |
| `github.com/playwright-community/playwright-go` | Browser automation (playwright backend) |
| `github.com/charmbracelet/lipgloss` | Terminal UI styling (CLI blue-pink palette) |
| `github.com/spf13/cobra` | CLI command structure |
| `gopkg.in/yaml.v3` | Skill frontmatter parsing |
