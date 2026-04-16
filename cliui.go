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

	if f.showThink {
		// Print the full think content in dim colour with a header.
		header := stGray.Render("┌── thinking ") + stDim.Render(line(38))
		footer := stGray.Render("└" + line(50))
		fmt.Println()
		fmt.Println(header)
		// Print each line dimmed.
		for _, l := range strings.Split(strings.TrimSpace(content), "\n") {
			fmt.Println(stDim.Render("│ " + l))
		}
		fmt.Println(footer)
		fmt.Println()
	} else {
		// Summary line only.
		chars := len([]rune(strings.TrimSpace(content)))
		fmt.Println(stDim.Render(fmt.Sprintf("  ◆ thinking… (%d chars)", chars)))
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

var (
	colPink   = lipgloss.Color("#F472B6")
	colBlue   = lipgloss.Color("#60A5FA")
	colPurple = lipgloss.Color("#C084FC")
	colGray   = lipgloss.Color("#6B7280")
	colGreen  = lipgloss.Color("#34D399")
	colRed    = lipgloss.Color("#F87171")
	colAmber  = lipgloss.Color("#FBBF24")
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

const diffMaxLines = 8

// printFileWrite renders the content being written to a file (first N lines).
func (u *cliUI) printFileWrite(path, content string) {
	fmt.Printf("  %s %s\n",
		stAmber.Render("┌─ Write"),
		stBlue.Render(path),
	)
	lines := strings.Split(content, "\n")
	shown := 0
	for _, l := range lines {
		if shown >= diffMaxLines {
			fmt.Printf("  %s\n", stDim.Render("    … (truncated)"))
			break
		}
		fmt.Printf("  %s %s\n", stGreen.Render("+"), l)
		shown++
	}
	fmt.Printf("  %s\n", stAmber.Render("└─────────────"))
}

// printFileEdit renders old→new diff for a file_edit operation.
func (u *cliUI) printFileEdit(path, oldStr, newStr string) {
	fmt.Printf("  %s %s\n",
		stAmber.Render("┌─ Edit"),
		stBlue.Render(path),
	)
	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")
	shown := 0
	for _, l := range oldLines {
		if shown >= diffMaxLines {
			fmt.Printf("  %s\n", stDim.Render("    … (truncated)"))
			break
		}
		fmt.Printf("  %s %s\n", stRed.Render("-"), stDim.Render(l))
		shown++
	}
	shown = 0
	for _, l := range newLines {
		if shown >= diffMaxLines {
			fmt.Printf("  %s\n", stDim.Render("    … (truncated)"))
			break
		}
		fmt.Printf("  %s %s\n", stGreen.Render("+"), l)
		shown++
	}
	fmt.Printf("  %s\n", stAmber.Render("└─────────────"))
}
