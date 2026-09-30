# Current Stage: Metadata-Driven Interactive Administration

## Objective

Replace the legacy numbered-input tool selector with lightweight keyboard-driven forms, add a safe memory administration command, and consolidate configuration editing under `ageage config`.

The CLI must not maintain duplicate lists of tools, backends, notification values, memory fields, or other domain options. The package that owns a capability must expose the metadata or typed options consumed by the CLI.

## Product Decisions

- Use lightweight interactive forms, not a persistent full-screen application.
- Interactive forms must support arrow-key navigation, Space for multi-select, Enter to submit, and Escape to cancel or return without writing.
- Use `github.com/charmbracelet/huh` on the existing Bubble Tea v1 and Lipgloss v1 dependency line. Do not introduce a second Charm v2 rendering stack.
- `ageage config` is the configuration entry point. Keep `ageage tools` as a compatibility alias for the tool form.
- Keep `ageage init` as the workspace bootstrap command. Do not add a redundant `ageage config init` command.
- Keep credentials, cron tasks, skills, and memories outside the configuration command because they are separate state domains.
- Add `ageage memory` as the interactive and scriptable administration surface for `MEMORY.jsonl`.
- Memory export is an exact JSONL backup. Do not add lossy CSV or Markdown export formats in this stage.
- Non-interactive subcommands must remain usable in pipes and scripts and must never start a terminal form.

## General Constraints

- Documentation, task descriptions, command help, and configuration examples must be written in English.
- The CLI may render metadata but must not own capability metadata.
- Do not use reflection to generate a universal configuration editor.
- Do not introduce a general plugin framework, schema language, or form DSL.
- Preserve comments, unknown TOML sections, unknown keys, configured MCP tool names, and forward-compatible JSON fields.
- All cancellations and validation failures must leave files unchanged.
- Configuration and memory writes must be private where appropriate, atomic, and safe against concurrent Agent processes.
- Do not initialize LLM clients, launch browsers, connect MCP servers, or perform other runtime side effects merely to render a form.
- Do not modify unrelated or pre-existing untracked files.

## Node A: Owned Metadata and Option Providers

Owned areas: `tools/registry.go`, `tools/metadata.go`, Agent tool registration and skill-only factories, configuration constants, and focused tests.

### A1. Runtime tool descriptors

Reuse the existing sources of truth:

- `Tool.Name()` for the stable tool name.
- `Tool.Description()` for user-facing capability text.
- `Registry.Metadata()` and `ToolMetadata` for risk and execution characteristics.
- Registry insertion order for deterministic display.

Add a small descriptor value and ordered registry accessor, equivalent in scope to:

```go
type ToolDescriptor struct {
    Name        string
    Description string
    Metadata    ToolMetadata
}

func (r *Registry) Descriptors() []ToolDescriptor
```

`Descriptors` must use the registered tool object and existing metadata resolution. It must not contain a switch over tool names.

### A2. Agent tool catalog

The interactive allowlist must also show tools that are currently disabled, so a runtime registry alone is insufficient. Replace the duplicated tool-name lists with one ordered Agent-owned registration catalog.

The catalog must describe only lifecycle information owned by the Agent layer:

```go
type ToolAvailability string

const (
    ToolDefault   ToolAvailability = "default"
    ToolSkillOnly ToolAvailability = "skill_only"
    ToolInternal  ToolAvailability = "internal"
    ToolExternal  ToolAvailability = "external"
)

type ToolOption struct {
    Descriptor   tools.ToolDescriptor
    Availability ToolAvailability
    Configurable bool
}
```

The exact names may differ, but the responsibilities must remain narrow:

1. Standard and Agent-owned tool registration must derive from the ordered catalog rather than a second list in the CLI. Each catalog entry should hold a side-effect-free prototype/descriptor constructor and the runtime constructor; it must not copy the tool name or description into Agent or CLI string fields. The prototype supplies `Name`, `Description`, and `Metadata`, while the runtime constructor supplies dependencies.
2. `skillOnlyToolFactories` must expose its availability and configurability alongside its constructor. Internal helpers such as `next_step` must not appear as user-selectable tools.
3. `AgentFactory.GetStandardToolNames`, skill validation, planner tool discovery, and the interactive selector must derive from the same catalog.
4. Actual registered tools and catalog prototypes must use the same concrete tool implementation for name, description, and execution metadata. Creating a prototype must not open files, connect services, start a browser, or require a usable Agent run.
5. Dynamic MCP descriptors may come from a connected runtime registry, but rendering the configuration form must not connect to MCP servers. Existing configured names that are not in the static catalog must be preserved and shown as unavailable/custom entries rather than discarded.
6. Catalog ordering must be deterministic. Do not expose a Go map's iteration order to the UI or model prompts.

Remove the CLI-owned `knownTools` table after all consumers use the provider.

### A3. Configuration choices

Move valid enum-like values into the owning configuration package as stable constants and copy-returning option providers. Add only a small shared display type, for example:

```go
type Choice struct {
    Value       string
    Label       string
    Description string
}
```

Providers are required for values presented by the new forms, including:

- Agent modes.
- Notification presets and progress categories.
- Supported channel types used by notification overrides.
- Web-search backends.
- Web-fetch backends.
- Browser backends and browser types if exposed by the form.
- LLM provider presets, endpoint examples, and model suggestions used by setup.

Runtime switches and defaults must use the same constants. The CLI must consume the provider output and must not reproduce these values in `main.go`.

Do not create a reflection-based field schema. Backend-specific advanced fields remain available through `ageage config edit` unless a typed owner-provided option is explicitly added later.

When a form needs backend-specific follow-up fields, expose that information with a narrow typed provider owned by that config domain. For example, a web-search backend option may state that it needs a SearXNG endpoint or supports a named credential field. Do not encode those relationships in CLI switches, and do not turn them into a universal schema language.

### A4. Information ownership boundary

Module-owned data includes:

- Capability names, tool descriptions, availability class, configurability, and execution risk.
- Valid enum values, defaults, labels, descriptions, and backend-specific requirements.
- Notification category names and preset semantics.
- Memory record fields, search behavior, persistence semantics, and export format.
- Validation rules and normalization behavior.

The UI may own:

- Page titles such as `Model & Agent` and action labels such as `Save`, `Discard`, `Back`, and `Exit`.
- Section ordering, spacing, colors, icons, key-help text, and review-screen layout.
- Concise contextual sentences that explain the current form without redefining a capability or valid value.

CLI files must not contain duplicate capability lists, enum slices, copied tool descriptions, backend matrices, notification category lists, or memory-format knowledge. Layout strings are not domain metadata.

### Acceptance criteria

- Adding a configurable built-in or skill-only tool in its owning registration catalog makes it appear in the tool form without editing CLI files.
- Tool descriptions and risk labels come from the actual tool and `ToolMetadata`.
- Internal-only tools do not appear in the allowlist.
- Existing configured unknown or MCP tool names survive a no-op edit.
- All displayed enum choices come from `config` providers.
- Catalog/provider tests fail if registration, validation, planner discovery, and configurable tool enumeration drift apart.

## Node B: Lightweight Forms and `ageage config`

Owned areas: Cobra command construction, a small form adapter, targeted TOML editing, documentation, and tests.

### B1. Form adapter

Add a small package or file that configures Huh consistently:

- Arrow keys move through options.
- Space toggles multi-select values.
- Enter submits the current form.
- Escape and Ctrl+C return a typed cancellation result.
- Cancellation is not printed as an operational error and never writes state.

The adapter may provide helpers for select, multi-select, input, text, and confirmation. It must not become a second application framework or contain domain option lists.

Avoid Huh's inline filter mode in this stage so Escape always has one meaning. Search flows should use a separate input form.

### B2. Command surface and sections

Implement:

```text
ageage config
ageage config tools
ageage config edit
ageage config validate
```

- `ageage config` opens an owner-metadata-driven configuration editor with these sections:
  - Model & Agent.
  - Router, Planner & Evaluator.
  - Web.
  - Tools.
  - Notifications.
  - Basic Safety.
  - Open raw config.
  - Validate.
- `ageage config tools` opens the tool multi-select directly.
- `ageage config edit` opens the resolved file with `$VISUAL`, then `$EDITOR`, then a documented platform-appropriate fallback. It must pass the path as one argument without invoking a shell.
- `ageage config validate` parses the resolved file, reports its path, and exits nonzero on failure.
- `ageage tools` calls the same implementation as `ageage config tools` and remains documented as a compatibility alias for this stage.
- Do not add `ageage config path`; validation and editing already report the resolved path.

Interactive commands must reject or use a documented plain fallback when stdin/stdout are not terminals. Scriptable subcommands must not depend on terminal capabilities.

The editable scope is intentionally finite:

- Model & Agent: API key, base URL, model, temperature, max tokens, agent mode, max iterations, and parallel-tool limit. Secret values must be masked in forms and reviews.
- Router, Planner & Evaluator: enabled states and the existing model/threshold fields needed by those components.
- Web: search and fetch backend choices plus owner-declared required follow-up fields. Browser choices may be included only when their typed provider and validation are ready.
- Tools: default-versus-custom allowlist selection.
- Notifications: preset, include/exclude categories, throttle, and existing channel overrides without inventing new policy semantics.
- Basic Safety: `forbid_rm` and other explicitly selected low-complexity safety toggles. Complex command/path policy remains in the raw editor for this stage.

The form must initialize from the loaded config rather than guessed defaults. Open raw config is an explicit escape hatch outside the staged editor. If the draft is dirty, require Save, Discard, or Back before launching the external editor. On return, reload and validate the file before continuing.

### B3. Tool allowlist semantics

The form must distinguish these states explicitly:

- Default tool set: an empty `agent.tools` value, with skill-only tools injected only when requested by a skill.
- Custom allowlist: an explicit non-empty list, which may intentionally promote skill-only tools to global availability.

Do not offer a fake `none` state. Under current configuration semantics, an empty list means defaults rather than no tools. Reject an empty custom allowlist unless that configuration contract is changed in a separately reviewed design.

Display default, skill-only, custom/unavailable, and excluded-by-policy status using `ToolOption` data and current configuration. Preserve configured unknown names unless the user explicitly deselects them.

### B4. Staged review and one final decision

All section edits must update an in-memory draft. Navigating between sections must not write `config.toml`.

Before exit, show one review screen containing only changed section/key values. Mask API keys, tokens, and other secret-looking values. The user then chooses exactly one of:

- Save all staged changes.
- Discard all staged changes.
- Back to editing.

Escape or Ctrl+C discards the draft after a clear confirmation when changes exist. An unchanged session exits without a confirmation prompt. Validation must run against the complete draft before Save is offered.

`ageage config tools` and the `ageage tools` alias use the same draft/review/save path even though they enter directly into one section.

### B5. Safe batched targeted TOML updates

Replace direct `os.WriteFile` editing in `updateConfigTools` with a narrow batched editor for known section/key assignments. It may accept values equivalent to `section`, `key`, and an already typed/encoded TOML value, but it must not become a reflected config serializer or schema DSL.

The batched update must:

- Preserve comments, ordering, unknown sections, and unknown keys.
- Applies all staged known-key changes in one replacement operation.
- Supports only the explicitly approved keys exposed by the forms.
- Inserts a missing known key into its owning section and creates that known section when absent.
- Preserve inline comments where practical and never rewrite unrelated lines.
- Uses a same-directory temporary file, sync, close, and atomic rename.
- Preserve the original file mode.
- Detects an external modification between read and save and reports a conflict instead of overwriting it.

Do not marshal the entire typed config back to TOML.

### Acceptance criteria

- A user can complete tool selection using arrows, Space, Enter, and Escape.
- Escape at every stage leaves `config.toml` byte-for-byte unchanged.
- Editing multiple sections produces one masked review and one atomic Save or Discard decision.
- Save validates the complete draft and applies all keys as one conflict-checked batch.
- Default and custom modes round-trip correctly.
- Skill-only and unavailable/custom tools are labeled from metadata rather than CLI name checks.
- Comments, unknown fields, MCP names, and file mode survive updates.
- Legacy `ageage tools` behavior remains available through the shared implementation.

## Node C: Modernized `ageage init`

Owned areas: `runInit`, setup-specific draft state, Huh forms, generated-file review, plain-mode compatibility, documentation, and tests.

### C1. Interactive setup flow

Keep `ageage init` as a top-level workspace bootstrap command. Do not merge it into `ageage config`.

Replace the current numbered TTY wizard with lightweight Huh forms that reuse:

- The shared form adapter and key behavior from Node B.
- Agent modes and backend choices from the owner-provided config options in Node A.
- The same metadata-driven tool selector used by `ageage config tools`.
- Existing config defaults and existing initialization builders rather than a second set of defaults in UI code.

The setup pages may retain the current logical groups: storage, LLM provider/model, Agent behavior, router, planner/evaluator, web, tools, and advanced settings.

### C2. Navigation, review, and writes

- Users can move back to earlier pages without losing draft values.
- Escape or Ctrl+C cancels setup and performs no filesystem writes.
- No directory, config, AGENT.md, SOUL.md, memory, skill, or other file may be created before the final confirmation.
- Show a final review of paths and changed/non-default settings before confirmation.
- Mask the API key and any other secret values everywhere after entry, including the review.
- If the target `config.toml` already exists, the default action is refusal. Overwrite requires an explicit affirmative selection at the final stage.
- After confirmation, create required directories and files in a bounded commit phase. Report partial failures precisely; do not claim setup completed when a required write failed.

### C3. Model discovery and fallback

- Preserve model fetching from the configured provider endpoint.
- Fetch only after the user requests or reaches the model-selection step; show a cancellable progress state.
- A fetch error, timeout, empty result, or unsupported response must fall back to manual model input without discarding other draft values.
- Model suggestions, provider examples, and choices must come from owner-provided provider metadata rather than a second URL switch in the form.

### C4. Non-TTY and plain compatibility

- Provide an explicit `--plain` path for accessible or automation-oriented setup.
- Automatically use the plain path when terminal capabilities required by Huh are unavailable.
- Preserve the current prompt/default behavior closely enough for existing piped setup workflows; document any unavoidable input-order change.
- Plain mode must consume the same defaults, validation, option providers, draft, review, and final commit functions as TTY mode.
- Plain mode must not silently accept overwrite of an existing config.

### Acceptance criteria

- TTY setup supports arrows, Space, Enter, back navigation, and Escape cancellation.
- Cancellation at every page leaves the target directory unchanged.
- The API key never appears unmasked after its input field.
- Model discovery failure cleanly falls back to manual entry.
- Existing config overwrite defaults to No and requires explicit confirmation.
- TTY and plain setup generate equivalent valid configuration for equivalent answers.
- Tool and backend options stay current through the same providers used by `ageage config`.

## Node D: Shared Memory Repository

Owned areas: `tools/memory.go`, a shared memory repository, locking, migration-compatible parsing, and tests.

### D1. Repository API

Move JSONL access behind one repository used by both the Agent tools and the CLI. It should expose focused domain operations rather than raw file mutation:

- List valid entries and report malformed records.
- Search using the existing case-insensitive OR keyword matching across content and tags.
- Add an entry using the existing ID and RFC3339 timestamp rules.
- Edit content and tags while preserving ID and timestamp.
- Remove one or more IDs.
- Produce an exact JSONL snapshot for export.

The existing `MemoryEntry` type remains the record metadata exposed to the UI. The CLI must not parse JSONL independently or duplicate the matching algorithm.

### D2. Compatibility and corruption resistance

- Preserve the existing JSONL format and Agent tool schemas.
- Preserve malformed lines byte-for-byte during edits and removals while reporting a warning to administrative callers.
- Preserve unknown JSON object fields when editing a known entry.
- Reject an ambiguous edit when duplicate valid IDs exist.
- Keep Agent-facing recall limits and output formatting unchanged; repository listing itself may return the complete set for administration.
- Keep the backing file and exported backups at mode `0600`.

### D3. Concurrent processes

The existing process-local mutex is not enough because `ageage memory` may run while `connect` or `serve` is active.

Add a cross-process lock that is honored by every repository read/write path and is portable across supported platforms. Keep the process-local lock if it is still useful for ordering goroutines. Do not hold a file lock while waiting for user input.

Mutation flow must be:

1. Read a snapshot for the form.
2. Release locks while the user edits or confirms.
3. Reacquire the lock and reload the latest file.
4. Detect a conflicting edit to the selected entry.
5. Apply the mutation to the latest file and atomically replace it.

This must prevent an Agent append from being lost during a CLI edit or deletion.

### Acceptance criteria

- Existing memory tool tests keep their behavior.
- Agent tools and CLI operations share the same repository implementation.
- Concurrent append, recall, edit, and delete operations leave valid JSONL and do not lose unrelated entries.
- Malformed lines and unknown fields survive mutations.
- Edit preserves ID and timestamp and reports conflicts.
- Cross-process contention has a bounded, cancellable failure path.

## Node E: `ageage memory` Administration

Owned areas: Cobra memory commands, Huh forms, output formatting, documentation, and tests.

### E1. Command surface

Implement:

```text
ageage memory
ageage memory list
ageage memory search <query>
ageage memory add
ageage memory edit <id>
ageage memory remove <id>...    # alias: rm
ageage memory export <path>
```

- The root command loops through a lightweight action selector: browse, search, add, edit, delete, export, and exit.
- Browse and search display complete IDs, timestamps, tags, and content. They may render newest entries first without changing storage order.
- Search input and results use the repository search semantics.
- Add uses the same content, tags, ID, and timestamp semantics as `memory_store`.
- Edit changes only content and tags.
- Delete uses a metadata-backed multi-select, then an explicit confirmation. Space selects entries; Enter advances; Escape cancels.
- Export writes an exact JSONL snapshot, refuses to overwrite by default, and uses mode `0600`.
- Missing files are an empty memory store for list/search. Add may create the data directory safely.

The UI may choose labels and layout, but record values, search behavior, persistence rules, and export format must come from the repository rather than duplicated file knowledge.

### E2. Non-interactive behavior

- `list`, `search`, and `export` must work without a TTY.
- `edit` and `add` must fail with clear guidance when required values cannot be collected interactively; do not fall back to an ancient numbered prompt.
- `remove` must require interactive confirmation unless an explicit script-oriented confirmation flag is supplied.
- Output intended for scripts must be stable enough to include the record ID; do not expose secret memory content in unrelated commands or debug logs.

### Acceptance criteria

- Browse, search, edit, and delete can be completed with the required keyboard controls.
- Escape from any form performs no mutation.
- Bulk deletion cannot occur without a confirmation step.
- Non-interactive list/search/export never starts a form.
- Export is exact, private, and refuses accidental overwrite.
- Administrative warnings expose malformed record locations without printing unrelated memory content.

## Node F: Integration, Documentation, and Review

Tasks:

1. Update root help, README examples, and configuration documentation for `init`, `config`, and `memory`.
2. Document default-versus-custom tool allowlist semantics and the `ageage tools` compatibility alias.
3. Document memory concurrency, exact JSONL export, overwrite behavior, and file permissions.
4. Add command tests with injected stdin/stdout and terminal detection rather than mutating global process state where avoidable.
5. Add provider drift tests for tools and configuration choices.
6. Run formatting, focused tests, the full test suite, vet, build, and diff checks. Run the race suite when a C compiler is available.

Integration acceptance:

- No interactive option list for tools, backends, notification values, or memory records is hard-coded in CLI files.
- Rendering configuration forms has no network, MCP, browser, LLM, or scheduler side effects.
- Init and config use the same owner-provided choices and defaults.
- Existing configs and memory files round-trip without data loss.
- Cancellation paths are write-free.
- Help and documentation describe the actual command surface.

## Implementation Order

1. Node A establishes metadata and option ownership.
2. Node B builds the shared form adapter, staged config draft, and safe batch writer on Node A.
3. Node C modernizes init on the Node A providers and Node B form/draft primitives.
4. Node D establishes the safe shared memory repository.
5. Node E builds memory administration on Node D and the form adapter from Node B.
6. Node F completes compatibility, documentation, and integration acceptance.

## Status

- [x] Node A: owned metadata and option providers
- [x] Node B: lightweight forms and `ageage config`
- [x] Node C: modernized `ageage init`
- [x] Node D: shared memory repository
- [x] Node E: `ageage memory` administration
- [x] Node F: integration, documentation, and review
