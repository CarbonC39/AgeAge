# Tool Reference

This document describes every tool available to the AgeAge agent. Tools are divided into two categories:

- **Global tools** — registered in every agent by default (unless excluded via config).
- **Skill-only tools** — not registered globally; injected temporarily only when a matched skill lists them in `required_tools`.

All tools implement the same interface (`Name`, `Description`, `Parameters`, `Execute`) and are exposed to the LLM as OpenAI-compatible function definitions.

---

## Global Tools

### `finish_task`

Terminates the agent loop and returns the final answer to the user. The agent **must** call this tool when it has completed the task; without it, the loop continues until the iteration limit is reached.

| Parameter | Type   | Required | Description                                      |
|-----------|--------|----------|--------------------------------------------------|
| `summary` | string | Yes      | The final answer or summary to present to the user. |

**Notes:**
- Always present in the tool list — it cannot be excluded via `non_include_tools`.
- Calling `finish_task` triggers context summarization (if enabled).
- If `update_todos` is active, the todo list is cleared **only** when every task is in a terminal state (`done` or `skipped`). If any task is still `pending` or `in_progress`, the list is preserved across the turn so the user can see what remains and the next turn can resume where it left off.

---

### `bash`

Executes a shell command and returns the combined stdout/stderr output.

| Parameter | Type   | Required | Description              |
|-----------|--------|----------|--------------------------|
| `command` | string | Yes      | The shell command to run. |

**Behavior:**
- On Windows, commands run via `cmd /C`. On all other platforms, via `sh -c`.
- Default timeout: **30 seconds**. Commands that exceed the timeout are killed and a timeout error is returned along with any partial output.
- Output is capped at **10,000 characters**; excess is truncated.

**Security:**
- Commands are checked against the configured `blocked_commands` list before execution.
- In **supervised mode** (`agent.mode = "supervised"`), each command requires user confirmation unless it matches an entry in `bash.auto_allow_commands`. Prefix matching applies — e.g., `"git"` auto-allows `git status`, `git log`, etc.

**Config:**
```toml
[agent]
mode = "supervised"          # "full" (no prompts) or "supervised"

[bash]
auto_allow_commands = ["git", "ls", "cat"]
```

---

### `file_read`

Reads the contents of a file, optionally restricting output to a line range.

| Parameter    | Type    | Required | Description                                                                 |
|--------------|---------|----------|-----------------------------------------------------------------------------|
| `path`       | string  | Yes      | Absolute or relative path to the file.                                      |
| `start_line` | integer | No       | First line to return (1-based, inclusive). Defaults to `1`.                 |
| `end_line`   | integer | No       | Last line to return (1-based, inclusive). Defaults to `start_line + 499`.   |

**Notes:**
- Returns at most **500 lines** per call. If `end_line - start_line + 1 > 500`, `end_line` is clamped. When more lines remain beyond the returned range, a `(M-N of T lines shown)` notice is appended.
- Use `start_line` to page through large files without re-reading from the beginning.
- Relative paths are resolved against the workspace root. All symlinks are followed before the access check — a symlink that resolves outside the allowed scope is denied.

---

### `file_write`

Writes content to a file, creating the file and any missing parent directories automatically.

| Parameter | Type   | Required | Description                   |
|-----------|--------|----------|-------------------------------|
| `path`    | string | Yes      | Destination file path.         |
| `content` | string | Yes      | Content to write to the file.  |

**Notes:**
- Overwrites the file if it already exists.
- Requires confirmation in supervised mode.
- Path security check resolves all symlinks before validation; the canonical path is used for the actual write, preventing TOCTOU symlink-swap attacks.

---

### `file_edit`

Edits a file by replacing the **first** occurrence of a search string with replacement text.

| Parameter | Type   | Required | Description                                 |
|-----------|--------|----------|---------------------------------------------|
| `path`    | string | Yes      | Path to the file to edit.                   |
| `search`  | string | Yes      | Exact substring to find (must exist in file). |
| `replace` | string | Yes      | Text to substitute in place of `search`.    |

**Notes:**
- Returns an error if `search` is not found in the file.
- Only the first occurrence is replaced.
- Requires confirmation in supervised mode.

---

### `web_fetch`

Fetches the text content of a web page by URL.

| Parameter | Type   | Required | Description          |
|-----------|--------|----------|----------------------|
| `url`     | string | Yes      | URL to fetch (`http://` or `https://`; scheme is prepended if omitted). |

**Backends** (configured via `[web_fetch]`):

| Backend    | Description                                                          |
|------------|----------------------------------------------------------------------|
| `native`   | Direct HTTP GET with HTML text extraction via goquery (default).     |
| `jina`     | Proxies through [Jina Reader](https://jina.ai) (`r.jina.ai/<url>`). |
| `crawl4ai` | Uses the Python `crawl4ai` library for JS-rendered pages.            |

**Config:**
```toml
[web_fetch]
backend       = "native"          # "native", "jina", or "crawl4ai"
jina_api_key  = ""                # Optional Jina API key
crawl4ai_cmd  = "python"          # Python command for crawl4ai backend
max_characters = 20000            # Output character cap (native backend)
```

---

### `web_search`

Searches the web and returns result snippets with titles and URLs.

| Parameter | Type   | Required | Description          |
|-----------|--------|----------|----------------------|
| `query`   | string | Yes      | Search query string. |

**Backends** (configured via `[web_search]`):

| Backend      | Description                                         |
|--------------|-----------------------------------------------------|
| `duckduckgo` | Scrapes DuckDuckGo HTML results (default, no API key needed). |
| `searxng`    | Queries a self-hosted [SearXNG](https://searxng.org) instance via JSON API. |
| `tavily`     | [Tavily Search API](https://tavily.com) — AI-optimised results, free tier available. |
| `brave`      | [Brave Search API](https://brave.com/search/api/) — independent index, free tier (2000 req/mo). |

All backends fall back to DuckDuckGo automatically if the API key is missing or the request fails.

**Config:**
```toml
[web_search]
backend         = "duckduckgo"   # or "tavily" / "brave" / "searxng"
max_results     = 10
blocked_domains = ["example.com"]

searxng_url     = "http://localhost:8080"
tavily_api_key  = ""   # or set TAVILY_API_KEY env var
brave_api_key   = ""   # or set BRAVE_API_KEY env var
```

---

### `memory_store`

Appends a piece of information to long-term memory (JSONL file). Memories persist across conversations.

| Parameter | Type   | Required | Description                                     |
|-----------|--------|----------|-------------------------------------------------|
| `content` | string | Yes      | The information to remember.                    |
| `tags`    | string | No       | Comma-separated tags for categorization.        |

**Notes:**
- Each entry is given a unique ID (`mem_<timestamp>`) which can be used with `memory_forget`.
- Requires confirmation in supervised mode.
- Available in every agent run regardless of whether memories exist.

---

### `memory_recall`

Searches long-term memory for entries matching a keyword query.

| Parameter | Type   | Required | Description                    |
|-----------|--------|----------|--------------------------------|
| `query`   | string | Yes      | Keywords to search for.        |

**Notes:**
- Performs case-insensitive substring matching across `content` and `tags`.
- Only registered when the memory file is non-empty at startup.

---

### `memory_forget`

Removes a specific memory entry by ID.

| Parameter | Type   | Required | Description                             |
|-----------|--------|----------|-----------------------------------------|
| `id`      | string | Yes      | The ID of the entry to delete (e.g., `mem_1718000000000000000`). |

**Notes:**
- Only registered when the memory file is non-empty at startup.
- Requires confirmation in supervised mode.

---

### `cron_add`

Registers a recurring task with a cron schedule. Tasks are stored in `data/cron.json` and executed by the framework's cron runner.

| Parameter  | Type   | Required | Description                                                   |
|------------|--------|----------|---------------------------------------------------------------|
| `schedule` | string | Yes      | Standard 5-field cron expression (e.g., `*/5 * * * *`).       |
| `command`  | string | Yes      | Description of the task to execute on each trigger.           |

**Notes:**
- Returns the assigned task ID on success.
- Requires confirmation in supervised mode.

---

### `cron_remove`

Removes a scheduled task by ID.

| Parameter | Type   | Required | Description                    |
|-----------|--------|----------|--------------------------------|
| `id`      | string | Yes      | The cron task ID to delete.    |

---

### `cron_list`

Lists all currently registered scheduled tasks. Takes no parameters.

---

### `delegate`

Delegates a sub-task to an isolated sub-agent with its own tool set, model, and iteration budget. The main agent waits for the sub-agent to finish and receives its result.

| Parameter      | Type     | Required | Description                                                                 |
|----------------|----------|----------|-----------------------------------------------------------------------------|
| `task`         | string   | Yes      | Goal or instruction for the sub-agent.                                       |
| `tools`        | string[] | Yes      | Tool names the sub-agent may use. `delegate` and `escalate` are always excluded. |
| `pre_tool`     | string   | No       | Tool to run before the sub-agent starts; its output is injected as context.  |
| `pre_tool_args`| object   | No       | Arguments for `pre_tool`.                                                   |

**Notes:**
- Uses the independently configurable `[subagent.model]`; falls back to the base LLM if not set.
- If the configured model fails, the tool automatically retries with the base model.
- Sub-agents can use skill-only tools (like `grep`, `glob`, `browser_navigate`) if they are explicitly passed in the `tools` array.
- Only available when `subagent.enabled = true` in config.

**Config:**
```toml
[subagent]
enabled        = true
max_iterations = 20
timeout        = 120      # seconds; 0 = no timeout

[subagent.model]
model   = "gpt-4o-mini"   # optional dedicated model; inherits base LLM if empty
api_key = ""              # inherits base if empty
base_url = ""             # inherits base if empty
```

---

## Skill-Only Tools

Skill-only tools are **never** in the global registry. They are instantiated and injected into the agent's tool list for the duration of a single `Run()` call, then removed. A tool is injected only when a matched skill lists its name in `required_tools`.

**All skill-only tools at a glance:**

| Tool name          | Description |
|--------------------|-------------|
| `grep`             | Regex search within a file. |
| `glob`             | File pattern matching (`**` supported). |
| `tree`             | Directory tree listing, similar to `tree -L N`. |
| `update_todos`     | Live task list shown to the user and injected into every LLM context. |
| `escalate`         | Spawn a sub-agent using the strongest configured model. |
| `node_complete`    | Replaces `finish_task` in pipeline `agent` nodes to return structured outputs. |
| `browser_navigate` | Open a URL in a managed browser; returns page title and readable text. |
| `browser_action`   | Interact with the current page (click, fill, hover, scroll, …). |
| `browser_content`  | Retrieve page content as text, raw HTML, or ARIA accessibility tree. |

To use a skill-only tool, add it to a skill's frontmatter:

```yaml
---
name: my-skill
required_tools:
  - grep
  - glob
---
```

---

### `grep`

Searches a file for lines matching a regular expression and returns matching lines with line numbers and optional context.

| Parameter       | Type    | Required | Description                                                  |
|-----------------|---------|----------|--------------------------------------------------------------|
| `path`          | string  | Yes      | Path to the file to search.                                  |
| `pattern`       | string  | Yes      | Regular expression or plain keyword to match.                |
| `case_sensitive`| boolean | No       | Enable case-sensitive matching. Default: `false`.            |
| `context_lines` | integer | No       | Lines of context before and after each match (0–5). Default: `0`. |

**Output format:**
```
Matches for "pattern" in path/to/file (N found):

>   42: matching line text
    43: context line
  ...
>   99: another match
```

**Notes:**
- Output is capped at **200 lines**.
- The path is security-checked.
- Not registered globally — use `required_tools: [grep]` in a skill.

---

### `glob`

Finds files matching a glob pattern under a directory, with full `**` cross-directory wildcard support.

| Parameter   | Type   | Required | Description                                                      |
|-------------|--------|----------|------------------------------------------------------------------|
| `pattern`   | string | Yes      | Glob pattern. Supports `*`, `**`, and `?`.                       |
| `base_path` | string | No       | Directory to search from. Defaults to the workspace root.        |

**Pattern examples:**

| Pattern         | Matches                                          |
|-----------------|--------------------------------------------------|
| `**/*.go`       | All `.go` files anywhere in the tree             |
| `src/**/*.ts`   | All `.ts` files under `src/`                     |
| `config.*`      | `config.toml`, `config.yaml`, etc. at root       |
| `*.md`          | Markdown files in the base directory only        |

**Notes:**
- Hidden directories (names starting with `.`) are skipped.
- Results are capped at **500 paths**.
- The base path is security-checked.
- Not registered globally — use `required_tools: [glob]` in a skill.

---

### `tree`

Returns a directory tree listing similar to the Unix `tree -L N` command, with directories listed first.

| Parameter | Type    | Required | Description                                                      |
|-----------|---------|----------|------------------------------------------------------------------|
| `path`    | string  | No       | Directory to list. Defaults to the workspace root.               |
| `depth`   | integer | No       | Maximum depth to recurse (1–6). Default: `2`.                    |
| `all`     | boolean | No       | Include hidden files and directories (names starting with `.`). Default: `false`. |

**Output format:**
```
my-project/
├── src/
│   ├── main.go
│   └── handler.go
├── config.toml
└── README.md

2 directories, 3 files
```

**Notes:**
- Results are sorted: directories first, then files, both alphabetically.
- Not registered globally — use `required_tools: [tree]` in a skill.

---

### `update_todos`

Replaces the agent's current task list in full. The list is:
- **Displayed to the user** immediately. In CLI mode, printed to stdout. In channel mode (Telegram, Matrix), the first update sends a new message and all subsequent updates **edit that same message in place** until the list is cleared; platforms that do not support editing (Discord) send a new message each time.
- **Injected into every subsequent LLM context** so the agent always sees its own progress.
- **Conditionally cleared** when `finish_task` is called: if all tasks are `done`/`skipped` the list is erased; if any task is still `pending` or `in_progress`, the list is kept so the next turn can resume from the same state.
- **Cleared unconditionally** by the `/clear` command.

| Parameter | Type     | Required | Description                                              |
|-----------|----------|----------|----------------------------------------------------------|
| `todos`   | object[] | Yes      | Complete replacement list. Pass `[]` to clear the list.  |

Each todo item:

| Field    | Type   | Required | Description                                                 |
|----------|--------|----------|-------------------------------------------------------------|
| `task`   | string | Yes      | Short description of the task.                              |
| `status` | string | Yes      | One of `pending`, `in_progress`, `done`, `skipped`.         |

**Status marks in output:**

| Status       | Mark  |
|--------------|-------|
| `pending`    | `[ ]` |
| `in_progress`| `[~]` |
| `done`       | `[x]` |
| `skipped`    | `[-]` |

**Example call:**
```json
{
  "todos": [
    { "task": "Fetch the report", "status": "done" },
    { "task": "Parse results",    "status": "in_progress" },
    { "task": "Write summary",    "status": "pending" }
  ]
}
```

**Example output:**
```
Current Tasks:
[x] Fetch the report
[~] Parse results
[ ] Write summary
```

**Notes:**
- Each call is a **full overwrite** — partial updates are not supported.
- The todo state is in-memory and not persisted between conversations.
- Not registered globally — use `required_tools: [update_todos]` in a skill.

---

### `escalate`

Escalates a sub-task to a powerful sub-agent using the configured **strong model**. Use when the task demands the highest quality reasoning beyond what the base agent model can provide.

| Parameter      | Type     | Required | Description                                                                |
|----------------|----------|----------|----------------------------------------------------------------------------|
| `task`         | string   | Yes      | Goal for the sub-agent.                                                    |
| `tools`        | string[] | Yes      | Tools the sub-agent may use. `delegate` and `escalate` are always excluded. |
| `pre_tool`     | string   | No       | Tool to run before the sub-agent; output is injected as context.           |
| `pre_tool_args`| object   | No       | Arguments for `pre_tool`.                                                  |

**Model used:** `router.strong` (falls back to base LLM if not set).

**Notes:**
- Prevents infinite recursion: `delegate` and `escalate` are stripped from the sub-agent's tool list.
- The sub-agent has `NoSoul = true` and `IsSubAgent = true`. It can use skill-only tools (like `grep`, `glob`) if they are explicitly passed in the `tools` array.
- Sub-agent timeout and max iterations come from the `[subagent]` config block.
- Not registered globally — use `required_tools: [escalate]` in a skill.

---

### `browser_navigate`

Opens a URL in a managed browser and returns the page title and readable text content. Keeps the session alive so subsequent `browser_action` or `browser_content` calls operate on the same page.

| Parameter    | Type   | Required | Description                                                                                          |
|--------------|--------|----------|------------------------------------------------------------------------------------------------------|
| `url`        | string | Yes      | URL to navigate to (`http://` or `https://`; scheme is prepended if omitted).                        |
| `wait_until` | string | No       | When to consider navigation complete: `"load"` (default), `"networkidle"`, `"domcontentloaded"`, `"commit"`. |

**Returns:** Page title followed by readable text extracted via Readability.

**Notes:**
- Not registered globally — use `required_tools: [browser_navigate]` in a skill.
- One `BrowserSession` is shared across all three `browser_*` tools per `Run()` call and closed automatically on completion.

---

### `browser_action`

Performs a single interaction on the current browser page (click, fill, hover, scroll, etc.). `browser_navigate` must be called first to open a page.

| Parameter  | Type   | Required | Description                                                                                                           |
|------------|--------|----------|-----------------------------------------------------------------------------------------------------------------------|
| `action`   | string | Yes      | Action to perform: `"click"`, `"type"`, `"fill"`, `"hover"`, `"scroll"`, `"select"`, `"press"`, `"check"`, `"uncheck"`. |
| `selector` | string | No       | CSS selector for the target element (e.g. `"#submit"`, `"button:has-text('Login')"`).                                 |
| `value`    | string | No       | For `type`/`fill`: text to enter. For `select`: option value. For `press`: key name (e.g. `"Enter"`). For `scroll`: `"up"`, `"down"`, or pixel amount. |

**Notes:**
- Not registered globally — use `required_tools: [browser_action]` in a skill.

---

### `browser_content`

Retrieves content from the current browser page in a chosen format.

| Parameter  | Type   | Required | Description                                                                                              |
|------------|--------|----------|----------------------------------------------------------------------------------------------------------|
| `format`   | string | No       | `"text"` (default — readable text via Readability), `"html"` (raw HTML), or `"snapshot"` (ARIA accessibility tree). |
| `selector` | string | No       | CSS selector to restrict content to a specific element. Defaults to the full page.                       |

**Notes:**
- Not registered globally — use `required_tools: [browser_content]` in a skill.

**Config** (shared by all three browser tools):
```toml
[browser]
backend      = "playwright"   # "playwright" (default) or "agent-browser"
headless     = true
browser_type = "chromium"     # "chromium" (default), "firefox", or "webkit" — playwright only
agent_bin    = "agent-browser" # path/name of agent-browser binary
timeout      = 30             # seconds per action
```

**Example skill frontmatter:**
```yaml
---
name: web-automation
description: Interact with JavaScript-heavy sites
required_tools:
  - browser_navigate
  - browser_action
  - browser_content
---
```

---

## MCP Tools

Tools from external [Model Context Protocol](https://modelcontextprotocol.io) servers are registered dynamically at startup. Each tool's name, description, and parameter schema are fetched directly from the server.

MCP tools appear in the tool list alongside global tools and follow the same filtering rules (`non_include_tools`, router tool selection, skill `required_tools`).

**Config:**
```toml
[mcp]
enabled = true

[mcp.servers.my-server]
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]

[mcp.servers.my-server.env]
MY_VAR = "value"
```

---

## Global Tool Exclusion

Any tool (global or MCP) can be permanently excluded from all agents via config:

```toml
[agent]
non_include_tools = ["bash", "cron_add", "cron_remove"]
```

Skill-only tools are not affected by this setting — they are only ever injected when explicitly declared in a skill's `required_tools`.

---

## Supervised Mode

When `agent.mode = "supervised"`, the following tools prompt for user confirmation before executing:

- `bash` (unless the command matches `bash.auto_allow_commands`)
- `file_write`
- `file_edit`
- `memory_store`
- `memory_forget`
- `cron_add`
- `cron_remove`

In CLI mode, confirmation is read from stdin (`y` / `n` / `a` for always-allow). In channel mode, an async confirmation request is sent to the user via the messaging platform.
