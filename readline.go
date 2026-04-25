package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"

	cxterm "github.com/charmbracelet/x/term"
)

// ErrInterrupt is returned by Readline.ReadLine when the user presses Ctrl+C.
var ErrInterrupt = errors.New("interrupt")

// Readline provides line editing with history navigation and cursor movement.
// It runs in raw terminal mode when stdout is a TTY, and falls back to plain
// bufio line reading (no editing) when piping.
type Readline struct {
	// PromptAnsi is the ANSI-escaped prompt printed before each input line.
	// It must be set before calling ReadLine.
	PromptAnsi string

	history []string
	histIdx int    // current position in history; len(history) = live input
	saved   string // live input saved while browsing history
}

// AddHistory appends a non-empty line to the in-memory history ring.
func (rl *Readline) AddHistory(line string) {
	if line != "" {
		rl.history = append(rl.history, line)
	}
}

// LoadHistory reads history entries from path (one per line).
// A missing file is silently ignored; any other error is also ignored.
func (rl *Readline) LoadHistory(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if line != "" {
			rl.history = append(rl.history, line)
		}
	}
}

// SaveHistory writes history to path (one entry per line), keeping only the
// most recent maxLines entries to cap file growth.  maxLines ≤ 0 means no limit.
func (rl *Readline) SaveHistory(path string, maxLines int) error {
	h := rl.history
	if maxLines > 0 && len(h) > maxLines {
		h = h[len(h)-maxLines:]
	}
	var sb strings.Builder
	for _, line := range h {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// ReadLine reads one input line from the terminal.
//
// In raw mode it supports:
//   - Left / Right arrow keys — cursor movement within the line
//   - Up / Down arrow keys   — history navigation
//   - Home / End (Ctrl+A/E)  — jump to line start / end
//   - Ctrl+K                 — erase to end of line
//   - Ctrl+U                 — erase whole line
//   - Ctrl+W                 — erase previous word
//   - Backspace / Delete      — delete character
//   - Ctrl+C                 — returns ErrInterrupt
//   - Ctrl+D on empty line   — returns io.EOF
func (rl *Readline) ReadLine() (string, error) {
	fd := os.Stdin.Fd()
	if !cxterm.IsTerminal(fd) {
		return rl.simpleRead()
	}

	state, err := cxterm.MakeRaw(fd)
	if err != nil {
		return rl.simpleRead()
	}
	defer cxterm.Restore(fd, state) //nolint:errcheck

	buf := []rune{}
	pos := 0
	rl.histIdx = len(rl.history)
	rl.saved = ""

	// redraw reprints the full line from column 0 and repositions the cursor.
	redraw := func() {
		s := "\r\033[K" + rl.PromptAnsi + string(buf)
		if d := len(buf) - pos; d > 0 {
			s += "\033[" + itoa(d) + "D"
		}
		os.Stdout.WriteString(s) //nolint:errcheck
	}

	reader := bufio.NewReaderSize(os.Stdin, 256)

	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			if err == io.EOF {
				return string(buf), io.EOF
			}
			return string(buf), err
		}

		switch r {

		case '\r', '\n': // Enter
			os.Stdout.WriteString("\r\n") //nolint:errcheck
			return string(buf), nil

		case 3: // Ctrl+C
			os.Stdout.WriteString("\r\n") //nolint:errcheck
			return "", ErrInterrupt

		case 4: // Ctrl+D — EOF on empty line; otherwise delete forward
			if len(buf) == 0 {
				os.Stdout.WriteString("\r\n") //nolint:errcheck
				return "", io.EOF
			}
			if pos < len(buf) {
				buf = append(buf[:pos], buf[pos+1:]...)
				redraw()
			}

		case 1: // Ctrl+A — beginning of line
			pos = 0
			redraw()

		case 5: // Ctrl+E — end of line
			pos = len(buf)
			redraw()

		case 11: // Ctrl+K — kill to end
			buf = buf[:pos]
			redraw()

		case 21: // Ctrl+U — kill whole line
			buf = buf[:0]
			pos = 0
			redraw()

		case 23: // Ctrl+W — kill previous word
			end := pos
			for pos > 0 && buf[pos-1] == ' ' {
				pos--
			}
			for pos > 0 && buf[pos-1] != ' ' {
				pos--
			}
			buf = append(buf[:pos], buf[end:]...)
			redraw()

		case 127, 8: // Backspace / DEL
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
				redraw()
			}

		case 27: // ESC — start of escape sequence
			b1, _, err := reader.ReadRune()
			if err != nil || (b1 != '[' && b1 != 'O') {
				continue
			}
			b2, _, err := reader.ReadRune()
			if err != nil {
				continue
			}

			// Extended sequences like \033[3~ (Delete), \033[1~, etc.
			if b2 >= '0' && b2 <= '9' {
				term, _, _ := reader.ReadRune() // consume trailing ~
				if term == '~' && b1 == '[' {
					switch b2 {
					case '3': // Delete key
						if pos < len(buf) {
							buf = append(buf[:pos], buf[pos+1:]...)
							redraw()
						}
					case '1', '7': // Home
						pos = 0
						redraw()
					case '4', '8': // End
						pos = len(buf)
						redraw()
					}
				}
				continue
			}

			switch b2 {
			case 'A': // Up — previous history entry
				if rl.histIdx > 0 {
					if rl.histIdx == len(rl.history) {
						rl.saved = string(buf)
					}
					rl.histIdx--
					buf = []rune(rl.history[rl.histIdx])
					pos = len(buf)
					redraw()
				}
			case 'B': // Down — next history entry
				if rl.histIdx < len(rl.history) {
					rl.histIdx++
					if rl.histIdx == len(rl.history) {
						buf = []rune(rl.saved)
					} else {
						buf = []rune(rl.history[rl.histIdx])
					}
					pos = len(buf)
					redraw()
				}
			case 'C': // Right
				if pos < len(buf) {
					pos++
					os.Stdout.WriteString("\033[1C") //nolint:errcheck
				}
			case 'D': // Left
				if pos > 0 {
					pos--
					os.Stdout.WriteString("\033[1D") //nolint:errcheck
				}
			case 'H': // Home (VT220)
				pos = 0
				redraw()
			case 'F': // End (VT220)
				pos = len(buf)
				redraw()
			}

		default:
			if r >= 32 { // printable rune — insert at cursor
				buf = append(buf, 0)
				copy(buf[pos+1:], buf[pos:])
				buf[pos] = r
				pos++
				redraw()
			}
		}
	}
}

// simpleRead reads one line without any editing support (for non-TTY contexts).
func (rl *Readline) simpleRead() (string, error) {
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	// Strip trailing newline characters.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}

// itoa converts a non-negative integer to its decimal string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
