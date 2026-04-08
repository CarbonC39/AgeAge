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
	model string
}

func newCLIUI(model string) *cliUI { return &cliUI{model: model} }

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
	stats := fmt.Sprintf("↑ %d  ↓ %d  ∑ %d tokens",
		usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	fmt.Println(stDim.Render("  " + stats))
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
