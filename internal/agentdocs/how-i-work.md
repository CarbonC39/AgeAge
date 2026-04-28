# How AgeAge Works

## Directory Layout

```
ageage-dir/              ← directory containing config.toml
  config.toml
  data/
    AGENT.md             ← behavioral directives (always injected)
    SOUL.md              ← personality (serve/connect mode only)
    MEMORY.jsonl         ← long-term memories
    cron.json            ← scheduled tasks
  skills/                ← skill .md and .yaml files
  credentials.toml       ← encrypted credentials

workspace/               ← file-ops working directory (set by `workspace` in config)
  .ageage/               ← runtime state; in CLI mode this is in the launch directory
    sessions/
      default/
        history.jsonl
        CONTEXT.md
      <session-id>/
        history.jsonl
        CONTEXT.md
    docs/                ← framework docs extracted at startup (read via file_read)
```

In **CLI mode** the effective working directory is always the shell's launch directory,
regardless of the `workspace` config value. In **serve/connect mode** `workspace` is used.

---

## Execution Loop

Each user message starts an iteration:

1. System prompt + full conversation history → LLM call
2. LLM responds with text and/or tool calls
3. Framework executes tool calls → results appended to history
4. Loop repeats until `finish_task` is called or `max_iterations` is reached

**`finish_task` must be called as a tool call**, not mentioned in text.
Returning text without calling it is not treated as a final answer — the loop continues.

`finish_task` requires two arguments:
- `status`: `"success"` when all work is done, `"failure"` for early exit
- `summary`: the final answer or reason for failure

**Todo guard**: if you called `update_todos` and any item is still pending, calling `finish_task(status="success")` will be rejected by the framework. Complete all todos first, or call `finish_task(status="failure")` to abort early.

---

## System Prompt Composition (top to bottom, stable prefix for KV-cache)

1. Core rules (finish_task instruction, language rule, quality requirements)
2. `AGENT.md` behavioral directives (`{ageage-dir}/data/AGENT.md`)
3. `SOUL.md` personality — serve/connect mode only (`{ageage-dir}/data/SOUL.md`)
4. CONTEXT.md of the active session (injected when non-empty)
5. Credential hint (when credentials are configured)
6. Framework documentation hint (pointer to `.ageage/docs/`)
7. Active skill instructions (changes per skill; only section that varies per turn)

Sections 1–6 are identical across turns → KV-cache hits.
Edits to AGENT.md or CONTEXT.md take effect from the **next** LLM call.

---

## Framework Documentation

Self-reference guides are extracted to `.ageage/docs/` at startup.
Read them with `file_read` — no special tool needed:

```
.ageage/docs/how-i-work.md    ← this file
.ageage/docs/troubleshooting.md
.ageage/docs/skills.md
.ageage/docs/pipeline.md
```

Read `troubleshooting.md` when a tool returns an unexpected error.
Read `skills.md` when creating or modifying a skill.
Read `pipeline.md` when building a multi-step pipeline workflow.

---

## Session CONTEXT.md

Each session has its own `CONTEXT.md` at `.ageage/sessions/{id}/CONTEXT.md`.
- Always injected into system prompt when non-empty
- `file_edit` or `file_write` targeting any session CONTEXT.md is **auto-approved**
- Keep under 2 000 characters; store decisions, key paths, constraints — not logs

---

## Tool Execution

- Multiple tool calls in one response run **in parallel** when `agent.max_parallel_tools > 1`
- Any mutation tool (file_write, bash, etc.) in the batch forces the entire batch serial
- MCP and unknown custom tools are conservatively treated as mutations
- Tool output is capped at `bash.max_output_bytes` (default 4 MB), then trimmed to
  8 000 visible runes before being stored in history

### Tool Allowlist

`agent.tools` in config is a positive allowlist of tool names. When non-empty, only the
named tools are registered. Empty (default) = all tools available.
Skill frontmatter `tools:` applies a further per-skill restriction on top of this.

---

## Message History

- Saved to `.ageage/sessions/{id}/history.jsonl` after every `Run()` call
- System message is never saved — rebuilt fresh each turn
- When message count exceeds the summarize threshold, old turns are compressed

---

## Credentials

Write `{{cred:name}}` in any tool argument. The framework substitutes the real value
before execution and scrubs it from the result. Values never appear in history.
Use `/cred list` to see available names.

---

## Skills

Markdown files in `{ageage-dir}/skills/`. Triggered by `/skill-name` prefix or
auto-selected by the router. Hot-reloaded every 2 s — no restart needed.
Read `.ageage/docs/skills.md` for the full format reference.

To explicitly create a skill or pipeline from conversation context, the user can type:
```
/build [description]
```
The Planner runs in isolation (does not modify the main conversation history) and receives
the last 8 messages as context. The new skill is available immediately via hot-reload.

---

## Parallel Tool Dispatch

When returning multiple independent tool calls (e.g., several `file_read` calls),
group them in a single response — the framework runs them in parallel when
`max_parallel_tools > 1`, saving latency. Sequential dependent calls must be in
separate responses.
