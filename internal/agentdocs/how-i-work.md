# How AgeAge Works

## Execution Loop

Each user message starts an iteration:

1. System prompt + full conversation history → LLM call
2. LLM responds with text and/or tool calls
3. Framework executes tool calls → results appended to history
4. Loop repeats until `finish_task` is called or `max_iterations` is reached

**You must call `finish_task` to end a task.** Returning a text response without calling it is
not treated as a final answer — the loop continues to the next iteration.

## Message History

- Saved to `.ageage/sessions/{id}/history.jsonl` after every `Run()` call
- **System message is never saved** — it is rebuilt fresh at the start of each turn
- Tool messages (role=tool) and assistant messages are saved verbatim
- When message count exceeds the summarize threshold, old turns are compressed and
  detail is lost — use CONTEXT.md to preserve critical facts across turns

## System Prompt Composition (top to bottom, stable prefix for KV-cache)

1. Core rules (finish_task instruction, language rule, quality requirements)
2. `AGENT.md` behavioral directives (workspace/data/AGENT.md)
3. `SOUL.md` personality — channel/serve mode only (workspace/data/SOUL.md)
4. CONTEXT.md contents of the active session (injected if non-empty)
5. Credential hint (if credentials are configured)
6. Framework documentation hint (this section)
7. Active skill instructions (dynamic — changes per skill trigger)

Sections 1–6 are identical across turns → KV-cache hits.
Edits to AGENT.md or CONTEXT.md take effect from the **next** LLM call.

## Session CONTEXT.md

Each session has its own `CONTEXT.md` at `.ageage/sessions/{id}/CONTEXT.md`.
- Always injected into system prompt when non-empty
- `file_edit` targeting any session CONTEXT.md is **auto-approved** (no supervision prompt)
- Keep under 2 000 characters; store decisions, key paths, constraints — not logs

## Tool Execution

- Multiple tool calls in one response run **in parallel** if `max_parallel_tools > 1`
- Any mutation tool (file_write, bash, etc.) in the batch forces the entire batch to
  serialize — prevents concurrent-write races
- MCP and unknown custom tools are conservatively treated as mutations
- Tool output is capped at `max_output_bytes` (default 4 MB) then further trimmed to
  8 000 visible runes before being stored in history

## Credentials

Write `{{cred:name}}` in any tool argument. The framework substitutes the real value
before execution and scrubs it from the result. Values never appear in conversation
history. Use `/cred list` to see available credential names.

## Skills

Markdown files in `{workspace}/skills/`. Triggered by `/skill-name` prefix or auto-
selected by the router. Hot-reloaded every 2 s — no restart needed.
Call `framework_doc("skills")` for format reference.

## Parallel Tool Dispatch Note

When returning multiple tool calls that are independent (e.g., several file_read calls),
group them in a single response — the framework runs them in parallel, saving latency.
Sequential chained calls should remain in separate responses.
