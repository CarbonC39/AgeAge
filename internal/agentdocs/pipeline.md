# Pipeline Skills — Compact Reference

Pipelines compose multi-step workflows in a single YAML file. Nodes are isolated
sub-agents; data flows only through a shared `vars` map.

---

## File Format

Pipeline skills are **standalone `.yaml` files** in `{ageage-dir}/skills/`.

```yaml
name: my-pipeline
description: "One-line summary for the router."
complexity: complex        # simple | medium | complex
vars:
  topic: ""                # declare vars and defaults; $vars.input = user message

pipeline:
  - id: step_one
    prompt: |
      Do X with {{$vars.input}}.
    outputs:
      result_a: result     # $vars.result_a ← node output

  - id: step_two
    prompt: |
      Do Y using: {{$vars.result_a}}
    outputs:
      answer: result
```

---

## Variables

| Syntax | Resolves to |
|--------|-------------|
| `$vars.name` or `{{$vars.name}}` | Pipeline variable `name` |
| `$vars.input` | User's original message (always set) |
| `$foreach.current` | Current item in a foreach loop |
| `$foreach.index` | Zero-based loop index |
| `$config.workspace` | Effective working directory for file ops |

---

## Node Fields

### Common (all types)

| Field | Description |
|-------|-------------|
| `id` | Required. Unique node name. |
| `type` | `agent` (default) or `auto` (direct tool call, no LLM) |
| `foreach` | `$vars.array` — run node once per item |
| `inputs` | Map of arguments; string values resolved as variable references |
| `outputs` | Map: `pipeline_var ← node output key` |
| `validate` | `not_empty` — fail pipeline if any resolved input is empty |
| `depends_on` | List of node IDs that must complete first |

### Agent-only

| Field | Description |
|-------|-------------|
| `prompt` | Task prompt; supports `{{$vars.x}}` template syntax |
| `tools` | Tool allowlist for this node; omit for all available tools |
| `complexity` | Model tier override for this node (`simple`/`medium`/`complex`) |
| `no_context` | `true` to suppress CONTEXT.md injection for this node |

### Auto-only

| Field | Description |
|-------|-------------|
| `tool` | Required. Tool name to call directly (no LLM, no iteration overhead) |

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
    inputs: { url: $vars.input }
    outputs: { raw: result }

  - id: summarize
    prompt: "Summarize: {{$vars.raw}}"
    outputs: { answer: result }
```

### Parallel: independent nodes run concurrently

```yaml
pipeline:
  - id: search_a
    prompt: "Search for X: {{$vars.input}}"
    outputs: { result_a: result }

  - id: search_b           # no depends_on → runs in parallel with search_a
    prompt: "Search for Y: {{$vars.input}}"
    outputs: { result_b: result }

  - id: merge
    depends_on: [search_a, search_b]
    prompt: "Combine: {{$vars.result_a}} and {{$vars.result_b}}"
    outputs: { answer: result }
```

### Foreach: process a list

```yaml
pipeline:
  - id: process_each
    foreach: $vars.urls
    type: auto
    tool: web_fetch
    inputs: { url: $foreach.current }
    outputs: { pages: result }   # foreach collects results into an array
```

---

## Foreach Concurrency

Set `pipeline.foreach_concurrency` in config to run foreach iterations in parallel
(default: sequential). Each iteration is still an isolated sub-agent.
