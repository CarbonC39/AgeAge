# Configuration Reference

AgeAge is configured through a single TOML file, typically `config.toml` in the workspace directory. Run `ageage init` to generate a starter file interactively.

All paths in the config are resolved relative to the config file's location unless they are absolute.

> **CLI working directory:** In `ageage cli` mode the agent's file-operation root is the directory you launched the command from (not the config workspace). This means tools like `file_read`, `bash`, and `@path` attachments resolve paths relative to your shell's current directory. In `ageage serve` mode the workspace directory is used instead.

---

## `workspace`

```toml
workspace = "./workspace"
```

Root directory for all runtime data. AgeAge stores `data/AGENT.md`, `data/SOUL.md`, `data/MEMORY.jsonl`, `data/cron.json`, `data/tmp/`, and the `skills/` directory here. Created automatically if it doesn't exist.

**Default:** `"."` (config file's directory)

---

## `[llm]`

Primary language model used for all agent turns.

```toml
[llm]
api_key     = "sk-..."
base_url    = "https://api.openai.com/v1"
model       = "gpt-4o-mini"
temperature = 0.7
max_tokens  = 8192
```

| Key           | Type    | Default                          | Description |
|---------------|---------|----------------------------------|-------------|
| `api_key`     | string  | `$AGEAGE_API_KEY` or `$OPENAI_API_KEY` | API key. Can also be set via environment variable. |
| `base_url`    | string  | `https://api.openai.com/v1`     | OpenAI-compatible endpoint. Change this for Gemini, local Ollama, etc. |
| `model`       | string  | `gpt-4o-mini`                   | Model identifier passed in every request. |
| `temperature` | float   | `0.7`                           | Sampling temperature (0 = deterministic). |
| `max_tokens`  | int     | `8192`                          | Maximum tokens in each response. Set to `-1` to remove the limit. |

**Google Gemini example:**
```toml
[llm]
api_key  = "AIzaSy..."
base_url = "https://generativelanguage.googleapis.com/v1beta/openai/"
model    = "gemini-2.0-flash"
```

---

## `[agent]`

Controls the main agent loop.

```toml
[agent]
max_iterations   = 20
mode             = "supervised"
non_include_tools = []
```

| Key                | Type         | Default        | Description |
|--------------------|--------------|----------------|-------------|
| `max_iterations`   | int          | `20`           | Hard limit on tool-call rounds per user turn. |
| `mode`             | string       | `"supervised"` | `"full"` allows all tools without confirmation. `"supervised"` prompts the user before destructive actions (bash, file_write, file_edit). |
| `non_include_tools`| string list  | `[]`           | Tools to never register. Supports exact names (`"bash"`) and prefix matching (`"cron"` excludes all cron tools, `"memory_"` excludes all memory tools). `finish_task` cannot be excluded. |

---

## `[subagent]`

Settings for agents spawned by the `delegate` and `escalate` tools.

```toml
[subagent]
enabled        = true
max_iterations = 10
timeout        = 300

[subagent.model]
model    = ""
api_key  = ""
base_url = ""
```

| Key             | Type   | Default | Description |
|-----------------|--------|---------|-------------|
| `enabled`       | bool   | `true`  | Whether the `delegate` tool is registered globally. |
| `max_iterations`| int    | `10`    | Iteration cap for each sub-agent. |
| `timeout`       | int    | `300`   | Seconds before a sub-agent run is cancelled. `0` = no timeout. |

**`[subagent.model]`** — if set, sub-agents use a different model than the main agent. All three fields fall back to the `[llm]` values when empty.

| Key       | Description |
|-----------|-------------|
| `model`   | Model name for sub-agents. |
| `api_key` | API key for sub-agents (defaults to `[llm].api_key`). |
| `base_url`| Endpoint for sub-agents (defaults to `[llm].base_url`). |

If the sub-agent model fails, the `delegate` tool automatically retries with the main `[llm]` model.

---

## `[router]`

The router is a lightweight LLM call that runs before each user turn. It classifies task complexity, selects the required tools, and optionally provides a direct answer for simple queries — all without loading the full agent.

```toml
[router]
enabled     = false
max_history = 8

[router.router]   # The lightweight classification model
model    = "gemini-2.0-flash"
api_key  = "..."
base_url = "https://generativelanguage.googleapis.com/v1beta/openai/"

[router.medium]   # Model for medium-complexity tasks
model = ""        # Falls back to [llm] if empty

[router.strong]   # Model for complex tasks and escalate tool
model = ""
```

| Key          | Type | Default | Description |
|--------------|------|---------|-------------|
| `enabled`    | bool | `false` | Enable the router. When disabled, all tools are available on every turn. |
| `max_history`| int  | `8`     | Number of recent user/assistant messages forwarded to the router for context. |

**Complexity levels:**

| Level     | Behaviour |
|-----------|-----------|
| `simple`  | Router returns a direct answer; no tool calls are made. |
| `medium`  | Uses `[router.medium]` model. `delegate` tool is NOT injected. |
| `complex` | Uses `[router.strong]` model. `delegate` tool IS injected. |

**Model config sections** — each accepts `model`, `api_key`, `base_url`. Any field left empty inherits from `[llm]`.

| Section          | Used for |
|------------------|----------|
| `[router.router]`| Router's own classification calls. Use a fast, cheap model. |
| `[router.medium]`| Agent execution on medium tasks. |
| `[router.strong]`| Agent execution on complex tasks; also used by the `escalate` skill-only tool. |

> **Note:** The router always sends requests with `response_format: {type: "json_object"}` to prevent markdown-wrapped output from causing parse failures.

---

## `[summarize]`

Automatically compresses old conversation history when it grows too long.

```toml
[summarize]
enabled     = false
model       = ""
threshold   = 10
keep_recent = 4
```

| Key          | Type   | Default | Description |
|--------------|--------|---------|-------------|
| `enabled`    | bool   | `false` | Enable automatic summarization. |
| `model`      | string | `""`    | Model to use for summarization. Falls back to `[llm].model` if empty. |
| `threshold`  | int    | `10`    | Number of user/assistant message pairs before summarization triggers. |
| `keep_recent`| int    | `4`     | Number of recent message pairs to preserve verbatim after summarization. |

When triggered, messages older than `keep_recent` pairs are condensed into a single `[Previous conversation summary]` system message.

---

## `[bash]`

```toml
[bash]
auto_allow_commands = ["git", "ls", "cat"]
```

| Key                   | Type        | Default | Description |
|-----------------------|-------------|---------|-------------|
| `auto_allow_commands` | string list | `[]`    | Commands that skip supervised confirmation. Supports prefix matching: `"git"` auto-allows `git status`, `git log`, etc. Only relevant when `agent.mode = "supervised"`. |

---

## `[web_search]`

```toml
[web_search]
backend        = "duckduckgo"
max_results    = 10
blocked_domains = []
searxng_url    = ""
```

| Key              | Type        | Default         | Description |
|------------------|-------------|-----------------|-------------|
| `backend`        | string      | `"duckduckgo"`  | `"duckduckgo"` (no setup required) or `"searxng"` (requires a running instance). |
| `max_results`    | int         | `10`            | Maximum search results returned per query. |
| `blocked_domains`| string list | `[]`            | Domains to exclude from results (e.g. `["youtube.com", "csdn.net"]`). Subdomain matching is included. |
| `searxng_url`    | string      | `""`            | Full URL of your SearXNG instance (required when `backend = "searxng"`). |

When `backend = "searxng"` and SearXNG is unreachable or returns an error, the tool automatically falls back to DuckDuckGo.

---

## `[web_fetch]`

```toml
[web_fetch]
backend        = "native"
max_characters = 15000
jina_api_key   = ""
crawl4ai_cmd   = "python"
```

| Key             | Type   | Default    | Description |
|-----------------|--------|------------|-------------|
| `backend`       | string | `"native"` | Content extraction backend. |
| `max_characters`| int    | `15000`    | Character limit on returned content. |
| `jina_api_key`  | string | `""`       | Optional Jina Reader API key (improves rate limits). |
| `crawl4ai_cmd`  | string | `"python"` | Python executable for the crawl4ai backend. |

**Backends:**

| Backend     | Description |
|-------------|-------------|
| `"native"`  | Built-in Go fetcher. Runs Mozilla Readability first to strip navbars, sidebars, and footers; falls back to a goquery-based extractor for non-article pages. |
| `"jina"`    | Proxies through `r.jina.ai` for clean Markdown extraction. |
| `"crawl4ai"`| Runs Crawl4AI via a Python subprocess. Best for JavaScript-heavy pages; requires `pip install crawl4ai`. |

---

## `[browser]`

Browser automation settings. Used by the `browser_navigate`, `browser_action`, and `browser_content` skill-only tools.

```toml
[browser]
backend      = "playwright"
headless     = true
browser_type = "chromium"
agent_bin    = "agent-browser"
timeout      = 30
```

| Key           | Type   | Default          | Description |
|---------------|--------|------------------|-------------|
| `backend`     | string | `"playwright"`   | Browser backend: `"playwright"` or `"agent-browser"`. |
| `headless`    | bool   | `true`           | Run browser without a visible window. |
| `browser_type`| string | `"chromium"`     | Browser to launch (playwright only): `"chromium"`, `"firefox"`, or `"webkit"`. |
| `agent_bin`   | string | `"agent-browser"`| Command to invoke agent-browser. Supports multi-word values such as `"npx agent-browser"` or `"npx --yes agent-browser"`. |
| `timeout`     | int    | `30`             | Seconds allowed per browser action. |

**Backends:**

| Backend          | Description |
|------------------|-------------|
| `"playwright"`   | Uses `playwright-go` (native Go bindings). Requires one-time browser install: `go run github.com/playwright-community/playwright-go/cmd/playwright install chromium`. |
| `"agent-browser"`| Drives the [agent-browser](https://github.com/vercel-labs/agent-browser) CLI. Each tool call spawns a separate process; browser state persists via a named `--session`. Install: `npm i -g agent-browser && agent-browser install`. |

---

## `[multimodal]`

Controls file attachment processing for both the CLI (`@path` syntax) and the HTTP API (multipart uploads).

```toml
[multimodal]
vision         = true
max_image_bytes = 10485760   # 10 MB

# Converters turn non-image files into plain text before sending to the LLM.
# {input} is replaced with the source file path; {output} with a managed .md tmp path.
[[multimodal.converters]]
extensions = ["pdf"]
command    = "pdftotext {input} {output}"

[[multimodal.converters]]
extensions = ["docx", "odt"]
command    = "libreoffice --headless --convert-to txt:Text {input} --outdir {output}"
```

| Key               | Type   | Default    | Description |
|-------------------|--------|------------|-------------|
| `vision`          | bool   | `true`     | When `true`, images are base64-encoded and sent as `image_url` content parts directly to the LLM. When `false`, image parts are stripped and a placeholder is inserted; converters may still be used for image formats if configured. |
| `max_image_bytes` | int    | `10485760` | Maximum file size for image attachments in bytes (default 10 MB). Attachments exceeding this limit are rejected with an error. |

### `[[multimodal.converters]]`

Each converter entry handles one or more file extensions. AgeAge does not parse any format itself — it simply calls the configured command and reads the output.

| Key          | Type        | Description |
|--------------|-------------|-------------|
| `extensions` | string list | File extensions this converter handles (without leading dot, case-insensitive). |
| `command`    | string      | Command template. `{input}` is replaced with the absolute source path; `{output}` with an absolute path to a managed `.md` temp file. |

**Command template tokens:**

| Token      | Replaced with |
|------------|---------------|
| `{input}`  | Absolute path to the source file. |
| `{output}` | Absolute path to the managed output `.md` file where the converter should write its result. |

The token substitution happens at the **token level**, not via shell expansion — paths with spaces are correctly quoted as single arguments and cannot inject additional shell commands.

If the converter writes to stdout instead of `{output}`, the output is captured and written to the managed file automatically.

**Output limits:** Converter output is capped at **512 KB**. Content beyond the limit is truncated at the last newline and a `[... output truncated at 512 KB ...]` notice is appended.

**Tmp file lifecycle:** Converted output is written to `data/tmp/` inside the workspace. These files are garbage-collected automatically after each turn — a file is deleted as soon as it is no longer referenced anywhere in the conversation history.

**CLI usage:**

In CLI mode, attach files using the `@path` syntax inline with your message:

```
You ▸ Summarize @report.pdf and compare with @notes.docx
```

Each `@path` token is resolved to an absolute path, processed through the appropriate converter (or vision path for images), and attached as a content part. Tokens that are not resolvable file paths (e.g. email addresses) are left as-is in the text.

---

## `[security]`

```toml
[security]
blocked_commands = ["rm -rf /", "mkfs"]
allowed_roots    = []
forbidden_roots  = []
```

| Key               | Type        | Default | Description |
|-------------------|-------------|---------|-------------|
| `blocked_commands`| string list | (see below) | Shell command substrings that are always rejected. |
| `allowed_roots`   | string list | `[]`    | If non-empty, file operations outside the workspace are only permitted under these directories. |
| `forbidden_roots` | string list | `[]`    | File operations targeting these directories are always rejected, even if inside the workspace. |

Default blocked commands: `rm -rf /`, `rm -rf /*`, `mkfs`, `dd if=`, `:(){ :|:& };:`, `> /dev/sda`, `chmod -R 777 /`.

**Path resolution:** All file paths are first resolved against the workspace root (if relative), then all symlinks are followed to obtain the canonical path. The canonical path is what is checked against `allowed_roots` / `forbidden_roots` and what is used for the actual OS operation — a symlink inside the workspace that points outside it is denied.

---

## `[mcp]`

Connect to external Model Context Protocol servers to expose additional tools.

```toml
[mcp]
enabled = false

[mcp.servers.my-server]
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-everything"]
env     = { API_KEY = "..." }
```

| Key       | Type   | Description |
|-----------|--------|-------------|
| `enabled` | bool   | Enable MCP client. |
| `command` | string | Executable to launch the MCP server. |
| `args`    | list   | Arguments for the command. |
| `env`     | table  | Extra environment variables for the server process. |

Each server's tools are discovered at startup and registered as regular tools.

---

## `[channels]`

Connect AgeAge to instant-messaging platforms.

```toml
[channels]
parallel = false
```

| Key        | Type | Default | Description |
|------------|------|---------|-------------|
| `parallel` | bool | `false` | Process messages from all users concurrently. When `false`, messages are queued. |

### `[channels.telegram]`

```toml
[channels.telegram]
enabled   = true
bot_token = "123456:ABC-..."
```

### `[channels.discord]`

```toml
[channels.discord]
enabled     = true
bot_token   = "your-discord-bot-token"
channel_ids = ["123456789012345678"]
```

`channel_ids` restricts the bot to specific channels. Leave empty to respond in any channel.

### `[channels.matrix]`

```toml
[channels.matrix]
enabled      = true
homeserver   = "https://matrix.org"
user_id      = "@bot:matrix.org"
access_token = "syt_..."
room_ids     = ["!roomid:matrix.org"]
```

---

## `[server]`

HTTP API server used by `ageage serve`.

```toml
[server]
host = "127.0.0.1"
port = 8080
```

---

## Environment variables

| Variable          | Description |
|-------------------|-------------|
| `AGEAGE_API_KEY`  | LLM API key (checked before `OPENAI_API_KEY`). |
| `OPENAI_API_KEY`  | Fallback LLM API key. |

Both are checked only when `[llm].api_key` is not set in the config file.

---

## Workspace layout

```
workspace/
├── config.toml          # Main configuration
├── data/
│   ├── AGENT.md         # Behavioural directives — always injected into every agent
│   ├── SOUL.md          # Agent persona — injected in serve/connect mode (or with --soul in CLI)
│   ├── MEMORY.jsonl     # Long-term memory entries
│   ├── cron.json        # Scheduled tasks
│   └── tmp/             # Managed temp files for converter output (auto-cleaned)
└── skills/
    ├── code_review.md   # Example skill
    └── ...
```

### AGENT.md vs SOUL.md

| File | Injected when | Purpose |
|------|---------------|---------|
| `AGENT.md` | Always | Execution directives — tells the agent *how* to behave: use tools, call `finish_task`, respond in the user's language, etc. |
| `SOUL.md` | `serve`/`connect` mode, or `ageage cli --soul` | Persona and communication style — defines the agent's *character*, tone, and role. |

Generated by `ageage init`. Both files are plain Markdown; edit them freely.
