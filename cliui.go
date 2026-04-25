package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"ageage/llm"

	"github.com/charmbracelet/lipgloss"
)

// ── Think-block stream filter ──────────────────────────────────────────────────

// ThinkStreamFilter wraps a StreamCallback and intercepts thinking-block tags
// produced by reasoning models:
//   - <think>…</think>   DeepSeek-R1, QwQ
//   - <thought>…</thought>  Gemma 4
//
// When showThink=false (default): think content is suppressed during streaming;
// after the closing tag a single summary line is printed.
// When showThink=true: think content is streamed in dim colour before the response.
//
// LastThink holds the raw text of the most recent think block so the /think
// command can replay it.
type ThinkStreamFilter struct {
	inner     llm.StreamCallback
	showThink bool

	// Hooks called around think-block output so the caller can pause/resume
	// any concurrent rendering (e.g. stop a spinner before printing, restart after).
	OnThinkBegin func()
	OnThinkEnd   func()

	// streaming state
	buf         string          // accumulates tokens until a safe flush point
	inThink     bool
	closeTag    string          // closing tag matching the open tag we entered with
	thinkBuf    strings.Builder // content inside the current think block
	hasThink    bool            // at least one think block seen this turn
	contentSeen bool            // real (non-whitespace) content already emitted; ignore subsequent think tags

	// exported result
	LastThink string
}

// thinking open/close tag pairs, in match-priority order.
var thinkOpenTags = []string{"<think>", "<thought>"}
var thinkCloseTags = map[string]string{
	"<think>":   "</think>",
	"<thought>": "</thought>",
}

// Wrap returns a StreamCallback that routes tokens through the filter.
// Call Reset() before each agent turn to clear state.
func (f *ThinkStreamFilter) Wrap() llm.StreamCallback {
	return f.feed
}

// Reset clears per-turn state. Call before starting a new agent run.
func (f *ThinkStreamFilter) Reset() {
	f.buf = ""
	f.inThink = false
	f.closeTag = ""
	f.thinkBuf.Reset()
	f.hasThink = false
	f.contentSeen = false
}

// feed is the actual StreamCallback implementation.
func (f *ThinkStreamFilter) feed(token string) {
	f.buf += token
	for {
		if !f.inThink {
			// Once real content has been emitted, pass everything through
			// without entering think mode — subsequent tags are body text.
			if f.contentSeen {
				if f.inner != nil && f.buf != "" {
					f.inner(f.buf)
				}
				f.buf = ""
				return
			}

			// Look for whichever opening tag appears first.
			idx, openTag := firstTagIndex(f.buf, thinkOpenTags)
			if idx != -1 {
				// Flush content before the tag.
				if idx > 0 && f.inner != nil {
					f.inner(f.buf[:idx])
					if strings.TrimSpace(f.buf[:idx]) != "" {
						f.contentSeen = true
					}
				}
				f.buf = f.buf[idx+len(openTag):]
				f.inThink = true
				f.closeTag = thinkCloseTags[openTag]
				f.thinkBuf.Reset()
				continue
			}
			// No opening tag; hold any tail that could be a partial match.
			safe, held := splitAtPartialTagAny(f.buf, thinkOpenTags)
			if safe != "" {
				if f.inner != nil {
					f.inner(safe)
				}
				if strings.TrimSpace(safe) != "" {
					f.contentSeen = true
				}
			}
			f.buf = held
			return
		}

		// Inside think block: look for the matching closing tag.
		idx := strings.Index(f.buf, f.closeTag)
		if idx != -1 {
			f.thinkBuf.WriteString(f.buf[:idx])
			f.buf = f.buf[idx+len(f.closeTag):]
			f.inThink = false
			f.onThinkEnd()
			continue
		}
		// No closing tag; hold the tail that could be a partial match.
		safe, held := splitAtPartialTag(f.buf, f.closeTag)
		f.thinkBuf.WriteString(safe)
		f.buf = held
		return
	}
}

// Flush drains any remaining buffered content at end-of-stream.
func (f *ThinkStreamFilter) Flush() {
	if f.buf == "" {
		return
	}
	if f.inThink {
		f.thinkBuf.WriteString(f.buf)
	} else if f.inner != nil {
		f.inner(f.buf)
	}
	f.buf = ""
}

func (f *ThinkStreamFilter) onThinkEnd() {
	content := f.thinkBuf.String()
	f.LastThink = content
	f.hasThink = true

	// Stop any concurrent rendering (e.g. spinner) before writing to stdout.
	if f.OnThinkBegin != nil {
		f.OnThinkBegin()
	}

	if f.showThink {
		header := stGray.Render("┌── thinking ") + stDim.Render(line(38))
		footer := stGray.Render("└" + line(50))
		fmt.Println()
		fmt.Println(header)
		for _, l := range strings.Split(strings.TrimSpace(content), "\n") {
			fmt.Println(stDim.Render("│ " + l))
		}
		fmt.Println(footer)
		fmt.Println()
	} else {
		chars := len([]rune(strings.TrimSpace(content)))
		fmt.Println(stDim.Render(fmt.Sprintf("  ◆ thinking… (%d chars)", chars)))
	}

	// Resume concurrent rendering after output is complete.
	if f.OnThinkEnd != nil {
		f.OnThinkEnd()
	}
}

// firstTagIndex returns the position and matched tag of the earliest occurrence
// of any tag in tags within s. Returns (-1, "") if none found.
func firstTagIndex(s string, tags []string) (int, string) {
	best, bestTag := -1, ""
	for _, tag := range tags {
		if idx := strings.Index(s, tag); idx != -1 && (best == -1 || idx < best) {
			best, bestTag = idx, tag
		}
	}
	return best, bestTag
}

// splitAtPartialTagAny holds the longest tail of s that could be a partial
// prefix of any tag in tags, returning the safe prefix and held suffix.
func splitAtPartialTagAny(s string, tags []string) (safe, held string) {
	bestHeld := 0
	for _, tag := range tags {
		_, h := splitAtPartialTag(s, tag)
		if len(h) > bestHeld {
			bestHeld = len(h)
		}
	}
	return s[:len(s)-bestHeld], s[len(s)-bestHeld:]
}

// splitAtPartialTag splits s into a safe-to-emit prefix and a held suffix.
// The suffix is the longest tail of s that could be the start of tag.
func splitAtPartialTag(s, tag string) (safe, held string) {
	// Walk backwards: find the longest suffix of s that is a prefix of tag.
	for l := min(len(s), len(tag)-1); l > 0; l-- {
		if strings.HasPrefix(tag, s[len(s)-l:]) {
			return s[:len(s)-l], s[len(s)-l:]
		}
	}
	return s, ""
}

// ── Colour palette ────────────────────────────────────────────────────────────
// Each color has a dark-background and a light-background variant.
// lipgloss selects automatically based on the terminal's color profile.

var (
	colPink   = lipgloss.AdaptiveColor{Dark: "#F472B6", Light: "#BE185D"}
	colBlue   = lipgloss.AdaptiveColor{Dark: "#60A5FA", Light: "#1D4ED8"}
	colPurple = lipgloss.AdaptiveColor{Dark: "#C084FC", Light: "#7E22CE"}
	colGray   = lipgloss.AdaptiveColor{Dark: "#9CA3AF", Light: "#6B7280"}
	colGreen  = lipgloss.AdaptiveColor{Dark: "#34D399", Light: "#047857"}
	colRed    = lipgloss.AdaptiveColor{Dark: "#F87171", Light: "#DC2626"}
	colAmber  = lipgloss.AdaptiveColor{Dark: "#FBBF24", Light: "#B45309"}
)

var (
	stPink   = lipgloss.NewStyle().Foreground(colPink).Bold(true)
	stBlue   = lipgloss.NewStyle().Foreground(colBlue).Bold(true)
	stPurple = lipgloss.NewStyle().Foreground(colPurple)
	stGray   = lipgloss.NewStyle().Foreground(colGray)
	stGreen  = lipgloss.NewStyle().Foreground(colGreen)
	stRed    = lipgloss.NewStyle().Foreground(colRed).Bold(true)
	stAmber  = lipgloss.NewStyle().Foreground(colAmber)
	stDim    = lipgloss.NewStyle().Foreground(colGray).Faint(true)
)

// Banner box styles
var bannerBox = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colBlue).
	Padding(0, 2)

// ── cliUI ─────────────────────────────────────────────────────────────────────

type cliUI struct {
	model      string
	sessionIn  int
	sessionOut int
}

func newCLIUI(model string) *cliUI { return &cliUI{model: model} }

// costPer1M returns the input and output cost per 1 million tokens for a
// recognised model name prefix. Returns (0, 0) for unknown models.
func costPer1M(model string) (in, out float64) {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "gpt-4o-mini"):
		return 0.15, 0.60
	case strings.HasPrefix(m, "gpt-4o"):
		return 2.50, 10.00
	case strings.HasPrefix(m, "gpt-4-turbo"):
		return 10.00, 30.00
	case strings.HasPrefix(m, "gpt-4"):
		return 30.00, 60.00
	case strings.HasPrefix(m, "gpt-3.5"):
		return 0.50, 1.50
	case strings.HasPrefix(m, "o1-mini"):
		return 1.10, 4.40
	case strings.HasPrefix(m, "o1"):
		return 15.00, 60.00
	case strings.HasPrefix(m, "o3-mini"):
		return 1.10, 4.40
	case strings.HasPrefix(m, "o3"):
		return 10.00, 40.00
	case strings.HasPrefix(m, "deepseek-r1"):
		return 0.55, 2.19
	case strings.HasPrefix(m, "deepseek"):
		return 0.27, 1.10
	case strings.HasPrefix(m, "claude-3-5-haiku"):
		return 0.80, 4.00
	case strings.HasPrefix(m, "claude-3-5"):
		return 3.00, 15.00
	case strings.HasPrefix(m, "claude-3-opus"):
		return 15.00, 75.00
	case strings.HasPrefix(m, "claude"):
		return 3.00, 15.00
	default:
		return 0, 0
	}
}

func (u *cliUI) printBanner() {
	title := stPink.Render("✦ AgeAge") + "  " + stPurple.Render(u.model)
	cmds := stDim.Render("/clear  /stop  /summarize  ·  exit")
	content := title + "\n" + cmds
	fmt.Println()
	fmt.Println(bannerBox.Render(content))
	fmt.Println()
}

func (u *cliUI) printPrompt() {
	fmt.Print(stPink.Render("You") + stGray.Render(" ▸ "))
}

func (u *cliUI) printAgentHeader() {
	bar := stBlue.Render("── Agent ") + stGray.Render(line(44))
	fmt.Println(bar)
}

func (u *cliUI) printUsage(usage llm.Usage) {
	if usage.TotalTokens == 0 {
		return
	}
	u.sessionIn += usage.PromptTokens
	u.sessionOut += usage.CompletionTokens

	// Per-turn line.
	turn := fmt.Sprintf("↑ %d  ↓ %d  ∑ %d tokens",
		usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	fmt.Println(stDim.Render("  " + turn))

	// Session-cumulative line with optional cost.
	inCost, outCost := costPer1M(u.model)
	if inCost > 0 {
		cost := float64(u.sessionIn)/1e6*inCost + float64(u.sessionOut)/1e6*outCost
		session := fmt.Sprintf("session ↑ %d  ↓ %d  ≈ $%.4f",
			u.sessionIn, u.sessionOut, cost)
		fmt.Println(stDim.Render("  " + session))
	} else {
		session := fmt.Sprintf("session ↑ %d  ↓ %d", u.sessionIn, u.sessionOut)
		fmt.Println(stDim.Render("  " + session))
	}
}

func (u *cliUI) printOK(msg string) {
	fmt.Println(stGreen.Render("  ✓  ") + msg)
}

func (u *cliUI) printErr(msg string) {
	fmt.Println(stRed.Render("  ✗  ") + msg)
}

func (u *cliUI) printWarn(msg string) {
	fmt.Println(stAmber.Render("  ⚠  ") + msg)
}

func (u *cliUI) printInfo(msg string) {
	fmt.Println(stGray.Render("  ·  ") + msg)
}

func (u *cliUI) printStatus(msg string) {
	fmt.Println(stBlue.Render("  ⟳  ") + msg)
}

func line(n int) string {
	return strings.Repeat("─", n)
}

// ── Spinner ───────────────────────────────────────────────────────────────────

// Spinner renders an animated indicator to stdout while work is in progress.
// It writes to the same line using carriage-return so it doesn't scroll.
type Spinner struct {
	frames  []string
	msg     string
	mu      sync.Mutex
	stopCh  chan struct{}
	doneCh  chan struct{}
	running bool
}

func newSpinner() *Spinner {
	return &Spinner{
		frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	}
}

// Start begins animating with the given message. Safe to call multiple times
// (updates the message if already running).
func (s *Spinner) Start(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		s.msg = msg
		return
	}
	s.msg = msg
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.running = true
	go func() {
		defer close(s.doneCh)
		idx := 0
		for {
			s.mu.Lock()
			m := s.msg
			s.mu.Unlock()
			fmt.Fprintf(os.Stdout, "\r%s %s",
				stGray.Render(s.frames[idx%len(s.frames)]),
				stDim.Render(m),
			)
			idx++
			select {
			case <-s.stopCh:
				fmt.Fprint(os.Stdout, "\r\033[K") // erase spinner line
				return
			case <-time.After(80 * time.Millisecond):
			}
		}
	}()
}

// Update changes the message while the spinner is running (no-op if stopped).
func (s *Spinner) Update(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()
}

// Stop halts the spinner and erases its line. Idempotent.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	close(s.stopCh)
	doneCh := s.doneCh
	s.mu.Unlock()
	<-doneCh
}

// ── Tool result display ───────────────────────────────────────────────────────

// toolResultMaxChars is how many characters of tool output to show inline.
const toolResultMaxChars = 300

// printToolResult prints a compact summary of a tool's return value.
// It is intentionally terse — the goal is glanceability, not full output.
func (u *cliUI) printToolResult(name, result string) {
	// Skip tools whose output is better shown via other callbacks (diff tools).
	switch name {
	case "file_write", "file_edit", "ask_user", "finish_task", "node_complete":
		return
	}
	if result == "" {
		return
	}
	// Condense multi-line output to a single representative line.
	firstLine := result
	lines := strings.SplitN(strings.TrimSpace(result), "\n", 2)
	if len(lines) > 0 {
		firstLine = strings.TrimSpace(lines[0])
	}
	lineCount := strings.Count(result, "\n") + 1
	suffix := ""
	if lineCount > 1 {
		suffix = fmt.Sprintf(" … +%d lines", lineCount-1)
	}
	if len([]rune(firstLine)) > toolResultMaxChars {
		firstLine = string([]rune(firstLine)[:toolResultMaxChars])
		suffix = "…"
	}
	fmt.Printf("  %s %s%s\n",
		stDim.Render("◁"),
		stDim.Render(firstLine),
		stDim.Render(suffix),
	)
}

// ── File diff display ─────────────────────────────────────────────────────────

const diffMaxLines = 16

// printFileWrite renders the content being written to a file (first N lines).
func (u *cliUI) printFileWrite(path, content string) {
	lines := strings.Split(content, "\n")
	fmt.Printf("  %s %s %s\n",
		stAmber.Render("┌─ Write"),
		stBlue.Render(path),
		stDim.Render(fmt.Sprintf("(%d lines)", len(lines))),
	)
	for i, l := range lines {
		if i >= diffMaxLines {
			fmt.Printf("  %s\n", stDim.Render(fmt.Sprintf("    … (%d more lines)", len(lines)-diffMaxLines)))
			break
		}
		fmt.Printf("  %s %s\n", stGreen.Render("+"), l)
	}
	fmt.Printf("  %s\n", stAmber.Render("└─────────────"))
}

// printFileEdit renders old→new diff for a file_edit operation.
func (u *cliUI) printFileEdit(path, oldStr, newStr string) {
	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")
	fmt.Printf("  %s %s %s\n",
		stAmber.Render("┌─ Edit"),
		stBlue.Render(path),
		stDim.Render(fmt.Sprintf("(-%d/+%d lines)", len(oldLines), len(newLines))),
	)
	for i, l := range oldLines {
		if i >= diffMaxLines {
			fmt.Printf("  %s\n", stDim.Render(fmt.Sprintf("    … (%d more lines)", len(oldLines)-diffMaxLines)))
			break
		}
		fmt.Printf("  %s %s\n", stRed.Render("-"), stDim.Render(l))
	}
	for i, l := range newLines {
		if i >= diffMaxLines {
			fmt.Printf("  %s\n", stDim.Render(fmt.Sprintf("    … (%d more lines)", len(newLines)-diffMaxLines)))
			break
		}
		fmt.Printf("  %s %s\n", stGreen.Render("+"), l)
	}
	fmt.Printf("  %s\n", stAmber.Render("└─────────────"))
}

// ── Bash display ──────────────────────────────────────────────────────────────

const bashOutputMaxLines = 40

// printBashCommand renders the shell command the agent is about to run.
func (u *cliUI) printBashCommand(cmd string) {
	lines := strings.Split(strings.TrimSpace(cmd), "\n")
	header := stGray.Render("  $ ") + stBlue.Render(lines[0])
	if len(lines) > 1 {
		header += stDim.Render(" …")
	}
	fmt.Println(header)
}

// printBashOutput renders the output returned by the shell command.
// When the output exceeds bashOutputMaxLines the LAST N lines are shown so
// that error messages (which appear at the end) are never truncated away.
func (u *cliUI) printBashOutput(output string) {
	output = strings.TrimRight(output, "\n")
	if output == "" {
		return
	}
	lines := strings.Split(output, "\n")
	if len(lines) > bashOutputMaxLines {
		dropped := len(lines) - bashOutputMaxLines
		fmt.Printf("  %s\n", stDim.Render(fmt.Sprintf("… (%d lines omitted)", dropped)))
		lines = lines[dropped:]
	}
	for _, l := range lines {
		fmt.Printf("  %s\n", stDim.Render(l))
	}
}

// ── Markdown rendering ────────────────────────────────────────────────────────

// renderInline processes inline markdown within a single text segment.
// Handles **bold**, `code`, and [text](url) links using only stdlib + lipgloss.
func renderInline(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		// **bold** — two-char ASCII marker, safe to byte-index
		if i+3 < len(s) && s[i] == '*' && s[i+1] == '*' {
			if end := strings.Index(s[i+2:], "**"); end >= 0 {
				out.WriteString(stBlue.Bold(true).Render(s[i+2 : i+2+end]))
				i += 2 + end + 2
				continue
			}
		}
		// `code`
		if s[i] == '`' {
			if end := strings.Index(s[i+1:], "`"); end >= 0 {
				out.WriteString(stAmber.Render(s[i+1 : i+1+end]))
				i += 1 + end + 1
				continue
			}
		}
		// [text](url)
		if s[i] == '[' {
			if textEnd := strings.Index(s[i+1:], "]("); textEnd >= 0 {
				urlStart := i + 1 + textEnd + 2
				if urlEnd := strings.Index(s[urlStart:], ")"); urlEnd >= 0 {
					text := s[i+1 : i+1+textEnd]
					url := s[urlStart : urlStart+urlEnd]
					out.WriteString(stBlue.Render(text))
					out.WriteString(stGray.Render(" (" + url + ")"))
					i = urlStart + urlEnd + 1
					continue
				}
			}
		}
		// Pass all other bytes through (multi-byte UTF-8 chars copy correctly
		// one byte at a time since non-ASCII bytes never match the markers above).
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// renderMarkdown applies minimal terminal formatting to markdown text.
// Handles code fences, headers, bullet/numbered lists, and inline markup —
// no external dependencies.
func (u *cliUI) renderMarkdown(text string) string {
	// Normalize CR/CRLF: some LLM JSON responses encode \r as carriage return,
	// which moves the terminal cursor to column 0 and corrupts the display.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "")
	lines := strings.Split(text, "\n")
	var sb strings.Builder
	inCode := false
	lang := ""
	for _, l := range lines {
		// Code fence toggle.
		if strings.HasPrefix(l, "```") {
			if !inCode {
				inCode = true
				lang = strings.TrimPrefix(l, "```")
				header := stGray.Render("  ┌─ ")
				if lang != "" {
					header += stDim.Render(lang)
				}
				sb.WriteString(header + "\n")
			} else {
				inCode = false
				lang = ""
				sb.WriteString(stGray.Render("  └─────────────") + "\n")
			}
			continue
		}
		if inCode {
			sb.WriteString(stDim.Render("  │ ") + l + "\n")
			continue
		}
		// Headers.
		if strings.HasPrefix(l, "### ") {
			sb.WriteString(stGray.Render(renderInline(l[4:])) + "\n")
			continue
		}
		if strings.HasPrefix(l, "## ") {
			sb.WriteString(stBlue.Render(renderInline(l[3:])) + "\n")
			continue
		}
		if strings.HasPrefix(l, "# ") {
			sb.WriteString(stBlue.Bold(true).Render(renderInline(l[2:])) + "\n")
			continue
		}
		// Bullet lists.
		if strings.HasPrefix(l, "- ") || strings.HasPrefix(l, "* ") {
			sb.WriteString("  " + stGray.Render("•") + " " + renderInline(l[2:]) + "\n")
			continue
		}
		// Numbered lists: "1. " through "99. "
		if len(l) > 3 && l[0] >= '1' && l[0] <= '9' {
			if dot := strings.Index(l, ". "); dot >= 1 && dot <= 3 {
				prefix := l[:dot+2]
				rest := l[dot+2:]
				sb.WriteString("  " + stGray.Render(prefix) + renderInline(rest) + "\n")
				continue
			}
		}
		sb.WriteString(renderInline(l) + "\n")
	}
	return sb.String()
}
