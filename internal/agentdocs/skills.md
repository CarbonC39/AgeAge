# Skills Reference

## What Skills Are

Markdown files in `{workspace}/skills/`. Each defines a specialized agent mode.

Triggered by `/skill-name` in a message, or auto-selected by the router based on intent.
Hot-reloaded every 2 seconds — no restart needed after saving.

---

## File Format

```
---
name: skill-name
description: One-line description (shown in /skills list and used for routing)
complexity: medium
tools: [bash, file_read, file_write]
prompt: |
  System instructions for this skill.
  Use {{input}} to reference the user's message text.
---
```

### Frontmatter Fields

| Field | Required | Values | Effect |
|-------|----------|--------|--------|
| name | yes | slug | command name, routing key |
| description | yes | string | shown in /skills, used by router |
| complexity | no | simple / medium / complex | model selection when router enabled |
| tools | no | list of tool names | allowlist; omit to allow all tools |
| prompt | no | string | prepended to system prompt when skill is active |
| pipeline | no | true | marks this as a pipeline skill (see pipeline doc) |

Body content after the frontmatter closing `---` is ignored for regular skills.

---

## Creating or Modifying a Skill

Use `file_write` to create `{skillsDir}/my-skill.md`:

```markdown
---
name: code-review
description: Review code for correctness, style, and security issues
complexity: complex
tools: [file_read, glob, grep, bash]
prompt: |
  You are a senior code reviewer. Focus on:
  - Correctness and edge cases
  - Security vulnerabilities
  - Idiomatic style for the detected language
  Always provide specific line references and actionable suggestions.
---
```

Then trigger with `/code-review <context>`.

---

## Complexity and Model Selection

When the router is enabled (`router.enabled = true`):

| complexity | model used |
|-----------|------------|
| simple | router.router model (lightweight) |
| medium | main LLM model |
| complex | router.strong model (if configured) |

Omitting `complexity` defaults to medium routing.

---

## Pipeline Skills

Add `pipeline: true` to frontmatter, then define nodes in YAML after the second `---`.
Call `framework_doc("pipeline")` for the full pipeline reference and examples.

---

## Tips

- Keep `prompt` focused — broad instructions produce unfocused behavior
- Use `tools` allowlist to prevent the skill from using unneeded tools
- The skill's `prompt` is appended **after** AGENT.md — it can override defaults
- Skills cannot call other skills; use a pipeline for multi-stage workflows
