# Skills Reference

## What Skills Are

Markdown files in `{ageage-dir}/skills/` (same directory as config.toml).
Each defines a specialized agent mode with its own prompt, tool restrictions, and
model tier. Triggered by `/skill-name` prefix or auto-selected by the router.
Hot-reloaded every 2 seconds — no restart needed after saving.

---

## File Format

```
---
name: skill-name
description: One-line description (shown in /skills list and used for routing)
tier: medium
tools: [bash, file_read, file_write]
prompt: |
  System instructions for this skill.
  Use {{input}} to reference the user's message text.
---
```

### Frontmatter Fields

| Field | Required | Values | Effect |
|-------|----------|--------|--------|
| name | yes | slug | command name and routing key |
| description | yes | string | shown in `/skills`, used by router |
| tier | no | base / medium / strong | model selection when router enabled |
| tools | no | list of tool names | restricts available tools for this skill |
| prompt | no | string | prepended to system prompt when skill is active |
| pipeline | no | true | marks this as a pipeline skill (see pipeline doc) |

The skill's `tools` list restricts on top of `agent.tools` config — the intersection is used.
Body content after the frontmatter closing `---` is ignored for regular skills.

---

## Creating or Modifying a Skill

Use `file_write` to create `{ageage-dir}/skills/my-skill.md`:

```markdown
---
name: code-review
description: Review code for correctness, style, and security issues
tier: strong
tools: [file_read, glob, grep, bash]
prompt: |
  You are a senior code reviewer. Focus on:
  - Correctness and edge cases
  - Security vulnerabilities
  - Idiomatic style for the detected language
  Always provide specific line references and actionable suggestions.
---
```

Trigger with `/code-review <context>` — the skill activates immediately (hot-reload).

---

## Model Tier Selection

| tier     | model used                              | delegate tool |
|----------|-----------------------------------------|---------------|
| `base`   | base `[llm]` model                      | no            |
| `medium` | `[router.medium]` model (if configured) | no            |
| `strong` | `[router.strong]` model (if configured) | yes           |

Omitting `tier` lets the router decide per turn based on its checklist.
Legacy values `direct`/`atomic`/`workflow` are still accepted.

---

## Pipeline Skills

Add `pipeline: true` to frontmatter, then define nodes in YAML after the second `---`.
Read `.ageage/docs/pipeline.md` for the full pipeline reference and examples.

---

## Tips

- Keep `prompt` focused — broad instructions produce unfocused behavior
- Use `tools` to restrict the skill from accessing unneeded tools
- The skill's `prompt` is appended **after** AGENT.md — it can override defaults
- Skills cannot invoke other skills; use a pipeline for multi-stage workflows
- The `{ageage-dir}/skills/` directory path is shown by `agent_info` if available
