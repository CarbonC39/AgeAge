# Tool Failure Diagnosis

Read the exact error message first — it almost always names the cause.

---

## "blocked" / "not allowed" / "path not permitted"

**Cause:** Security checker rejected the path or command.
- File paths must be within the effective working directory or `allowed_roots`
  - CLI mode: the directory where `ageage cli` was launched
  - Serve/connect mode: the `workspace` value in config
- The credentials file is permanently blocked for all file tools
- Certain command strings match `blocked_commands` (e.g. `rm -rf /`)

**Fix:** Use a path inside the working directory. Rewrite the command to avoid
the blocked pattern. Ask the user to add the path to `allowed_roots` in config
if a different root is legitimately needed.

---

## "CONFIRMATION_REQUIRED" / operation appears to hang

**Cause:** Supervised mode (`agent.mode = "supervised"`) requires user approval.

Auto-approved without prompt:
- `file_edit` / `file_write` targeting any session `CONTEXT.md`
- Commands whose prefix is in `bash.auto_allow_commands` config list

Everything else waits for the user to reply `y`, `n`, or `a` (always allow).

**Fix:** Wait for user response. Or ask the user to set `mode = "full"` in config
for autonomous operation (required for channel mode).

---

## Tool call not executed / "unknown tool"

**Cause:** Tool not registered in this agent instance. Possible reasons:
- `agent.non_include_tools` config excludes it
- `agent.tools` allowlist is set and doesn't include it
- Skill's `tools` frontmatter doesn't include it
- MCP server disconnected at startup

**Fix:** Check the active tool list. If using a skill, add the tool to its `tools:`
frontmatter. If using a global allowlist, add it to `agent.tools` in config.

---

## Output ends with "… (truncated)"

**Cause:** Output exceeded `bash.max_output_bytes` (default 4 MB) or the
8 000-rune display cap applied before storing in history.

**Fix:** Narrow the operation:
```
grep "pattern" file          # instead of cat file
head -n 100 file             # instead of reading the whole file
go test ./pkg/... -run Test  # target a specific package or test
```

---

## "command timed out after 30s"

Default bash timeout is 30 s.

**Fix:** Split into smaller steps. For long-running processes, background them
and poll: `long-cmd & echo $!` — then check with `kill -0 <pid>`.

---

## Command not found / environment variable missing in bash

**Cause:** The subprocess runs in an isolated environment. Only a safe allowlist of
prefixes is forwarded: PATH, HOME, GOPATH, GOROOT, JAVA_HOME, NODE_PATH, etc.
API keys and most custom vars are stripped.

**Fix:** Pass values explicitly in the command:
```bash
MY_KEY=value my-tool --arg
```
For persistent access, ask the user to add the var name or prefix to
`bash.passthrough_env_vars` in config.

---

## web_search / web_fetch fails

- Default backend is `duckduckgo` — no key required
- `tavily` and `brave` backends require API keys in config or env vars
  (`TAVILY_API_KEY` / `BRAVE_API_KEY`)
- Large pages are truncated; switch to `jina` backend for cleaner extraction:
  set `web_fetch.backend = "jina"` in config

---

## finish_task not ending the loop

`finish_task` must be called as a **tool call**, not mentioned in text.
The loop ends only when the tool result is received. If the loop keeps continuing,
verify the tool call was actually dispatched (check debug output with `--debug`).

---

## Self-Repair Strategy

1. Read the exact error → match a category above
2. Try the simplest fix once
3. If still blocked after 2–3 attempts, explain the blocker clearly to the user
   and ask for guidance rather than retrying indefinitely
