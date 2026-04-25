# Planner Guide — Creating Skills and Pipelines

## Overview

The Planner creates new skill files when a complex task has no matching skill.
You have access to `file_read` (skills dir + docs dir) and `file_write` (skills dir only).

---

## Agent Skill (.md)

```markdown
---
name: My Skill
description: What this skill does (one sentence).
complexity: medium          # simple | medium | complex
required_tools: [bash, file_read]
auto_generated: true
success_count: 0
---

You are a specialist agent. Your task:
1. ...
2. ...

When done, call finish_task with a summary.
```

**Rules:**
- `name` and `description` are required.
- `complexity` controls which LLM model is used; required.
- `required_tools` lists tools the agent will need (optional but recommended).
- Set `auto_generated: true` and `success_count: 0` for all planner-created files.

---

## Pipeline Skill (.yaml)

```yaml
name: My Pipeline
description: What this pipeline does.
complexity: complex
auto_generated: true
success_count: 0

vars:
  input: ""          # always present; populated from the user's message
  my_var: ""         # declare all pipeline-level variables here

pipeline:
  - id: step1
    complexity: medium
    prompt: |
      Do something with {{input}}.
    tools: [file_read, bash]
    outputs: result        # shorthand: stores node key "result" → $vars.result

  - id: step2
    complexity: medium
    prompt: |
      Now process: {{result}}
    outputs: answer
```

**Rules:**
- Every node must have a unique `id`.
- `type` is `agent` (default, LLM-driven) or `auto` (direct tool call, no LLM).
- `auto` nodes require a `tool:` field and no `prompt:`.
- Variables must be declared under `vars:` before use.
- `$vars.input` / `{{input}}` holds the user's original message — always available.
- `$foreach.current` and `$foreach.index` are available inside `foreach:` nodes.
- Use `outputs:` to save a node's `node_complete` vars into `$vars.*` for later nodes.
- The engine automatically injects CONTEXT.md into every agent node.
- The engine automatically injects SOUL only into the last agent node.
- On transient LLM errors the engine retries with the next lower complexity tier.

---

## Variable Syntax

| In prompts | In `inputs:` values | Foreach field |
|------------|---------------------|---------------|
| `{{name}}` or `{{$vars.name}}` | `$name` or `$vars.name` | `items` (bare) or `$vars.items` |

---

## outputs: Shorthand

```yaml
outputs: answer               # scalar: stores "result" key → $vars.answer
outputs: [answer, summary]    # list: both vars read from "result" key
outputs: {answer: result}     # explicit map (same as scalar form above)
```

---

## Common Mistakes

| Mistake | Fix |
|---------|-----|
| Using `{{foo}}` without declaring `foo` in `vars:` | Add `foo: ""` under `vars:` |
| Referencing an unknown tool | Check the tool list; use only registered tool names |
| Missing `name` or `description` | Add them — they are required |
| Missing `complexity` | Add `complexity: medium` (or simple/complex) |
| Forgetting `auto_generated: true` | Always set this for planner-created files |
