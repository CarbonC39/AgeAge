# Design: router tool filtering, embedded docs without disk writes, configurable turn compression

Date: 2026-06-16

## 1. Router-disabled tool filtering — no change needed

Verified by reading `agent/agent.go`: the entire router phase (agent.go:434) is gated by
`if a.router != nil`. When `config.Router.Enabled=false`, `a.router` is nil (factory.go:575),
so `routerResult` stays `nil` for the whole turn — including when an explicit `/skill-name`
command is used. `buildExecPlan` (agent.go:599) falls into its `default` branch whenever
`rr == nil`, which sets `neededTools = a.registry.List()` — every currently-registered tool,
unfiltered. An explicitly matched skill only *adds* its `RequiredTools` on top; it never narrows
the set.

Conclusion: router-disabled already yields a full, unfiltered ReAct loop over whatever is
registered. No code change required for this item.

## 2. Framework docs: serve from embedded FS, no disk writes

### Current behavior
`agent/factory.go:170` calls `agentdocs.ExtractTo(docsDir)` on every startup, writing 5 `.md`
files to `<workspace>/.ageage/docs/`. Three consumers:
- `agent/planner.go:201-202` reads two of them directly via `os.ReadFile` (Go-level, no tool).
- `agent/evaluator.go:133` adds `docsDir` to a sub-agent's `AllowedRoots`.
- The main agent's system prompt (agent.go:300, agent.go:897) tells the LLM to `file_read`
  files under `.ageage/docs/`.

### Why this is safe to virtualize
`docsDir` is always `<EffectiveWorkDir>/.ageage/docs`, and `security.Checker` is constructed
with `workspace = EffectiveWorkDir` (factory.go:188). `Checker.CheckPath` only requires the
*parent* directory to resolve via `EvalSymlinks`; it never requires the target file to exist.
A path under the workspace passes the workspace check regardless of whether anything is on
disk there. So no `AllowedRoots` entry and no real file are needed for `file_read` to succeed.

### Change
1. `internal/agentdocs/docs.go`: replace `ExtractTo` with:
   - `Read(name string) (string, bool)` — returns embedded file content by filename.
   - (keep the `embed.FS` as the single source of truth; no disk I/O in this package.)
2. `agent/factory.go`: delete the `agentdocs.ExtractTo(docsDir)` call and its error handling.
   `docsDir` is still computed (same path, same string) — it's now a purely virtual path used
   for prompt text and tool wiring, never written to.
3. `tools/file.go` (`FileReadTool`): add a `DocsDir string` field. In `Execute`, after resolving
   the path:
   - If `os.Stat` finds a real file at the resolved path, read it normally (preserves the
     ability for a user to drop a real file at `.ageage/docs/<name>.md` to override a doc).
   - Else, if the resolved path's parent equals `filepath.Clean(DocsDir)` and
     `agentdocs.Read(basename)` succeeds, return that content directly (no disk access).
   - Else, fall through to the existing not-found error path.
4. `agent/factory.go`: set `DocsDir: docsDir` wherever `FileReadTool` is constructed (the main
   registration call; sub-agents share the same factory-level `docsDir` value).
5. `agent/planner.go`: change `readDoc(filepath.Join(p.docsDir, "planner-guide.md"))` and the
   `pipeline.md` read to call `agentdocs.Read("planner-guide.md")` / `agentdocs.Read("pipeline.md")`
   directly, since this is a Go-level read, not a tool call.
6. `agent/evaluator.go`: leave the existing `AllowedRoots: []string{e.docsDir}` as-is (harmless,
   already redundant given workspace coverage — not in scope to remove).
7. No change to system prompt text (agent.go:300, agent.go:897) — the LLM still calls
   `file_read` on `.ageage/docs/<name>.md`; the virtualization is invisible to it.

### Known side effect (accepted)
If a future skill or prompt tries to `grep`/`glob` over `.ageage/docs/`, it will find nothing,
since no real files exist there unless a user has placed one manually. Only `file_read` on the
five known filenames is supported by this design.

## 3. Configurable tool-turn-to-narrative compression

### Current behavior
`agent/agent.go` tracks `pendingTurns` (turnRecord per LLM↔tool exchange). After each turn,
`runLoop` (agent.go:922-925) hardcodes:
```go
const keepRecentTurns = 2
for len(a.pendingTurns) > keepRecentTurns {
    a.compressOldestTurn()
}
```
`compressOldestTurn` (agent.go:1086) replaces the oldest turn's raw messages (assistant message
+ tool calls + tool results) with a single narrative assistant message like
`"1. Ran: ls -la\n   → ..."` (via `buildTurnNarrative`/`briefActionSummary`). This is separate
from and runs much more aggressively than the LLM-based `Summarizer` (`[summarize]` config,
threshold-gated).

### Change
1. `config/config.go`: add
   ```go
   type HistoryConfig struct {
       CompressToolTurns bool `toml:"compress_tool_turns"` // default true
       KeepRecentTurns   int  `toml:"keep_recent_turns"`   // default 2
   }
   ```
   and a `History HistoryConfig `toml:"history"`` field on the top-level `Config` struct.
   Set defaults (`CompressToolTurns: true, KeepRecentTurns: 2`) in the same place other
   defaults are set (near `Router: RouterConfig{...}`), so existing configs without a
   `[history]` section behave exactly as today.
2. `agent/agent.go` (`runLoop`):
   - Only append to `a.pendingTurns` when `a.cfg.History.CompressToolTurns` is true. When
     disabled, `pendingTurns` stays empty for the whole run — tool turns remain in `a.conv`
     in raw JSON form indefinitely (until the existing `Summarizer` threshold kicks in).
   - When enabled, replace the hardcoded `2` with `a.cfg.History.KeepRecentTurns`, falling
     back to `2` if the configured value is `<= 0`.
3. No change to `trySummarize`, `RollbackLastTurn`, or `ClearHistory` — all already tolerate
   `pendingTurns` being empty (verified by reading agent.go:1283-1356).

## Out of scope
- Item 1 required no code change; verified only.
- No changes to the `Summarizer`/`[summarize]` LLM-based compression mechanism.
- No changes to `evaluator.go`'s `AllowedRoots` wiring.
