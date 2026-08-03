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

The first node may be `auto` when its arguments are already deterministic. Keep
in mind that `$vars.input` is the user's raw string: use an initial `agent` node
when structured fields must be extracted or validated before calling a typed tool.

---

## File format

Pipeline skills are standalone `.yaml` files in the `skills/` directory. They are **not** Markdown files with frontmatter — the entire file is YAML. Unknown top-level or node fields are rejected, so misspellings fail at load time instead of being silently ignored.

> **Hot reload**: changes to `.yaml` (and `.yml`) pipeline files are detected automatically alongside `.md` skill changes. No restart required.

```yaml
# skills/my-pipeline.yaml
name: my-pipeline
version: "1.0"
description: "One-line summary for the router."
tier: strong
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
| `tier` | string | `base` / `medium` / `strong`. Pipelines default to `strong`. |
| `vars` | map | Initial variable values. `$vars.input` is always set from the user's message. |
| `pipeline` | list | Ordered list of nodes executed sequentially. |
| `returns` | string | Name of the pipeline variable returned to the user. Strongly recommended. |

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
  urls: []         # list
  topic: ""        # string
  max_results: 5   # number
  enabled: true    # boolean
```

If omitted from `vars`, a variable is `nil` until written by a node.

Variable defaults and literal values in `inputs` retain their YAML types. Exact
references such as `$vars.max_results` therefore pass a number to an `auto`
tool. Prompt interpolation converts values to text; maps and lists use JSON.

### Reference syntax

Both the bare (`$vars.x`) and brace (`{{$vars.x}}`) forms work identically in **both** `inputs` values and `prompt` strings:

| Syntax | Resolves to |
|--------|-------------|
| `$vars.name` or `{{$vars.name}}` | The current value of pipeline variable `name` |
| `$foreach.current` or `{{$foreach.current}}` | The current iteration item (foreach nodes only) |
| `$foreach.current.field` | A named field of the current item (when items are maps) |
| `$foreach.index` or `{{$foreach.index}}` | The zero-based iteration index (int) |
| `$config.workspace` or `{{$config.workspace}}` | The workspace directory path |

---

## Node fields

Every node in the `pipeline` list supports these fields:

### Common fields (all node types)

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `id` | string | **required** | Unique node name. Shown in the pipeline progress display. |
| `type` | string | `agent` | `agent` (LLM-driven) or `auto` (direct tool call). |
| `foreach` | string | — | A `$vars.name` reference to an array. The node runs once per item. |
| `inputs` | map | — | Arguments passed to the node. String values are resolved as variable references. |
| `outputs` | map | — | Maps pipeline var names ← node output keys. |
| `validate` | string | — | `not_empty`: fail if any resolved input is empty. |

### Agent-only fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `prompt` | string | — | The task given to the node's agent. Supports template syntax. |
| `skill` | string | — | Activate a named skill inside this node. May reference another pipeline skill (nested, max 1 level). |
| `tools` | list | — | Tool allowlist for this node. If empty and no skill, all global tools are available. |
| `tier` | string | — | `base` / `medium` / `strong`. Selects the LLM model for this node. |

### Auto-only fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `tool` | string | **required** | The tool to call directly (no LLM). |

---

## Node types

### `agent` (default)

Spawns an isolated sub-agent. The agent:

- Gets a fresh conversation with no shared history
- Receives `.ageage/CONTEXT.md`
- Receives `SOUL.md` only when it is the last agent node and SOUL is enabled for the parent mode
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

An `auto` node may be first when all required arguments are explicit or the tool
accepts the raw `$vars.input` string. Use an `agent` first when natural language
must be converted into typed fields.

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
| `reason` | string | when failing | Human-readable explanation for the failed node attempt. |

```
node_complete({
  "status": "success",
  "vars": {
    "result": "The page is about X and covers Y."
  }
})
```

If an agent returns normally without calling `node_complete`, the pipeline uses its
last assistant text as `vars.result`. LLM errors and iteration-limit errors are
propagated to the node fallback/error path instead of being masked.

---

## Patterns

### Basic pipeline

```yaml
name: research-and-summarize
description: "Fetch a URL and summarize it."
vars:
  url: ""
returns: answer
pipeline:
  - id: parse_input
    prompt: |
      The user's message is expected to be a URL. Validate it and return it as
      vars.url. If it is not a URL, call node_complete with status="failure".
      Input: {{$vars.input}}
    outputs:
      url: url

  - id: fetch
    type: auto
    tool: web_fetch
    inputs:
      url: $vars.url
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
returns: summaries
pipeline:
  - id: prepare_urls
    prompt: |
      Extract all URLs from the user's message and return them as vars.urls.
      Input: {{$vars.input}}
    outputs:
      urls: urls

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

Set `foreach_concurrency` in `[pipeline]` config to run iterations simultaneously:

```toml
# ageage.toml
[pipeline]
foreach_concurrency = 4  # up to 4 simultaneous sub-agents per foreach node
```

```yaml
- id: analyze_articles
  foreach: $vars.article_urls
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
  - id: confirm_sections
    prompt: |
      Return the declared file-section list unchanged as vars.files:
      {{$vars.files}}
    outputs:
      files: files

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

### `ask_user` — pause for human input

Use the `ask_user` skill-only tool to pause the pipeline and request input from the user. Execution blocks until the user replies. If the user sends `/stop` or `/session abort`, the pending request is cancelled and the pipeline stops.

In channels that support interactive elements (e.g. Telegram), options are rendered as clickable buttons.

```yaml
name: content-approver
description: "Draft content, then ask the user for approval before publishing."
pipeline:
  - id: draft
    prompt: |
      Write a short announcement post about: {{$vars.input}}
    outputs:
      draft: result

  - id: review
    tools:
      - ask_user
    prompt: |
      The draft is ready:

      {{$vars.draft}}

      Ask the user whether to publish, revise, or discard it.
      Use ask_user with options: ["Publish", "Revise", "Discard"]
      Then call node_complete with:
      - status: "success"
      - vars.decision: the user's choice
    outputs:
      decision: decision

  - id: act
    prompt: |
      The user chose: {{$vars.decision}}
      Draft: {{$vars.draft}}

      Carry out the decision.
```

`ask_user` is a **skill-only tool** — it must be listed explicitly in the node's `tools` field.

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

1. If top-level `returns` is set, return that variable; missing output is an error.
2. Otherwise check `$vars.result`, then `$vars.output`, then `$vars.answer`.
3. If none exists, the pipeline returns an error asking the author to declare `returns`.

---

## Error handling

- If a node ultimately fails (tool error, `node_complete` with `status: "failure"`, or validation error), the pipeline stops.
- All remaining nodes are marked as **skipped** in the progress display.
- The error message is surfaced to the user.

### Model fallback

Agent nodes automatically retry once at the next lower model tier after a model
or execution error:

- `strong` retries with `medium`
- `medium` retries with the base model
- `base` has no lower-tier retry

Cancellation and deliberate `node_complete(status="failure")` decisions do not
retry. This avoids repeating side effects after a node has explicitly decided
the workflow cannot continue. There is currently no per-node fallback override.

Design pipelines so that each node is robust enough to succeed on its own, or use `validate: not_empty` to surface missing data early.

---

## Worked example: multi-source research report

```yaml
# skills/research-report.yaml
name: research-report
description: "Search multiple queries in parallel, then synthesize a report."
vars:
  queries: []
returns: report
pipeline:
  - id: prepare_queries
    prompt: |
      Turn the user's request into a JSON-compatible list of focused search
      queries and return it as vars.queries.
      Request: {{$vars.input}}
    outputs:
      queries: queries

  - id: search_all
    type: auto
    tool: web_search
    foreach: $vars.queries
    inputs:
      query: $foreach.current
    outputs:
      search_results: result

  - id: fetch_articles
    type: auto
    tool: web_fetch
    foreach: $vars.search_results
    inputs:
      url: $foreach.current
    outputs:
      articles: result

  - id: extract_facts
    foreach: $vars.articles
    prompt: |
      Extract the 3 most important facts from this article:

      {{$foreach.current}}

      Call node_complete with:
      - status: "success"
      - vars.result: a brief summary of the key facts (2–3 sentences)
    outputs:
      facts: result

  - id: write_report
    tier: strong # use the strongest model for synthesis
    prompt: |
      Write a comprehensive research report based on these findings:
      {{$vars.facts}}
      Structure it with an executive summary, key findings, and conclusion.
    outputs:
      report: result
```

---

## Configuration

Pipeline behaviour is controlled through the `[pipeline]` section of `ageage.toml`.

### `[pipeline]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `foreach_concurrency` | int | `0` | Max parallel iterations for `foreach` nodes. `0` or `1` = sequential. |

```toml
[pipeline]
foreach_concurrency = 4
```

### `[pipeline.models]`

Override which LLM model is used for each node tier. Takes precedence over the `[router]` model settings.

```toml
[pipeline.models.base]
model = "gpt-4o-mini"

[pipeline.models.medium]
model = "gpt-4o"

[pipeline.models.strong]
model = "o3"
api_key = "sk-..."  # optional: different provider
```

If a tier is not set here, the corresponding `[router]` model is used as a fallback (e.g. `[router.strong]` for `strong`). If neither is configured, the base `[llm]` model is used.

---

## Tips

- **Keep nodes small.** A node that does one thing is easier to debug than one that does five.
- **Name output keys clearly.** `search_results`, `article_content`, `final_summary` — not `data`, `out`, `result` everywhere.
- **Use `auto` for mechanical steps.** File reads, web fetches, and shell commands don't need an LLM.
- **Declare all variables in `vars`.** Even as empty strings. It documents the pipeline's data contract at a glance.
- **Declare `returns`.** It makes the user-visible output explicit and avoids relying on common variable names.
