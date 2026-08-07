# Skills Reference

Skills are Markdown files that teach the agent specialised behaviour for specific task types. When a user's message matches a skill by name, the agent loads that skill's instructions into its system prompt and restricts its tool access to the skill's declared list.

Skills are stored in `{workspace}/skills/` and are **hot-reloaded every 2 seconds** — no restart needed after editing.

---

## File format

A skill file is a Markdown document with a YAML frontmatter block at the top:

```markdown
---
name: my-skill
version: "1.0"
description: "One-line summary shown to the router."
tier: medium
required_tools:
  - web_search
  - web_fetch
---

# My Skill

Instructions for the agent go here. Use plain Markdown.
You can include lists, headers, code blocks, etc.
```

The YAML block is delimited by `---` lines. Everything after the second `---` is the **prompt body**, injected verbatim into the agent's system prompt when the skill is matched.

---

## Frontmatter fields

### `name` *(string, required)*

The skill's identifier. Used for name-based matching against user input.

```yaml
name: code_review
```

Matching is **case-insensitive word-boundary regex**: a skill named `code_review` matches messages that contain the word `code_review` (not as part of a larger word). If omitted, the filename without `.md` is used as the name.

### `version` *(string, optional)*

Informational only. Useful for tracking changes, not parsed by the engine.

```yaml
version: "1.0"
```

### `description` *(string, optional)*

A one-line summary of what the skill does. Shown to the **router** when the skill is matched, helping it reason about complexity and tool selection.

```yaml
description: "Review code files for bugs, style issues, and security problems."
```

### `auto_generated` and `success_count` *(boolean/int, optional)*

Used by the **Planner** and **Evaluator**. When the Planner creates a skill automatically, it sets `auto_generated: true` and `success_count: 0`. The background Evaluator checks these skills after execution and increments `success_count` on passes.

```yaml
auto_generated: true
success_count: 0
```

### `required_tools` *(string list, optional)*

The tools the agent is allowed to use while this skill is active. Only tools that exist in the global registry (or the skill-only tool list below) can be listed.

```yaml
required_tools:
  - file_read
  - bash
  - delegate
  - finish_task
```

**`finish_task` is always injected automatically** — you do not need to include it, though doing so causes no harm.

If no skill is matched, the router determines the tool list. If a skill is matched but `required_tools` is empty, the full tool set remains available.

#### Skill-only tools

Some tools are not globally registered. They are injected only when a matched skill lists them:

| Tool name          | Description |
|--------------------|-------------|
| `grep`             | Recursive text search within files (ripgrep-style). |
| `glob`             | Recursive file pattern matching (`**` supported). |
| `update_todos`     | Maintain a live task list shown to the user and injected into every LLM context. Cleared automatically after `finish_task`. |
| `escalate`         | Spawn a sub-agent using the strongest configured model (`[router.strong]`). Use for deep reasoning subtasks. |
| `browser_navigate` | Open a URL in a managed browser session; returns the page title and readable text. Keeps the session alive across calls in the same turn. |
| `browser_action`   | Interact with the current page: click, type, fill, hover, scroll, select, press, check, uncheck. |
| `browser_content`  | Retrieve page content as readable text, raw HTML, or ARIA accessibility tree snapshot. |

All three `browser_*` tools share one `BrowserSession` per `Run()` call and clean up automatically on turn completion. Configure the browser backend in `[browser]` — see [config.md](config.md#browser).

### `tier` *(string, optional)*

When set, **bypasses the router LLM call entirely** and synthesises a routing decision directly. This saves one LLM round-trip per user turn for skills where the model tier is always known in advance.

```yaml
tier: medium
```

| Value    | Effect |
|----------|--------|
| `base`   | Router skipped; base `[llm]` model; no `delegate` tool. |
| `medium` | Router skipped; uses `[router.medium]` model if configured; no `delegate` tool. |
| `strong` | Router skipped; uses `[router.strong]` model if configured; `delegate` tool injected. |

Legacy values `direct`/`atomic`/`workflow` are still accepted.

If an unrecognised value is used, a warning is logged and the normal router flow runs instead.

When multiple skills are matched simultaneously, the **highest** tier wins.

### `segmented` *(boolean, optional)*

**Progressive instruction delivery** for long or multi-phase workflows. Must be explicitly set to `true` — segmented mode is never inferred.

```yaml
segmented: true
```

When enabled, the prompt body is split into **segments** on standalone separator lines of three or more `=` `-` or `*`:

```markdown
---
name: staged-report
description: "Produce a report in two controlled stages."
segmented: true
---

Phase 1 — gather the raw data with the tools below.

================================================================

Phase 2 — write the final report from the gathered data.
```

Only **one segment** is injected into the system prompt at a time (each replaces the previous). The framework injects a short note at the end of each segment: non-final segments instruct the agent to call the **`next_step`** tool after finishing that phase; the final segment instructs it to call `finish_task`. `next_step` is injected automatically and removed at turn end.

`finish_task(status="success")` is **blocked before the final segment** — the agent is steered back to `next_step`. Early abort is still possible via `finish_task(status="failure")`.

Requirements:
- At least **two** non-empty segments; otherwise the skill fails to load.
- This is a markdown-skill feature; YAML pipeline skills cannot be segmented.

---

## Prompt body

Everything after the closing `---` of the frontmatter is the skill's prompt, injected into the system prompt under a `## Matched Skills & Specialized Instructions` heading:

```
## Matched Skills & Specialized Instructions

### code_review
*Description: Review code for best practices, bugs, and improvement suggestions.*

[your skill body here]
```

Write the body as instructions directed at the agent. Be explicit: specify the steps to follow, the output format, and what to do in edge cases. The agent treats this content as authoritative system-level guidance.

> **Author comments:** HTML comments (`<!-- ... -->`) in the prompt body are **stripped at load time** and never reach the agent. Use them for author notes and TODO markers. Comments inside fenced code blocks (```` ``` ```` or `~~~`) are preserved — they are literal content.

---

## Matching behaviour

Skills are activated in one of two ways (mutually exclusive — only one skill is ever active per turn):

1. **Explicit command** — the user message starts with `/skill-name`. The name is normalised (lowercase, spaces and underscores → hyphens) and matched exactly. The `/skill-name` prefix is stripped from the message before it reaches the agent.
2. **Router selection** — when the router is enabled, it makes an LLM call that reads all skill descriptions and the user message, then returns the name of the best-matching skill (or none). Requires `[router] enabled = true`.

## Pipeline Skills

Instead of a single prompt, a skill can define a **structured pipeline** of execution nodes. This is ideal for multi-step processes where you want to isolate variables, enforce sequential execution, or iterate over a list.

Pipeline skills are **standalone `.yaml` files** (not `.md` files) placed in the `skills/` directory. The file contains top-level `name`, `description`, `vars`, and `pipeline` fields.

```yaml
# skills/summarize-articles.yaml
name: summarize-articles
version: "1.0"
description: "Fetch and summarize multiple articles."
vars:
  urls: []
returns: final_output
pipeline:
  - id: prepare_urls
    type: agent
    prompt: |
      Extract all URLs from the user's message and return them as vars.urls.
      Input: {{input}}
    outputs:
      urls: urls

  - id: get_content
    type: auto
    tool: web_fetch
    foreach: urls           # bare name resolves to $vars.urls
    inputs:
      url: $foreach.current
    outputs: contents       # scalar: pipeline var "contents" ← node key "result"

  - id: summarize
    type: agent
    foreach: contents       # bare name resolves to $vars.contents
    prompt: |
      Summarize this article:
      {{$foreach.current}}
    outputs: summaries      # scalar: pipeline var "summaries" ← node key "result"

  - id: final_report
    type: agent
    prompt: |
      Combine these summaries into a single report:
      {{summaries}}
    outputs: final_output
```

### Pipeline Concepts

- **Variables (`vars`)**: A shared state map. Values can be initialized in the frontmatter or passed by the user (e.g., `input`).
- **Nodes**: Executed sequentially. If a node fails, subsequent nodes are marked as `skipped` (⏭️).
- **Isolation**: Every `agent` node gets a completely fresh sub-agent. They do not share conversation history, preventing context bloat and hallucination bleed-over.
- **Node Types**:
  - `agent` (default): Spawns an LLM sub-agent. The agent's `finish_task` tool is replaced with `node_complete` to return structured data (`status`, `vars`, `reason`).
  - `auto`: Calls a tool directly without LLM reasoning. Extremely fast and deterministic.
- **Iteration (`foreach`)**: Runs the node for every item in an array. Outputs are automatically collected into arrays matching the input length.
- **First node**: Must be `agent`; use it to validate and structure the user's raw input before downstream `auto` nodes.
- **Context injection**: Every pipeline agent node receives `.ageage/CONTEXT.md`. `SOUL.md` is injected only into the last agent node when enabled for the parent mode.

**Routing:** When the router selects a pipeline skill (or it is triggered via `/skill-name`), it is automatically treated as `strong` tier — ensuring the strong model and full tool set are available to orchestrate multiple sub-tasks. This can be overridden with a top-level `tier:` field in the YAML file.

> For a complete reference on all pipeline fields, syntax, and patterns, see [pipeline.md](pipeline.md).

---

## Examples

### Minimal skill

```markdown
---
name: summarize
description: "Summarize a document or URL."
tier: medium
required_tools:
  - web_fetch
---

Fetch the given URL with `web_fetch`, then write a concise summary in plain paragraphs. Call `finish_task` with the summary.
```

### Skill with sub-agent delegation

```markdown
---
name: research
description: "Deep research using parallel web searches."
tier: strong
required_tools:
  - web_search
  - web_fetch
  - delegate
---

## Research Protocol

1. Use `web_search` to find 3-5 relevant sources.
2. For each source, use `delegate` to fetch and summarise it in parallel:
   - task: "Summarise the key points of this article relevant to the user's question."
   - tools: ["web_fetch"]
   - pre_tool: "web_fetch"
   - pre_tool_args: {"url": "<source_url>"}
3. Synthesise all summaries into a final answer and call `finish_task`.
```

### Skill with task tracking

```markdown
---
name: migration
description: "Database schema migration planning."
tier: strong
required_tools:
  - bash
  - file_read
  - file_write
  - update_todos
---

Before starting, call `update_todos` with the full task list. Mark each step as `in_progress` while working and `done` when complete. The user can see your progress in real time.
```

---

## AGENT.md and SOUL.md

AgeAge splits system-level instructions into two separate files:

### AGENT.md *(always injected)*

`AGENT.md` (`{workspace}/data/AGENT.md`) contains **execution directives** — rules about how the agent should behave regardless of persona. These are injected into every agent turn in all modes.

```markdown
# AGENT

## Execution Directives

- Use tools to gather information and perform actions.
- Call finish_task when the task is complete.
- Always respond in the same language the user uses.
- Deploy tools efficiently; avoid unnecessary calls.
```

### SOUL.md *(injected conditionally)*

`SOUL.md` (`{workspace}/data/SOUL.md`) contains the agent's **persona and communication style**. It is injected:

- In `ageage serve` and `ageage connect` modes (always)
- In `ageage cli --soul` mode (opt-in with the `--soul` flag; off by default)

```markdown
# SOUL

You are a concise, direct assistant. Avoid filler phrases.
Use clear formatting when helpful. Keep responses focused.
```

**Precedence:** AGENT.md is injected first, then SOUL.md (if active), then matched skill prompts. Skill instructions take precedence over both personality files where they conflict.

**Sub-agents** spawned by `delegate` and `escalate` inherit neither file and do not receive `.ageage/CONTEXT.md` — they operate with a minimal system prompt focused on their specific subtask.

**Pipeline nodes** receive `.ageage/CONTEXT.md`. The last agent node also receives `SOUL.md` when SOUL is enabled for the parent mode.

---

### Skill with browser automation

```markdown
---
name: web-automation
description: "Interact with JavaScript-heavy sites or pages requiring login."
tier: strong
required_tools:
  - browser_navigate
  - browser_action
  - browser_content
  - update_todos
---

Use `browser_navigate` to open the target URL, `browser_action` to interact with
page elements, and `browser_content` to read results. All three share the same
browser session within a turn. Call `finish_task` with your findings.
```

---

## Tips

- **Keep skill names unique and specific.** Generic names like `help` or `task` will match too broadly.
- **List only the tools the skill actually needs.** Fewer tools means a smaller, cleaner context for the LLM.
- **Use `tier` to skip the router** for skills with a predictable workload — it saves latency and tokens on every matched turn. Use `base`, `medium`, or `strong`.
- **Use `update_todos`** for multi-step skills where the user benefits from seeing progress (e.g. long file processing, multi-source research).
- **Skills are live-reloaded** — edit a `.md` file and the change takes effect within 2 seconds without restarting the server.
