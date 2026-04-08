package channel

import (
	"regexp"
	"strings"
)

// markdownToHTML provides a basic Markdown → HTML conversion for Matrix's
// org.matrix.custom.html formatted_body. It handles the most common patterns
// that LLM output uses: bold, italic, code, headings, links, and lists.
func markdownToHTML(md string) string {
	lines := strings.Split(md, "\n")
	var result []string
	inCodeBlock := false
	inList := false

	for _, line := range lines {
		// Code blocks (``` ... ```)
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inCodeBlock {
				result = append(result, "</code></pre>")
				inCodeBlock = false
			} else {
				if inList {
					result = append(result, "</ul>")
					inList = false
				}
				result = append(result, "<pre><code>")
				inCodeBlock = true
			}
			continue
		}

		if inCodeBlock {
			result = append(result, escapeHTML(line))
			continue
		}

		// Close list if current line is not a list item.
		trimmed := strings.TrimSpace(line)
		isList := strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") ||
			matchOrderedList(trimmed)
		if inList && !isList && trimmed != "" {
			result = append(result, "</ul>")
			inList = false
		}

		// Headings.
		if strings.HasPrefix(trimmed, "### ") {
			result = append(result, "<h3>"+inlineMarkdown(trimmed[4:])+"</h3>")
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			result = append(result, "<h2>"+inlineMarkdown(trimmed[3:])+"</h2>")
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			result = append(result, "<h1>"+inlineMarkdown(trimmed[2:])+"</h1>")
			continue
		}

		// Unordered list items.
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			if !inList {
				result = append(result, "<ul>")
				inList = true
			}
			content := trimmed[2:]
			result = append(result, "<li>"+inlineMarkdown(content)+"</li>")
			continue
		}

		// Ordered list items.
		if matchOrderedList(trimmed) {
			if !inList {
				result = append(result, "<ul>")
				inList = true
			}
			// Strip "1. " prefix.
			idx := strings.Index(trimmed, ". ")
			if idx >= 0 {
				content := trimmed[idx+2:]
				result = append(result, "<li>"+inlineMarkdown(content)+"</li>")
			}
			continue
		}

		// Empty line.
		if trimmed == "" {
			result = append(result, "<br/>")
			continue
		}

		// Normal paragraph.
		result = append(result, "<p>"+inlineMarkdown(line)+"</p>")
	}

	if inList {
		result = append(result, "</ul>")
	}
	if inCodeBlock {
		result = append(result, "</code></pre>")
	}

	return strings.Join(result, "\n")
}

var reOrderedList = regexp.MustCompile(`^\d+\.\s`)

func matchOrderedList(line string) bool {
	return reOrderedList.MatchString(line)
}

// inlineMarkdown converts inline Markdown (bold, italic, code, links) to HTML.
func inlineMarkdown(text string) string {
	// Inline code: `code`
	reCode := regexp.MustCompile("`([^`]+)`")
	text = reCode.ReplaceAllString(text, "<code>$1</code>")

	// Bold: **text** or __text__
	reBold := regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	text = reBold.ReplaceAllStringFunc(text, func(match string) string {
		inner := strings.TrimPrefix(strings.TrimSuffix(match, "**"), "**")
		if inner == match {
			inner = strings.TrimPrefix(strings.TrimSuffix(match, "__"), "__")
		}
		return "<strong>" + inner + "</strong>"
	})

	// Italic: *text* or _text_ (but not inside bold markers)
	reItalic := regexp.MustCompile(`(?:^|[^*_])\*([^*]+)\*(?:[^*_]|$)|(?:^|[^*_])_([^_]+)_(?:[^*_]|$)`)
	text = reItalic.ReplaceAllStringFunc(text, func(match string) string {
		// Simple approach: just wrap with <em>
		trimmed := strings.Trim(match, " ")
		if strings.HasPrefix(trimmed, "*") && strings.HasSuffix(trimmed, "*") {
			inner := strings.TrimPrefix(strings.TrimSuffix(trimmed, "*"), "*")
			return strings.Replace(match, trimmed, "<em>"+inner+"</em>", 1)
		}
		return match
	})

	// Links: [text](url)
	reLink := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	text = reLink.ReplaceAllString(text, `<a href="$2">$1</a>`)

	return text
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
