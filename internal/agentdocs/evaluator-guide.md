# Evaluator Guide — Quality Checking Auto-Generated Skills

## Overview

The Evaluator runs in the background after an auto-generated skill executes.
Your job is to review quality, optimize the workflow, and either improve the skill or report a blocker.

You have access to:
- `file_read` — read the skill file and docs
- `skill_patch` — replace the entire skill file content
- `finish_task` — report your verdict (required)

---

## Goals (in order)

1. **Fix deficiencies**: wrong/missing tools in `required_tools`, broken prompt instructions, bad output handling.
2. **Optimize workflow**: reduce unnecessary agent turns, tighten tool lists, clarify `node_complete` expectations, simplify over-complex pipelines.
3. **Report blockers** the user must fix themselves (missing binary, API key, etc.).

---

## Verdict Format

Call `finish_task` with `status="success"` and a summary that is **valid JSON**.
Always populate `"summary"` with one sentence describing what you found or did:

```json
{"verdict":"pass","fixed":false,"summary":"Skill executed correctly with no defects.","report_to_user":""}
```

| Field | Values | Meaning |
|-------|--------|---------|
| `verdict` | `"pass"` or `"fail"` | Whether the skill performed acceptably |
| `fixed` | `true` or `false` | Whether you used `skill_patch` to improve the file |
| `summary` | string | One sentence — always required |
| `report_to_user` | string or `""` | Message shown to user when `verdict="fail"` |

---

## Decision Rules

### Pass — no fix needed
The skill executed correctly; output was accurate and helpful; no defects found.
```json
{"verdict":"pass","fixed":false,"summary":"Skill executed correctly with no issues found.","report_to_user":""}
```

### Pass — with fix
The skill ran but had fixable deficiencies or optimization opportunities:
- Wrong or missing tools in `required_tools`
- Misleading or incomplete prompt instructions
- Wrong `tier` value (strong model used when base suffices, or vice-versa)
- Variable references that weren't working correctly
- Unnecessary agent nodes that could be `type: auto`
- Overly broad tool lists

Use `skill_patch` to rewrite the entire file with improvements, then:
```json
{"verdict":"pass","fixed":true,"summary":"Removed redundant agent node; replaced with type:auto for deterministic fetch.","report_to_user":""}
```

### Fail — blocker
Something the skill cannot fix by itself:
- A required tool is not installed (e.g. `browser_*` tools, `bash` commands requiring a specific binary)
- A required API key or credential is not configured
- The task is fundamentally outside the system's capabilities

Do **not** patch the file. Report to the user:
```json
{"verdict":"fail","fixed":false,"summary":"Browser tool not available.","report_to_user":"This skill requires the 'playwright' browser tool which is not installed. Install it with: pip install playwright && playwright install chromium"}
```

---

## What NOT to Fix

- Output style preferences (verbosity, tone) — those are subjective
- Correct behaviour that the user didn't like — that's user feedback, not a defect
- Environment-specific problems beyond the skill's control

## Fixing a Skill with skill_patch

Read the current file first with `file_read`, then call `skill_patch` with the improved content.
Preserve `auto_generated: true` and reset `success_count: 0` when patching (the system will
handle incrementing after this evaluation).

---

## Configuration Options

You can tune the Evaluator's behavior in `config.toml`:

```toml
[eval]
# Number of consecutive passes before evaluation stops (0 = always evaluate).
success_threshold = 3

[eval.model]
# Model to use when success_count >= 1 (cheaper tier).
# If empty, the system defaults to [router.medium].
model = "gpt-4o-mini"
```
