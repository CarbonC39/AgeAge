# Planner Guide — Creating Skills and Pipelines

## Overview

The Planner creates new skill files when a strong-tier task has no matching skill,
or when the user explicitly runs `/build [description]`.
You have access to `file_read` (skills dir + docs dir) and `file_write` (skills dir only).

When invoked via `/build`, the task prompt includes recent conversation context so you
can infer the intended workflow from what was already discussed.

---

## Hard Rules (apply to pipelines)

1. **First node MUST be `type: agent`.** The user's raw natural-language message arrives
   in `{{input}}` / `$vars.input`. A `type: auto` first node feeds that text into a tool's
   typed schema and crashes immediately. Use the first agent node to parse/extract the
   structured fields downstream auto nodes need.

2. **Last node MUST produce the returnable variable.** Either the variable named in
   top-level `returns:`, or one of `result` / `output` / `answer`. Its prompt MUST tell
   the sub-agent explicitly: "the user sees ONLY this value — put the COMPLETE final
   answer here." A pipeline that ends without producing a returnable variable shows the
   user nothing.

3. **Every agent node's `prompt:` must include:**
   - one-sentence goal
   - brief description of each `{{var}}` referenced
   - required output format (prose / JSON / markdown)
   - explicit note: "the user cannot see intermediate steps — put the complete result
     into node_complete vars."

4. **Every agent node's prompt must specify fallback behavior** when an upstream step
   produced empty / error / missing data — do not assume the happy path.

---

## Tier Selection (the skill's `tier:` field)

This reflects how complex the skill will be on FUTURE calls — NOT the router's tier
rating for the current request. Never copy the router's tier into the new skill.

- `base`   — single step, ≤1 tool call, no synthesis (fetch+summarize, read+reply)
- `medium` — multiple tools / 2+ sources / moderate reasoning
- `strong` — cross-system workflows, decision trees, parallel sub-tasks

Examples:
- Analyze one URL (fetch + summarize) → `tier: base`
- Compare 3 sources, write report      → `tier: medium`
- Multi-file refactor across services  → `tier: strong`

---

## Agent Skill (.md)

```markdown
---
name: My Skill
description: What this skill does (one sentence).
tier: medium            # base | medium | strong
required_tools: [bash, file_read]
auto_generated: true
success_count: 0
---

You are a specialist agent. Your task:
1. ...
2. ...

When done, call finish_task(status="success", summary=<absolute path of the file you created>).
```

**Rules:**
- `name` and `description` are required.
- `tier` selects the LLM model used when this skill is active; required.
- `required_tools` lists tools the agent will need (optional but recommended).
- Set `auto_generated: true` and `success_count: 0` for all planner-created files.

---

## Pipeline Skill (.yaml)

Minimal shape:

```yaml
name: My Pipeline
description: What this pipeline does.
tier: strong
auto_generated: true
success_count: 0

vars:
  input: ""          # always present; populated from the user's message
  my_var: ""         # declare all pipeline-level variables here

pipeline:
  - id: step1
    type: agent      # first node MUST be agent (see Hard Rule 1)
    tier: medium
    prompt: |
      Do something with {{input}}.
    tools: [file_read, bash]
    outputs: result        # shorthand: stores node key "result" → $vars.result

  - id: step2
    type: agent
    tier: medium
    prompt: |
      Now process: {{result}}.
      IMPORTANT: the user sees ONLY node_complete vars.result — put the complete
      final answer there.
    outputs: answer
```

### Worked Example — fetch + summarize

Study how it complies with the Hard Rules: first node is `agent` (parses the URL out
of natural language); last node produces the returnable variable; every prompt names
its variables, output format, fallback behavior, and the "user can't see intermediate"
note.

```yaml
name: analyze-link
description: "Fetch a URL and summarize it for the user."
tier: base
auto_generated: true
success_count: 0
vars:
  url: ""
  page: ""
returns: answer
pipeline:
  - id: extract
    type: agent
    tier: base
    prompt: |
      Goal: extract the URL the user wants analyzed.
      {{input}} is the user's raw message (natural language).
      Output: call node_complete with vars={"result": "<url-string>"}.
      The user cannot see this node — put the URL into vars.result.
      If no URL is present, set vars.result to an empty string.
    outputs: url
  - id: fetch
    type: auto
    tool: web_fetch
    inputs: { url: $vars.url }
    outputs: page
  - id: report
    type: agent
    tier: base
    prompt: |
      Goal: write the user-facing summary.
      {{page}} is the fetched page text (may be empty if fetch failed).
      If {{page}} is empty or contains an error message, explain the failure and
      suggest the user verify the URL. Otherwise produce a 3-paragraph summary:
      topic, key claims, source credibility.
      Output format: prose markdown.
      IMPORTANT: the user sees ONLY node_complete vars.result — put the COMPLETE
      summary there.
    outputs: { answer: result }
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
- On transient LLM errors the engine retries with the next lower model tier.

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
| Missing `tier` | Add `tier: medium` (or base/strong) |
| Forgetting `auto_generated: true` | Always set this for planner-created files |
