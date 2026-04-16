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

| Key                  | Type         | Default        | Description |
|----------------------|--------------|----------------|-------------|
| `max_iterations`     | int          | `20`           | Hard limit on tool-call rounds per user turn. |
| `mode`               | string       | `"supervised"` | `"full"` allows all tools without confirmation. `"supervised"` prompts the user before destructive actions (bash, file_write, file_edit). |
| `non_include_tools`  | string list  | `[]`           | Tools to never register. Supports exact names (`"bash"`) and prefix matching (`"cron"` excludes all cron tools, `"memory_"` excludes all memory tools). `finish_task` cannot be excluded. |
| `max_parallel_tools` | int          | `0`            | Maximum number of tool calls that may execute concurrently within a single LLM response. `0` or `1` = sequential (default). `>1` = parallel; when combined with streaming, tools whose JSON arguments complete during the stream are dispatched immediately without waiting for the full response. |

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
auto_allow_commands  = ["git", "ls", "cat"]
max_output_bytes     = 4194304   # 4 MB
passthrough_env_vars = []
```

| Key                    | Type        | Default   | Description |
|------------------------|-------------|-----------|-------------|
| `auto_allow_commands`  | string list | `[]`      | Commands that skip supervised confirmation. Supports prefix matching: `"git"` auto-allows `git status`, `git log`, etc. Only relevant when `agent.mode = "supervised"`. |
| `max_output_bytes`     | int         | `4194304` | Hard cap on combined stdout+stderr buffered in memory per command. Output beyond this limit is discarded and a truncation notice is appended. Prevents OOM when the agent runs commands that emit large volumes of data (log tails, bulk conversions, etc.). |
| `passthrough_env_vars` | string list | `[]`      | Additional env var name prefixes forwarded to subprocesses (case-insensitive). By default subprocesses run with a restricted environment — only a safe allowlist of variables (PATH, HOME, locale, common dev tools) is inherited. Use this to expose project-specific env vars that commands legitimately need. **Do not add vars that contain secrets.** |

### Shell selection

On **Linux / macOS** commands run under `sh -c`. On **Windows**, AgeAge prefers `pwsh` (PowerShell Core 7+) for better POSIX-command compatibility (aliases for `ls`, `cat`, `grep`, etc.); it falls back to `powershell` (Windows PowerShell 5.1) if `pwsh` is not found.

### Environment isolation

Subprocesses receive a restricted environment derived from the parent process. Only variables matching a safe allowlist of prefixes are forwarded:

- Core: `PATH`, `HOME`, `USER`, `SHELL`, `TERM`, `LANG`, `LC_*`, `TMPDIR`, `TEMP`, `TMP`
- Windows system: `USERNAME`, `USERPROFILE`, `APPDATA`, `SYSTEMROOT`, `WINDIR`, `COMSPEC`, `PATHEXT`, `PSMODULEPATH`
- Dev tools: `GOPATH`, `GOROOT`, `JAVA_HOME`, `VIRTUAL_ENV`, `CONDA_*`, `PYTHONPATH`, `CARGO_HOME`, `NODE_PATH`, `NODE_ENV`
- Git identity: `GIT_AUTHOR_*`, `GIT_COMMITTER_*`
- SSH: `SSH_AUTH_SOCK`, `SSH_AGENT_PID`

Variables whose names end with `_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD`, `_PASS`, or `_CREDENTIAL` are always blocked regardless of the allowlist, protecting API keys and other secrets that the AgeAge process itself holds (e.g. `OPENAI_API_KEY`).

---

## `[web_search]`

```toml
[web_search]
backend         = "duckduckgo"
max_results     = 10
blocked_domains = []

# SearXNG backend
searxng_url     = ""

# Tavily backend (https://tavily.com)
tavily_api_key  = ""   # or set TAVILY_API_KEY env var

# Brave Search backend (https://brave.com/search/api/)
brave_api_key   = ""   # or set BRAVE_API_KEY env var
```

| Key               | Type        | Default        | Description |
|-------------------|-------------|----------------|-------------|
| `backend`         | string      | `"duckduckgo"` | Search backend: `"duckduckgo"`, `"searxng"`, `"tavily"`, or `"brave"`. |
| `max_results`     | int         | `10`           | Maximum results returned per query (Brave caps at 20). |
| `blocked_domains` | string list | `[]`           | Domains to exclude (e.g. `["youtube.com"]`). Subdomain matching included. |
| `searxng_url`     | string      | `""`           | Full URL of your SearXNG instance (required for `backend = "searxng"`). |
| `tavily_api_key`  | string      | `""`           | Tavily API key. Falls back to `TAVILY_API_KEY` env var. |
| `brave_api_key`   | string      | `""`           | Brave Search API key. Falls back to `BRAVE_API_KEY` env var. |

### Backends

| Backend      | Quality | Rate limits | Setup |
|--------------|---------|-------------|-------|
| `duckduckgo` | Good    | Scraping; may rate-limit | None |
| `searxng`    | Good    | Self-hosted | Running SearXNG instance |
| `tavily`     | Excellent | API quota (free tier available) | [tavily.com](https://tavily.com) API key |
| `brave`      | Excellent | API quota (free tier: 2000 req/mo) | [brave.com/search/api](https://brave.com/search/api/) API key |

### Fallback behaviour

All configured backends fall back to DuckDuckGo on failure:

- **`tavily`** — falls back if `tavily_api_key` is empty or the API returns an error.
- **`brave`** — falls back if `brave_api_key` is empty or the API returns an error.
- **`searxng`** — falls back if the instance is unreachable or returns a non-200 response.

A warning is printed to stdout whenever a fallback occurs.

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

**Hardcoded blocks:** `credentials.toml` is unconditionally blocked from all file tools regardless of any config setting. This cannot be overridden.

---

## Credentials

AgeAge can store named secrets (API tokens, passwords, SSH keys) encrypted alongside `config.toml`. The agent references them via `{{cred:name}}` placeholders — it never sees the actual values.

### Storage and encryption

| Item | Location |
|------|----------|
| Encrypted credential store | `<workspace>/credentials.toml` (binary, AES-256-GCM) |
| Master key | `os.UserConfigDir()/ageage/master.key` (hex, 0600) |

The master key is **auto-generated** on first use. It lives outside the workspace so that `credentials.toml` cannot be decrypted if only the workspace is leaked. Back up `master.key` separately if you need portability.

### CLI management

```sh
ageage cred keygen               # Show master key path (auto-generated if missing)
ageage cred list                 # List stored credential names (never values)
ageage cred add <name>           # Prompt for value with no terminal echo
ageage cred set <name> <value>   # Inline set (for scripts / non-interactive use)
ageage cred remove <name>        # Delete a credential
```

All commands accept `-c <path>` to specify a config file (same as other `ageage` commands).

### IM channel management

```
/cred list           — list stored names
/cred remove <name>  — remove a credential
/cred reload         — hot-reload from disk (after CLI edits while the bot is running)
```

`/cred set` and `/cred add` are **permanently blocked** in IM — credential values must never appear in chat logs.

### Agent usage

When credentials are stored, the agent's system prompt is automatically extended with a list of available names:

```
## Stored Credentials
Use `{{cred:name}}` as a placeholder in tool call arguments. Available names:
- {{cred:deploy_key}}
- {{cred:db_password}}
The credentials file is system-protected. Do not attempt to read it directly.
```

The agent uses the placeholder in tool arguments:

```
Run the deploy script: bash("./deploy.sh --password {{cred:deploy_pass}}")
```

Substitution happens in-memory immediately before tool execution. Tool results are scrubbed — any credential value that appears in a tool's stdout/stderr is replaced with `[REDACTED]` before being stored in conversation history.

### Security layers

Three independent mechanisms prevent the agent from reading `credentials.toml`:

1. **Security checker** — `credentials.toml`'s absolute path is added to the hardcoded blocked-file list at startup. `file_read`, `file_write`, and `file_edit` are rejected before execution.
2. **Tool dispatch pre-check** — the raw JSON arguments of every tool call are scanned for the credentials file path before execution. Matches are rejected with an error returned to the agent.
3. **System prompt declaration** — the agent is told the file is system-protected and not to attempt direct access.

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
| `parallel` | bool | `false` | Process messages from all users concurrently. When `false`, messages are queued per chat. |

### Group chat behaviour

AgeAge distinguishes **group chats** (multiple users in one room) from **direct messages** (private 1-on-1 chat with the bot). The rules differ to avoid spamming group rooms and to prevent unauthorised access:

| Scenario | Bot responds when | Session scope |
|----------|------------------|---------------|
| **DM / private chat** | Always (for any whitelisted user) | Per room (each DM is already unique per user) |
| **Group room** | Only when @mentioned or replied to | Per room (all users share one context) |

In group chats the entire room shares a single session. If a user wants a private conversation with the bot, they should start a DM. Multiple users talking in the same room will see each other's messages in the shared history; the bot labels each message with the sender's ID so it can track who said what.

> **`allowed_users` in group chats:** If `allowed_users` is left empty in a group room, all messages from that room are **denied** and a warning is printed at startup. This is a safety default — an unconfigured bot in a public room should not respond to strangers. In DMs, an empty `allowed_users` still means "allow everyone" (personal assistant pattern). **Always configure `allowed_users` before adding the bot to a group.**

### Channel UX features

All IM channels share these UX behaviours when the platform supports them:

| Feature | Telegram | Discord | Matrix |
|---------|----------|---------|--------|
| Typing indicator (keep-alive) | ✅ | ✅ | ✅ |
| ⏳ reaction while processing | ✅ | ✅ | ✅ |
| ❌ reaction on error | ✅ | ✅ | ✅ |
| Read receipts | — | — | ✅ |
| Thread sessions | Topics (supergroups) | — | ✅ |
| @mention required in groups | ✅ | ✅ | ✅ |

### In-chat commands

These commands are handled directly in the channel handler and never routed through the agent.

| Command | Description |
|---------|-------------|
| `/clear` | Clear conversation history for the current session (session stays). |
| `/stop` | Abort the running agent task. |
| `/summarize` | Compress conversation history into a summary. |
| `/undo` | Remove the last turn from history. |
| `/retry [text]` | Re-run the last message, optionally with additional text appended. |
| `/sessions` | List all sessions for this room/chat, newest first. Matrix sessions include a `matrix.to` link to jump to the thread. |
| `/session list\|ls` | List sessions scoped to this chat. |
| `/session new\|n [name]` | Create a new session and switch to it. On Matrix top-level messages, the bot's reply starts a thread — continue in that thread to stay in the session. |
| `/session switch\|sw <name>` | Switch to an existing session (prefix matching supported). |
| `/session remove\|rm <name>` | Move a session to the OS trash (fallback: permanent delete). Cannot remove the currently active session. |
| `/cred list\|ls` | List stored credential names. |
| `/cred remove\|rm <name>` | Remove a credential. |
| `/cred reload` | Hot-reload credentials from disk (after CLI edits). |
| `/help` | Show available commands. |

> **Session scope:** Each room/channel/DM has its own session namespace. A session named `research` in one Telegram chat is independent from `research` in another. Matrix threads each get their own isolated session that resumes automatically after a restart.

### `[channels.telegram]`

```toml
[channels.telegram]
enabled       = true
bot_token     = "123456:ABC-..."
allowed_users = ["123456789", "987654321"]  # Telegram numeric user IDs
```

| Key             | Type        | Default | Description |
|-----------------|-------------|---------|-------------|
| `enabled`       | bool        | `false` | Enable Telegram connector. |
| `bot_token`     | string      | `""`    | Bot token from @BotFather. |
| `allowed_users` | string list | `[]`    | Telegram numeric user IDs that may interact. Empty = allow all in DMs, deny all in groups (see note above). |

Supergroup topics are supported: messages in a topic thread are treated as a separate session. In groups, the bot only responds when @mentioned (e.g. `@YourBot do this`) or when Privacy Mode delivers a message (bot must be admin or Privacy Mode must be disabled for the bot to receive all messages).

**Finding your Telegram user ID:** Forward any message from your account to [@userinfobot](https://t.me/userinfobot) and it will reply with your numeric ID.

### `[channels.discord]`

```toml
[channels.discord]
enabled       = true
bot_token     = "your-discord-bot-token"
channel_ids   = ["123456789012345678"]
allowed_users = ["111222333444555666"]  # Discord user IDs (snowflakes)
```

| Key             | Type        | Default | Description |
|-----------------|-------------|---------|-------------|
| `enabled`       | bool        | `false` | Enable Discord connector. |
| `bot_token`     | string      | `""`    | Bot token from the [Discord developer portal](https://discord.com/developers/applications). |
| `channel_ids`   | string list | `[]`    | Channel IDs to monitor. Required — the bot ignores all other channels. |
| `allowed_users` | string list | `[]`    | Discord user IDs (snowflakes) that may interact. Empty = allow all in DMs, deny all in guild channels. |

The bot polls each configured channel every 2 seconds for new messages (REST API — no WebSocket gateway or privileged intents required). In guild channels the bot only responds when `<@BotID>` appears in the message content.

**Finding Discord user IDs:** Enable Developer Mode in Discord (Settings → Advanced), then right-click any user and choose "Copy User ID".

### `[channels.matrix]`

```toml
[channels.matrix]
enabled       = true
homeserver    = "https://matrix.org"
user_id       = "@bot:matrix.org"
access_token  = "syt_..."
room_ids      = ["!roomid:matrix.org"]
allowed_users = ["@alice:matrix.org", "@bob:matrix.org"]
```

| Key             | Type        | Default | Description |
|-----------------|-------------|---------|-------------|
| `enabled`       | bool        | `false` | Enable Matrix connector. |
| `homeserver`    | string      | `""`    | Full URL of the Matrix homeserver (e.g. `https://matrix.org`). |
| `user_id`       | string      | `""`    | Full Matrix user ID of the bot account (e.g. `@bot:matrix.org`). |
| `access_token`  | string      | `""`    | Access token for the bot account. Generate one in Element → Settings → Help & About → Access Token. |
| `room_ids`      | string list | `[]`    | Room IDs to join and monitor. Leave empty to monitor all joined rooms. |
| `allowed_users` | string list | `[]`    | Full Matrix user IDs that may interact. Empty = allow all in DMs (2-member rooms), deny all in group rooms. |

**Getting an access token:** In Element Web, go to Settings → Help & About → scroll to the bottom → click "Access Token". Alternatively, use the Matrix login API.

**Thread sessions:** When a message arrives inside a Matrix thread (`m.thread`), it is automatically routed to a thread-specific session — independent from other threads in the same room. The session ID is derived from the thread's root event ID so history resumes correctly after a restart. In group rooms the bot only responds when its user ID (`@bot:matrix.org`) appears in the message body.

Use `/session new` from a top-level message to have the bot create a thread and start a fresh named session inside it.

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
| `TAVILY_API_KEY`  | Tavily search API key (fallback when not set in config). |
| `BRAVE_API_KEY`   | Brave Search API key (fallback when not set in config). |

`AGEAGE_API_KEY` / `OPENAI_API_KEY` are checked only when `[llm].api_key` is not set in the config file.

The credential master key is **not** read from an environment variable — it is auto-generated and stored at `os.UserConfigDir()/ageage/master.key`. Run `ageage cred keygen` to find its location.

---

## Workspace layout

```
workspace/
├── config.toml          # Main configuration
├── credentials.toml     # Encrypted credential store (AES-256-GCM; binary content)
├── data/
│   ├── AGENT.md         # Behavioural directives — always injected into every agent
│   ├── SOUL.md          # Agent persona — injected in serve/connect mode (or with --soul in CLI)
│   ├── MEMORY.jsonl     # Long-term memory entries
│   ├── cron.json        # Scheduled tasks
│   └── tmp/             # Managed temp files for converter output (auto-cleaned)
└── skills/
    ├── code_review.md   # Example markdown skill
    └── research.yaml    # Example pipeline skill
```

In CLI mode, AgeAge also creates a **`.ageage/`** directory in the current working directory (the directory you launched the CLI from). In serve/connect/mcp modes, it is created inside the workspace instead.

```
.ageage/
├── .gitignore           # Ignores tmp/ only; CONTEXT.md and settings.json are tracked
├── CONTEXT.md           # Workspace notes auto-injected into every agent system prompt
├── settings.json        # Per-directory always-allow command prefixes (CLI mode)
└── tmp/                 # Scratch space for temporary pipeline data
```

| File | Purpose |
|------|---------|
| `CONTEXT.md` | Free-form working notes injected into the system prompt when non-empty. The agent may update it without user confirmation. Keep under 2 000 characters. |
| `settings.json` | When you answer `a` (always allow) at a supervised confirmation prompt, the command prefix is recorded here and auto-allowed in future sessions from the same directory. |

### AGENT.md vs SOUL.md

| File | Injected when | Purpose |
|------|---------------|---------|
| `AGENT.md` | Always | Execution directives — tells the agent *how* to behave: use tools, call `finish_task`, respond in the user's language, etc. |
| `SOUL.md` | `serve`/`connect` mode, or `ageage cli --soul` | Persona and communication style — defines the agent's *character*, tone, and role. |

Generated by `ageage init`. Both files are plain Markdown; edit them freely.
