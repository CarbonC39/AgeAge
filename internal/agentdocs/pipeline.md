# Pipeline Skills — Compact Reference

Pipelines compose multi-step workflows in a single YAML file. Nodes are isolated
sub-agents; data flows only through a shared `vars` map.

---

## Hard Rules (enforced by the validator)

1. **First node must be `type: agent`.** The user's raw natural-language input arrives
   in `$vars.input` / `{{input}}`. A first node of `type: auto` would feed natural text
   into a tool's typed schema and fail at the first step. Use the first agent node to
   extract structured fields for downstream auto nodes.

2. **The pipeline must produce a returnable variable.** Either declare top-level
   `returns: <name>` where some node outputs `<name>`, OR have at least one node output
   `result` / `output` / `answer`. Otherwise the user receives no content.

3. **Each agent node's prompt should state:** goal, meaning of each `{{var}}`, output
   format, fallback behavior on empty/error upstream, and "the user cannot see
   intermediate steps."

4. **`tier:` reflects the skill's intrinsic complexity**, not the current router rating.
   Single fetch+summarize → `base`. Multi-source synthesis → `medium`. Cross-system
   workflows → `strong`.

---

## File Format

Pipeline skills are **standalone `.yaml` files** in `{ageage-dir}/skills/`.

```yaml
name: my-pipeline
description: "One-line summary for the router."
tier: strong             # base | medium | strong
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
| `tier` | Model tier for this node (`base`/`medium`/`strong`) |

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
| **Tier fallback** | On transient LLM error: `strong→medium`, `medium→base` |

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

Set `"status": "failure"` with `"reason": "..."` to fail the current node attempt.
The engine may retry an agent node once at the next lower model tier. If an agent
returns normally without calling `node_complete`, its last message becomes
`vars.result`; execution and iteration-limit errors are propagated.

---

## Common Patterns

### Sequential: fetch then summarize

```yaml
pipeline:
  - id: parse_input
    prompt: "Validate {{input}} as a URL and return it as vars.url."
    outputs: {url: url}

  - id: fetch
    type: auto
    tool: web_fetch
    inputs: { url: $url }
    outputs: raw

  - id: summarize
    prompt: "Summarize: {{raw}}"
    outputs: answer
```

### Foreach: process a list

```yaml
pipeline:
  - id: prepare_urls
    prompt: "Extract URLs from {{input}} and return them as vars.urls."
    outputs: {urls: urls}

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
# Override the [router] models for specific pipeline node tiers:
base = { model = "gpt-4o-mini" }
medium = { model = "claude-3-haiku" }
strong = { model = "gpt-4o" }
```
Each iteration in a parallel foreach loop remains an isolated sub-agent.
