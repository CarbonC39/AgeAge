# Pipeline Skills — Compact Reference

Pipelines compose multi-step workflows in a single YAML file. Nodes are isolated
sub-agents; data flows only through a shared `vars` map.

---

## File Format

Pipeline skills are **standalone `.yaml` files** in `{ageage-dir}/skills/`.

```yaml
name: my-pipeline
description: "One-line summary for the router."
complexity: workflow       # direct | atomic | workflow
vars:
  topic: ""                # declare vars and defaults; input = user message

pipeline:
  - id: step_one
    prompt: |
      Do X with {{input}}.
    outputs: result_a      # shorthand: stores "result" key → $vars.result_a

  - id: step_two
    prompt: |
      Do Y using: {{result_a}}
    outputs: answer
```

---

## Variables

| Syntax | Resolves to |
|--------|-------------|
| `{{name}}` or `{{$vars.name}}` | Pipeline variable `name` (in prompts) |
| `$name` or `$vars.name` | Pipeline variable `name` (in `inputs:` values) |
| `$vars.input` / `{{input}}` | User's original message (always set) |
| `$foreach.current` | Current item in a foreach loop |
| `$foreach.index` | Zero-based loop index |
| `$config.workspace` | Effective working directory for file ops |

All old forms (`$vars.name`, `{{$vars.name}}`) still work — the new shorthands
are optional but recommended for readability.

---

## Node Fields

### Common (all types)

| Field | Description |
|-------|-------------|
| `id` | Required. Unique node name. |
| `type` | `agent` (default) or `auto` (direct tool call, no LLM) |
| `foreach` | Variable name (`items` or `$vars.items`) — run node once per item |
| `inputs` | Map of arguments; string values resolved as variable references |
| `outputs` | `{var: key}`, `[var1, var2]`, or `"var"` — see below |
| `validate` | `not_empty` — fail pipeline if any resolved input is empty |

### Agent-only

| Field | Description |
|-------|-------------|
| `prompt` | Task prompt; supports `{{name}}` and `{{$vars.name}}` syntax |
| `skill` | Name of a regular or pipeline skill to embed in this node |
| `tools` | Tool allowlist for this node; omit for all available tools |
| `complexity` | Model tier for this node (`direct`/`atomic`/`workflow`) |

### Auto-only

| Field | Description |
|-------|-------------|
| `tool` | Required. Tool name to call directly (no LLM) |

---

## outputs: Forms

| YAML | Meaning |
|------|---------|
| `outputs: {answer: result}` | Map: pipeline var `answer` ← node key `result` |
| `outputs: [answer, summary]` | List: each var reads from key `result` |
| `outputs: answer` | Scalar: `answer` reads from key `result` |

All three forms are equivalent when the node key is always `result`.

---

## Automatic Engine Behaviour

| What | How |
|------|-----|
| **CONTEXT.md** | Always injected into every agent node |
| **SOUL** | Injected only into the last agent node (if the parent agent has SOUL enabled) |
| **Complexity fallback** | On transient LLM error: `workflow→atomic`, `atomic→base` |

No per-node flags needed — the engine decides.

---

## node_complete (agent nodes)

Agent nodes use `node_complete` instead of `finish_task`:

```json
{
  "status": "success",
  "vars": { "result": "output text" }
}
```

Set `"status": "failure"` with `"reason": "..."` to terminate the pipeline early.
If an agent hits `max_iterations` without calling `node_complete`, its last message
becomes `vars.result`.

---

## Common Patterns

### Sequential: fetch then summarize

```yaml
pipeline:
  - id: fetch
    type: auto
    tool: web_fetch
    inputs: { url: $input }
    outputs: raw

  - id: summarize
    prompt: "Summarize: {{raw}}"
    outputs: answer
```

### Foreach: process a list

```yaml
pipeline:
  - id: process_each
    foreach: urls           # bare name resolves to $vars.urls
    type: auto
    tool: web_fetch
    inputs: { url: $foreach.current }
    outputs: pages          # foreach collects results into an array
```

### Nested pipeline skill

```yaml
pipeline:
  - id: deep_research
    skill: Research         # embedded pipeline skill; output flows back here
    inputs: { input: $input }
    outputs: findings

  - id: reply
    prompt: "Write a report based on: {{findings}}"
    outputs: answer
```

---

## Configuration Options

You can tune pipeline execution behavior in `config.toml`:

```toml
[pipeline]
foreach_concurrency = 1   # Set to >1 to run foreach iterations in parallel (default: sequential)

[pipeline.models]
# Override the [router] models for specific pipeline node complexities:
simple = { model = "gpt-4o-mini" }
medium = { model = "claude-3-haiku" }
complex = { model = "gpt-4o" }
```
Each iteration in a parallel foreach loop remains an isolated sub-agent.
