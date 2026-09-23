# Current Stage: IM Control Plane, Session Integrity, and Agent-Owned Scheduling

## Objective

Improve the IM control plane, progress reporting, multi-session correctness, and scheduled-agent workflow while keeping the tool surface small enough for weaker models.

The implementation may support richer internal policies, but optional complexity must not appear in an agent's tool schema unless the corresponding feature is enabled.

## Product Decisions

- Matrix uses `!` for AgeAge commands. Telegram, Discord, and the interactive CLI continue to use `/`.
- IM progress is emitted as structured events. Configuration decides which events are visible and how a channel renders them.
- A scheduled run always uses a run-local Agent instance. It may optionally operate on the same logical chat session, but it must never concurrently reuse the same mutable Agent instance.
- Cron remains an Agent scheduling facility. The CLI is an administrative surface, not the ownership model.
- Advanced session integration for cron is disabled by default.
- The default Agent-facing cron creation schema stays minimal: schedule and task. Ownership, delivery, timeout, notification defaults, and the task session are supplied by runtime policy.

## General Constraints

- Documentation and user-facing configuration examples must be written in English.
- Preserve existing Telegram, Discord, CLI, HTTP, MCP, skill, and pipeline behavior unless a node explicitly changes it.
- Do not expose arbitrary channel IDs, session policy combinations, overlap policies, or capability matrices to the model by default.
- IM commands must use exact token matching; prefix collisions such as `!sessionist` must not trigger `!session`.
- Session mutation, history persistence, and scheduled writes must be serialized per logical session.
- Configuration migrations must preserve existing cron entries and session histories.
- Do not modify unrelated or pre-existing untracked files.
- Every node must include focused unit tests. Final validation must run formatting, vet, the full test suite, and a build. Run the race suite when a C compiler is available.

## Node A: Matrix Command Router

Owned areas: a new IM command parser, the IM command handling portions of `main.go`, Matrix-facing help text, and focused tests.

Tasks:

1. Add a channel-aware command parser with `!` for Matrix and `/` for Telegram and Discord.
2. Match command names as complete tokens and preserve arguments without lowercasing them.
3. Support `!!` as a Matrix literal escape.
4. Route built-in commands, fast-path stop/abort commands, and dynamic skill commands through the parser.
5. Treat recognized Matrix commands as messages directed at the bot in an authorized group room; unknown `!` text must not bypass mention rules.
6. Render help, usage, build-success, confirmation-cancellation, and session guidance with the active channel prefix.

Acceptance criteria:

- Matrix `!help`, `!stop`, `!session`, and `!skill-name` work.
- Matrix `/help` is not interpreted as an AgeAge command.
- Telegram and Discord retain `/` behavior.
- Prefix collisions and literal escapes are covered by tests.

## Node B: Session Registry and Serialization

Owned areas: `agent/session.go`, a new session registry, the IM session management portions of `main.go`, and tests.

Tasks:

1. Introduce structured session bindings for channel, room, thread, owner, session ID, and kind.
2. Add a per-session execution/persistence lock shared by chat runs, scheduled runs, history saves, rename, and removal.
3. Prevent two chat keys from concurrently attaching the same mutable Agent instance.
4. Persist active bindings and Matrix thread roots under `.ageage/sessions/` using private, atomic storage.
5. Restore bindings after restart and remove stale bindings safely.
6. Reject `new` when a session already exists, generate collision-free automatic names, and reject removal while any binding is active.
7. Keep cron sessions out of normal user session listings and operations.

Acceptance criteria:

- The same logical session cannot run concurrently through different room/thread handlers.
- Matrix thread bindings and links survive restart.
- Session history remains valid under concurrent save attempts.
- New, switch, rename, and remove operations have deterministic ownership and active-binding behavior.

## Node C: Configurable IM Progress Events

Owned areas: channel callback abstractions, notification configuration, todo/pipeline progress rendering, Matrix typing, and documentation.

Tasks:

1. Define stable progress event categories for lifecycle, plan/pipeline updates, waiting for input, selected tool activity, sub-agent activity, and cron outcomes.
2. Add quiet, balanced, and verbose presets plus explicit event include/exclude configuration.
3. Allow per-channel overrides without adding notification-policy decisions to the model prompt or tool schemas.
4. Preserve one editable progress message when the channel supports editing; throttle and deduplicate updates.
5. Keep progress in the originating thread.
6. Refresh Matrix typing during long tasks and stop it reliably on success, failure, cancellation, or panic.
7. Ensure progress-cleanup paths cannot leave stale reactions or typing loops.

Acceptance criteria:

- Configuration determines which progress events reach IM.
- Balanced mode avoids per-tool message spam.
- Matrix long-running tasks retain typing and update one progress message in place.
- Telegram and Discord behavior remains compatible.

## Node D: Agent-Owned Cron Service

Owned areas: `tools/cron.go`, `agent/cron_scheduler.go`, cron construction in `agent/factory.go`, cron delivery in `main.go`, configuration, documentation, and tests.

Tasks:

1. Add a service layer that applies ownership, scope, delivery, validation, execution, and audit policy consistently to Agent, CLI, and scheduler operations.
2. Bind Agent-created entries to the current principal and source session. Agent list, run, pause, resume, and remove operations may access only owned entries; CLI remains an administrative view.
3. Default Agent-created delivery to the current IM room/thread and reject arbitrary cross-room delivery.
4. Keep the default creation schema limited to `schedule` and `task`.
5. Add `[cron].session_integration`, default `false`. When disabled, omit session integration from the tool schema entirely. When enabled, expose only optional `continue_current_session: boolean`.
6. For `continue_current_session=true`, serialize through the session registry, use a run-local Agent, queue behind an active chat run, and write the scheduled turn back to the logical session.
7. Keep the current task-owned persistent cron session behavior when session integration is disabled or not requested.
8. Prevent scheduled Agents from mutating cron entries unless a future explicit policy enables it.
9. Give runs a finite default timeout and make manual and scheduled execution use the same audit path.
10. Store cron data with mode `0600` using same-directory temporary files and atomic replacement.

Acceptance criteria:

- Existing cron files migrate without losing entries.
- Two IM principals cannot list, run, pause, resume, or remove each other's tasks.
- Agent-created results default to the originating thread.
- The default tool schema contains no session-mode field.
- Enabling session integration adds only the boolean continuation field.
- Shared logical-session runs are serialized with chat runs and do not reuse a live Agent object.
- CLI and scheduled/manual Agent runs record consistent audit results.

## Acceptance Order

1. Node B establishes session identity and locking.
2. Node A moves Matrix commands onto the new control prefix.
3. Node C builds configurable progress on structured session/channel scope.
4. Node D consumes the session registry and notification policy.
5. Integration acceptance: migration tests, example configuration parsing, full suite, documentation, and security review.

## Status

- [x] Node A: Matrix command router
- [x] Node B: session registry and serialization
- [x] Node C: configurable IM progress events
- [x] Node D: Agent-owned cron service
- [x] Integration acceptance
