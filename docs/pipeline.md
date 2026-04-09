# Pipeline Skills

Pipeline skills let you compose multi-step agent workflows in a single YAML file. Each step is isolated — nodes don't share memory — so data flows explicitly through a shared variable map. This makes pipelines predictable, debuggable, and safe to run in parallel.

---

## Mental model

Think of a pipeline as a small factory floor:

- **`vars`** — a shared conveyor belt carrying data between stations.
- **nodes** — isolated workers. Each gets a task, does its job, and puts its output back on the belt.
- **`inputs`** — what a node reads off the belt before starting.
- **`outputs`** — what it writes back when done.

No node can see another node's internal reasoning or history. They can only see what is explicitly passed through `vars`.

---

## File format

Pipeline skills are standalone `.yaml` files in the `skills/` directory. They are **not** Markdown files with frontmatter — the entire file is YAML.

```yaml
# skills/my-pipeline.yaml
name: my-pipeline
version: "1.0"
description: "One-line summary for the router."
complexity: complex
vars:
  topic: "" # declare variables and their defaults
pipeline:
  - id: step_one
    prompt: |
      Do something with {{$vars.input}}.
    outputs:
      result_a: result

  - id: step_two
    prompt: |
      Now do something else using: {{$vars.result_a}}
    outputs:
      answer: result
```

### Top-level fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Skill identifier. Defaults to the filename without extension. |
| `version` | string | Informational only. |
| `description` | string | Shown to the router for skill selection. |
| `complexity` | string | `simple` / `medium` / `complex`.  Pipelines default to `complex`. |
| `vars` | map | Initial variable values. `$vars.input` is always set from the user's message. |
| `pipeline` | list | Ordered list of nodes executed sequentially. |

---

## Variables

Variables are the only way data moves between nodes. They live in a single shared map for the duration of the pipeline run.

### Built-in variables

| Variable | Value |
|----------|-------|
| `$vars.input` | The user's message. Always set automatically. |

### Declaring variables

Declare variables and their defaults in the top-level `vars` block:

```yaml
vars:
  urls: []       # an empty list
  topic: ""      # an empty string
  max_results: 5 # a number
```

If omitted from `vars`, a variable is `nil` until written by a node.

### Reference syntax

Use these in `inputs` values and `foreach`:

| Syntax | Resolves to |
|--------|-------------|
| `$vars.name` | The current value of pipeline variable `name` |
| `$foreach.current` | The current iteration item (foreach nodes only) |
| `$foreach.current.field` | A named field of the current item (when items are maps) |
| `$foreach.index` | The zero-based iteration index (int) |

### Template syntax

Use these inside `prompt` strings:

| Template | Resolves to |
|----------|-------------|
| `{{$vars.name}}` | Pipeline variable `name` (as string) |
| `{{$foreach.current}}` | Current foreach item |
| `{{$foreach.index}}` | Current foreach index |
| `{{$config.workspace}}` | The workspace directory path |

---

## Node fields

Every node in the `pipeline` list supports these fields:

### Common fields (all node types)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `id` | string | **required** | Unique node name. Shown in the pipeline progress display. |
| `type` | string | `agent` | `agent` (LLM-driven) or `auto` (direct tool call). |
| `foreach` | string | — | A `$vars.name` reference to an array. The node runs once per item. |
| `concurrency` | int | `0` | Max parallel foreach iterations. `0` or `1` = sequential. |
| `inputs` | map | — | Arguments passed to the node. String values are resolved as variable references. |
| `outputs` | map | — | Maps pipeline var names ← node output keys. |
| `validate` | string | — | `not_empty`: fail if any resolved input is empty. |

### Agent-only fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `prompt` | string | — | The task given to the node's agent. Supports template syntax. |
| `skill` | string | — | Activate a named skill inside this node. May reference another pipeline skill (nested, max 1 level). |
| `tools` | list | — | Tool allowlist for this node. If empty and no skill, all global tools are available. |
| `complexity` | string | — | `simple` / `medium` / `complex`. Selects the LLM model for this node. |
| `inject_soul` | bool | `false` | Whether to include `SOUL.md` in this node's system prompt. |
| `no_context` | bool | `false` | Suppress `.ageage/CONTEXT.md` injection for this node. |
| `output_context` | bool | `false` | Allow this node to pass a context string to all subsequent nodes via `node_complete`. |

### Auto-only fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tool` | string | **required** | The tool to call directly (no LLM). |

---

## Node types

### `agent` (default)

Spawns an isolated sub-agent. The agent:

- Gets a fresh conversation with no shared history
- Receives `.ageage/CONTEXT.md` by default (suppress with `no_context: true`)
- Has `finish_task` replaced by `node_complete` (see below)
- Writes outputs back to the pipeline via `node_complete`

```yaml
- id: summarize
  type: agent  # or omit — agent is the default
  tools:
    - web_fetch
  prompt: |
    Fetch and summarize this page: {{$vars.url}}
  outputs:
    summary: result # $vars.summary ← node_complete vars.result
```

### `auto`

Calls a tool directly — no LLM, no agent, just the tool. Extremely fast and deterministic. Use for mechanical steps like reading a file, running a command, or fetching a URL.

```yaml
- id: fetch_page
  type: auto
  tool: web_fetch
  inputs:
    url: $vars.target_url
  outputs:
    page_content: result # $vars.page_content ← tool return value
```

**`auto` outputs:** The tool's return string is always available as `result`. If the tool returns a JSON object, its top-level keys are also available directly:

```yaml
outputs:
  title: title # from a JSON {"title": "...", "content": "..."} response
  body: content
```

---

## The `node_complete` tool

Agent nodes use `node_complete` instead of `finish_task`. It is the primary way a node returns data to the pipeline.

| Argument | Type | Required | Description |
|----------|------|----------|-------------|
| `status` | string | yes | `"success"` or `"failure"` |
| `vars` | object | no | Output variables. Keys should match the node's declared `outputs`. |
| `reason` | string | when failing | Human-readable explanation. Terminates the whole pipeline. |
| `context` | string | no | Key findings for subsequent nodes (only used when `output_context: true`). |

```
node_complete({
  "status": "success",
  "vars": {
    "result": "The page is about X and covers Y."
  }
})
```

If an agent returns without calling `node_complete` (e.g., hits max iterations), the pipeline falls back to using the last assistant message as `vars.result`.

---

## Patterns

### Basic two-node pipeline

```yaml
name: research-and-summarize
description: "Fetch a URL and summarize it."
vars:
  url: ""
pipeline:
  - id: fetch
    type: auto
    tool: web_fetch
    inputs:
      url: $vars.input # user's message is the URL
    outputs:
      raw_content: result

  - id: summarize
    prompt: |
      Summarize the following web page content in 3–5 bullet points:

      {{$vars.raw_content}}
    outputs:
      answer: result
```

### Foreach — process a list sequentially

```yaml
name: batch-summarize
description: "Summarize multiple URLs one by one."
vars:
  urls: []
pipeline:
  - id: fetch_pages
    type: auto
    tool: web_fetch
    foreach: $vars.urls
    inputs:
      url: $foreach.current
    outputs:
      pages: result # collected as an array after all iterations

  - id: summarize_each
    foreach: $vars.pages
    prompt: |
      Summarize this content in 2 sentences:

      {{$foreach.current}}
    outputs:
      summaries: result
```

### Foreach — process in parallel

Add `concurrency` to run iterations simultaneously:

```yaml
- id: analyze_articles
  foreach: $vars.article_urls
  concurrency: 4 # up to 4 simultaneous sub-agents
  tools:
    - web_fetch
  prompt: |
    Fetch and analyze this article: {{$foreach.current}}
  outputs:
    analyses: result
```

Results are always written to `$vars.analyses` in index order, regardless of completion order.

### Foreach over a list of objects

When the foreach array contains maps, use `$foreach.current.field` to access named fields:

```yaml
vars:
  files:
    - path: src/main.go
      start_line: 1
      end_line: 50
    - path: src/util.go
      start_line: 10
      end_line: 80

pipeline:
  - id: read_sections
    type: auto
    tool: file_read
    foreach: $vars.files
    inputs:
      path: $foreach.current.path
      start_line: $foreach.current.start_line
      end_line: $foreach.current.end_line
    outputs:
      sections: result
```

### `output_context` — passing findings forward

When a node discovers important facts that all subsequent nodes should know, use `output_context: true`. The node calls `node_complete` with a `context` string, which gets prepended to every subsequent node's prompt automatically.

```yaml
pipeline:
  - id: gather_facts
    output_context: true
    tools:
      - bash
      - file_read
    prompt: |
      Explore the repository and find: the main entry point, the config file format,
      and how tests are run. Report your findings concisely.
    # No outputs needed — context string carries the information forward

  - id: write_readme
    prompt: |
      Write a README.md for this project.
      # The gather_facts findings are automatically prepended to this prompt
```

The pipeline's final result is the joined text of all accumulated context strings (if any exist).

### `no_context` — suppress workspace context

For nodes doing pure generation or processing external data, the workspace `CONTEXT.md` is irrelevant. Suppress it to reduce noise:

```yaml
- id: translate
  no_context: true # don't inject .ageage/CONTEXT.md
  prompt: |
    Translate the following text to French:

    {{$vars.original_text}}
  outputs:
    translated: result
```

### Nested pipeline skills

A node can invoke another pipeline skill by name using `skill:`. This runs the nested pipeline as a sub-pipeline and surfaces its final result.

```yaml
pipeline:
  - id: deep_research
    skill: research # calls the "research" pipeline skill
    inputs:
      input: $vars.query # sets $vars.input in the nested pipeline
    outputs:
      findings: result # nested pipeline's final output
```

Nesting is limited to **1 level deep**. A pipeline skill running inside a node cannot itself invoke another pipeline skill.

### Validate inputs before running

Use `validate: not_empty` to fail fast if required inputs are missing:

```yaml
- id: process
  validate: not_empty
  inputs:
    content: $vars.raw_content # fails immediately if this is empty
  prompt: |
    Process: {{$vars.raw_content}}
```

---

## `inputs` value types

`inputs` accepts any YAML type. String values are resolved as variable references; everything else passes through as-is:

```yaml
inputs:
  url: $vars.target_url # string reference → resolved
  timeout: 30 # integer → passed through unchanged
  headless: true # boolean → passed through unchanged
  headers: # map → passed through; string values inside are resolved
    Authorization: $vars.token
  allowed_domains: # list → passed through; string items inside are resolved
    - $vars.primary_domain
    - example.com
```

---

## Output variable names

The `outputs` map connects node output keys to pipeline variable names:

```yaml
outputs:
  pipeline_var_name: node_output_key
```

- **For `agent` nodes**: `node_output_key` must match a key in the `vars` object passed to `node_complete`.
- **For `auto` nodes**: `node_output_key` is `result` (always available), or a top-level key from a JSON object response.

After a node completes, the mapped values are written into `e.vars` and become available as `$vars.pipeline_var_name` for all subsequent nodes.

**For `foreach` nodes**, outputs are automatically collected into arrays:

```yaml
# After a foreach over 3 items:
outputs:
  summaries: result # → $vars.summaries = ["...", "...", "..."]
```

---

## Final result

After all nodes complete, the pipeline determines its final result in this order:

1. **Accumulated context**: If any `output_context` nodes wrote context strings, they are joined and returned.
2. **Common output variables**: Checks `$vars.result`, then `$vars.output`, then `$vars.answer`.
3. **Fallback**: `"Pipeline completed successfully."`

---

## Error handling

- If a node fails (tool error, `node_complete` with `status: "failure"`, or validation error), the pipeline stops immediately.
- All remaining nodes are marked as **skipped** in the progress display.
- The error message is surfaced to the user.

There is no retry or partial recovery — design pipelines so that each node is robust enough to succeed on its own, or use `validate: not_empty` to surface missing data early.

---

## Worked example: multi-source research report

```yaml
# skills/research-report.yaml
name: research-report
description: "Search multiple queries in parallel, then synthesize a report."
vars:
  queries: []
pipeline:
  - id: search_all
    type: auto
    tool: web_search
    foreach: $vars.queries
    concurrency: 3
    inputs:
      query: $foreach.current
    outputs:
      search_results: result

  - id: fetch_articles
    type: auto
    tool: web_fetch
    foreach: $vars.search_results
    concurrency: 3
    inputs:
      url: $foreach.current
    outputs:
      articles: result

  - id: extract_facts
    foreach: $vars.articles
    concurrency: 3
    no_context: true # pure extraction, no workspace context needed
    output_context: true # findings flow forward automatically
    tools:
      - finish_task # not needed — node_complete is injected automatically
    prompt: |
      Extract the 3 most important facts from this article:

      {{$foreach.current}}

      Call node_complete with:
      - status: "success"
      - context: a brief summary of the key facts (2–3 sentences)

  - id: write_report
    complexity: complex # use the strongest model for synthesis
    prompt: |
      Write a comprehensive research report based on the findings above.
      Structure it with an executive summary, key findings, and conclusion.
    outputs:
      report: result
```

---

## Tips

- **Keep nodes small.** A node that does one thing is easier to debug than one that does five.
- **Name output keys clearly.** `search_results`, `article_content`, `final_summary` — not `data`, `out`, `result` everywhere.
- **Use `auto` for mechanical steps.** File reads, web fetches, and shell commands don't need an LLM.
- **Use `no_context` for pure generation.** Nodes translating, formatting, or generating from scratch don't benefit from workspace context.
- **Use `output_context` sparingly.** It's powerful for multi-node research, but clutters the prompt for simple sequential pipelines.
- **Declare all variables in `vars`.** Even as empty strings. It documents the pipeline's data contract at a glance.
