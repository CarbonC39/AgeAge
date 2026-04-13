package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"ageage/config"

	readability "codeberg.org/readeck/go-readability/v2"
	playwright "github.com/playwright-community/playwright-go"
)

// ── Backend interface ────────────────────────────────────────────────────────

// browserBackend is the internal interface both backends implement.
type browserBackend interface {
	// Navigate opens the given URL and returns the page title and readable text.
	Navigate(url, waitUntil string, timeout time.Duration) (title, text string, err error)
	// Action performs a single interaction on the page.
	Action(action, selector, value string, timeout time.Duration) (string, error)
	// Content returns the current page content in the requested format.
	Content(format, selector string, timeout time.Duration) (string, error)
	// Close releases all browser resources.
	Close()
}

// ── BrowserSession ───────────────────────────────────────────────────────────

// BrowserSession is a single browser lifecycle shared by all three browser_*
// tools for one agent Run() call. It is created lazily on first use and
// closed by the agent's deferred cleanup block.
type BrowserSession struct {
	cfg     *config.BrowserConfig
	backend browserBackend
	mu      sync.Mutex
	once    sync.Once
	openErr error
}

// NewBrowserSession creates a session bound to the given config.
// The browser is NOT opened yet; it opens on the first tool call.
func NewBrowserSession(cfg *config.BrowserConfig) *BrowserSession {
	return &BrowserSession{cfg: cfg}
}

// open initialises the backend exactly once. Subsequent calls are no-ops.
func (s *BrowserSession) open() error {
	s.once.Do(func() {
		switch s.cfg.Backend {
		case "agent-browser":
			b, err := newAgentBrowserBackend(s.cfg)
			if err != nil {
				s.openErr = err
				return
			}
			s.backend = b
		default: // "playwright"
			b, err := newPlaywrightBackend(s.cfg)
			if err != nil {
				s.openErr = err
				return
			}
			s.backend = b
		}
	})
	return s.openErr
}

// Close shuts down the browser. Safe to call multiple times.
func (s *BrowserSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backend != nil {
		s.backend.Close()
		s.backend = nil
	}
}

func (s *BrowserSession) navigate(url, waitUntil string, timeout time.Duration) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(); err != nil {
		return "", "", err
	}
	return s.backend.Navigate(url, waitUntil, timeout)
}

func (s *BrowserSession) action(action, selector, value string, timeout time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(); err != nil {
		return "", err
	}
	return s.backend.Action(action, selector, value, timeout)
}

func (s *BrowserSession) content(format, selector string, timeout time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(); err != nil {
		return "", err
	}
	return s.backend.Content(format, selector, timeout)
}

func (s *BrowserSession) timeout() time.Duration {
	t := s.cfg.Timeout
	if t <= 0 {
		t = 30
	}
	return time.Duration(t) * time.Second
}

// ── Playwright backend ───────────────────────────────────────────────────────

type playwrightBackend struct {
	pw          *playwright.Playwright
	context     playwright.BrowserContext
	page        playwright.Page
	userDataDir string
}

func newPlaywrightBackend(cfg *config.BrowserConfig) (*playwrightBackend, error) {
	runOpts := &playwright.RunOptions{SkipInstallBrowsers: true}
	pw, err := playwright.Run(runOpts)
	if err != nil {
		return nil, fmt.Errorf("playwright: failed to start: %w", err)
	}

	// Create a unique temporary directory for this browser session.
	// This prevents port/lock conflicts when multiple agents run in parallel.
	userDataDir, err := os.MkdirTemp("", "ageage-browser-*")
	if err != nil {
		pw.Stop() //nolint:errcheck
		return nil, fmt.Errorf("playwright: failed to create user data dir: %w", err)
	}

	launchOpts := playwright.BrowserTypeLaunchPersistentContextOptions{
		Headless: playwright.Bool(cfg.Headless),
	}

	var context playwright.BrowserContext
	switch cfg.BrowserType {
	case "firefox":
		context, err = pw.Firefox.LaunchPersistentContext(userDataDir, launchOpts)
	case "webkit":
		context, err = pw.WebKit.LaunchPersistentContext(userDataDir, launchOpts)
	default: // "chromium"
		context, err = pw.Chromium.LaunchPersistentContext(userDataDir, launchOpts)
	}
	if err != nil {
		os.RemoveAll(userDataDir) //nolint:errcheck
		pw.Stop()                 //nolint:errcheck
		return nil, fmt.Errorf("playwright: failed to launch browser context: %w", err)
	}

	pages := context.Pages()
	var page playwright.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = context.NewPage()
		if err != nil {
			context.Close()           //nolint:errcheck
			os.RemoveAll(userDataDir) //nolint:errcheck
			pw.Stop()                 //nolint:errcheck
			return nil, fmt.Errorf("playwright: failed to create page: %w", err)
		}
	}

	return &playwrightBackend{pw: pw, context: context, page: page, userDataDir: userDataDir}, nil
}

func (b *playwrightBackend) Navigate(rawURL, waitUntil string, timeout time.Duration) (string, string, error) {
	waitEvent := playwright.WaitUntilStateLoad
	switch waitUntil {
	case "networkidle":
		waitEvent = playwright.WaitUntilStateNetworkidle
	case "domcontentloaded":
		waitEvent = playwright.WaitUntilStateDomcontentloaded
	case "commit":
		waitEvent = playwright.WaitUntilStateCommit
	}

	ms := float64(timeout.Milliseconds())
	_, err := b.page.Goto(rawURL, playwright.PageGotoOptions{
		WaitUntil: waitEvent,
		Timeout:   &ms,
	})
	if err != nil {
		return "", "", fmt.Errorf("navigate to %s: %w", rawURL, err)
	}

	title, _ := b.page.Title()
	html, err := b.page.Content()
	if err != nil {
		return title, "", fmt.Errorf("get page content: %w", err)
	}
	text := extractReadable(html, rawURL)
	return title, text, nil
}

func (b *playwrightBackend) Action(action, selector, value string, timeout time.Duration) (string, error) {
	ms := float64(timeout.Milliseconds())

	loc := func() playwright.Locator {
		if selector == "" {
			return b.page.Locator("body")
		}
		return b.page.Locator(selector)
	}

	switch action {
	case "click":
		if err := loc().Click(playwright.LocatorClickOptions{Timeout: &ms}); err != nil {
			return "", fmt.Errorf("click %q: %w", selector, err)
		}
		return fmt.Sprintf("clicked %q", selector), nil

	case "type", "fill":
		if err := loc().Fill(value, playwright.LocatorFillOptions{Timeout: &ms}); err != nil {
			return "", fmt.Errorf("fill %q: %w", selector, err)
		}
		return fmt.Sprintf("filled %q with text", selector), nil

	case "hover":
		if err := loc().Hover(playwright.LocatorHoverOptions{Timeout: &ms}); err != nil {
			return "", fmt.Errorf("hover %q: %w", selector, err)
		}
		return fmt.Sprintf("hovered %q", selector), nil

	case "scroll":
		// value: "up" | "down" | "<pixels>"
		dir := "window.scrollBy(0, 600)"
		if value == "up" {
			dir = "window.scrollBy(0, -600)"
		} else if value != "" && value != "down" {
			dir = fmt.Sprintf("window.scrollBy(0, %s)", value)
		}
		if _, err := b.page.Evaluate(dir); err != nil {
			return "", fmt.Errorf("scroll: %w", err)
		}
		return "scrolled", nil

	case "select":
		if _, err := loc().SelectOption(playwright.SelectOptionValues{Values: &[]string{value}},
			playwright.LocatorSelectOptionOptions{Timeout: &ms}); err != nil {
			return "", fmt.Errorf("select %q: %w", selector, err)
		}
		return fmt.Sprintf("selected %q in %q", value, selector), nil

	case "press":
		if err := loc().Press(value, playwright.LocatorPressOptions{Timeout: &ms}); err != nil {
			return "", fmt.Errorf("press %q on %q: %w", value, selector, err)
		}
		return fmt.Sprintf("pressed %q on %q", value, selector), nil

	case "check":
		if err := loc().Check(playwright.LocatorCheckOptions{Timeout: &ms}); err != nil {
			return "", fmt.Errorf("check %q: %w", selector, err)
		}
		return fmt.Sprintf("checked %q", selector), nil

	case "uncheck":
		if err := loc().Uncheck(playwright.LocatorUncheckOptions{Timeout: &ms}); err != nil {
			return "", fmt.Errorf("uncheck %q: %w", selector, err)
		}
		return fmt.Sprintf("unchecked %q", selector), nil

	default:
		return "", fmt.Errorf("unknown action %q; supported: click, type, fill, hover, scroll, select, press, check, uncheck", action)
	}
}

func (b *playwrightBackend) Content(format, selector string, timeout time.Duration) (string, error) {
	switch format {
	case "html":
		if selector != "" {
			ms := float64(timeout.Milliseconds())
			return b.page.Locator(selector).InnerHTML(playwright.LocatorInnerHTMLOptions{Timeout: &ms})
		}
		return b.page.Content()

	case "snapshot":
		// Accessibility tree snapshot — useful for AI navigation.
		snap, err := b.page.Locator("body").AriaSnapshot()
		if err != nil {
			return "", fmt.Errorf("accessibility snapshot: %w", err)
		}
		if snap == "" {
			return "(empty snapshot)", nil
		}
		return snap, nil

	default: // "text"
		if selector != "" {
			ms := float64(timeout.Milliseconds())
			return b.page.Locator(selector).InnerText(playwright.LocatorInnerTextOptions{Timeout: &ms})
		}
		html, err := b.page.Content()
		if err != nil {
			return "", err
		}
		pageURL := b.page.URL()
		return extractReadable(html, pageURL), nil
	}
}

func (b *playwrightBackend) Close() {
	b.page.Close()    //nolint:errcheck
	b.context.Close() //nolint:errcheck
	b.pw.Stop()       //nolint:errcheck
	if b.userDataDir != "" {
		os.RemoveAll(b.userDataDir) //nolint:errcheck
	}
}

// ── agent-browser backend ────────────────────────────────────────────────────

// agentBrowserBackend drives the agent-browser CLI (https://github.com/vercel-labs/agent-browser).
// Each tool call spawns a separate process; state persists across calls via a
// named --session that is closed in Close().
type agentBrowserBackend struct {
	bin     string   // executable (first token of agent_bin)
	binArgs []string // any prefix args from agent_bin (e.g. ["agent-browser"] when bin="npx")
	session string   // unique session name for this BrowserSession
	headed  bool     // true = show browser window
}

// abData is the "data" object returned by agent-browser --json.
type abData struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Text     string `json:"text"`
	HTML     string `json:"html"`
	Snapshot string `json:"snapshot"`
}

// abResp is the top-level agent-browser --json response envelope.
type abResp struct {
	Success bool   `json:"success"`
	Data    abData `json:"data"`
	Error   string `json:"error"`
}

func newAgentBrowserBackend(cfg *config.BrowserConfig) (*agentBrowserBackend, error) {
	agentBin := cfg.AgentBin
	if agentBin == "" {
		agentBin = "agent-browser"
	}
	// Support multi-word agent_bin like "npx agent-browser" or "npx --yes agent-browser".
	parts := strings.Fields(agentBin)
	return &agentBrowserBackend{
		bin:     parts[0],
		binArgs: parts[1:],
		session: fmt.Sprintf("ageage-%d", time.Now().UnixNano()),
		headed:  !cfg.Headless,
	}, nil
}

// run executes a single agent-browser command and returns the parsed data object.
func (b *agentBrowserBackend) run(timeout time.Duration, args ...string) (abData, error) {
	// Build: <bin> [binArgs...] [--headed] --session <s> --json <args...>
	cmdArgs := append([]string{}, b.binArgs...)
	if b.headed {
		cmdArgs = append(cmdArgs, "--headed")
	}
	cmdArgs = append(cmdArgs, "--session", b.session, "--json")
	cmdArgs = append(cmdArgs, args...)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, b.bin, cmdArgs...)
	// Set agent-browser's internal Playwright timeout to 3s less than ours so
	// it exits cleanly with a JSON error before Go's context kills the process.
	// Clamp to at least 1s; never let it go negative.
	playwrightMS := timeout.Milliseconds() - 3000
	if playwrightMS < 1000 {
		playwrightMS = 1000
	}
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("AGENT_BROWSER_DEFAULT_TIMEOUT=%d", playwrightMS),
	)
	// WaitDelay: after the context cancels the process, forcibly close the
	// stdout/stderr pipes. Cap at half the timeout so short timeouts don't
	// double their effective wait; minimum 500ms for a clean shutdown attempt.
	cmd.WaitDelay = max(500*time.Millisecond, min(3*time.Second, timeout/2))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// On context timeout the stdout buffer may be partial; try to parse
		// whatever was written before the pipe was force-closed.
		var resp abResp
		if json.Unmarshal(stdout.Bytes(), &resp) == nil && resp.Error != "" {
			return abData{}, fmt.Errorf("agent-browser: %s", resp.Error)
		}
		if ctx.Err() != nil {
			return abData{}, fmt.Errorf("agent-browser: timed out after %s", timeout)
		}
		if se := strings.TrimSpace(stderr.String()); se != "" {
			return abData{}, fmt.Errorf("agent-browser: %s", se)
		}
		return abData{}, fmt.Errorf("agent-browser %v: %w", args, err)
	}

	var resp abResp
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return abData{}, fmt.Errorf("agent-browser: parse response: %w (raw: %s)", err, stdout.String())
	}
	if !resp.Success {
		return abData{}, fmt.Errorf("agent-browser: %s", resp.Error)
	}
	return resp.Data, nil
}

func (b *agentBrowserBackend) Navigate(rawURL, _ string, timeout time.Duration) (string, string, error) {
	data, err := b.run(timeout, "open", rawURL)
	if err != nil {
		return "", "", err
	}
	// snapshot is the documented AI-friendly content command; it returns the
	// accessibility tree which works reliably on SPAs and auth-gated pages.
	snapData, err := b.run(timeout, "snapshot")
	if err != nil {
		return data.Title, "", fmt.Errorf("get page content: %w", err)
	}
	content := snapData.Snapshot
	if content == "" {
		content = "(page loaded but no content could be extracted)"
	}
	return data.Title, content, nil
}

func (b *agentBrowserBackend) Action(action, selector, value string, timeout time.Duration) (string, error) {
	var args []string
	switch action {
	case "click":
		args = []string{"click", selector}
	case "type", "fill":
		args = []string{"fill", selector, value}
	case "hover":
		args = []string{"hover", selector}
	case "scroll":
		dir := "down"
		if value == "up" {
			dir = "up"
		} else if value != "" && value != "down" {
			dir = value
		}
		args = []string{"scroll", dir}
	case "select":
		args = []string{"select", selector, value}
	case "press":
		args = []string{"press", value}
	case "check":
		args = []string{"check", selector}
	case "uncheck":
		args = []string{"uncheck", selector}
	default:
		return "", fmt.Errorf("unknown action %q; supported: click, type, fill, hover, scroll, select, press, check, uncheck", action)
	}
	if _, err := b.run(timeout, args...); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s done", action), nil
}

func (b *agentBrowserBackend) Content(format, selector string, timeout time.Duration) (string, error) {
	switch format {
	case "html":
		target := "body"
		if selector != "" {
			target = selector
		}
		data, err := b.run(timeout, "get", "html", target)
		if err != nil {
			return "", err
		}
		return data.HTML, nil
	case "snapshot":
		data, err := b.run(timeout, "snapshot", "-i")
		if err != nil {
			return "", err
		}
		return data.Snapshot, nil
	default: // "text"
		target := "body"
		if selector != "" {
			target = selector
		}
		data, err := b.run(timeout, "get", "text", target)
		if err != nil {
			return "", err
		}
		return data.Text, nil
	}
}

func (b *agentBrowserBackend) Close() {
	b.run(30*time.Second, "close") //nolint:errcheck
}

// ── HTML → readable text helper ──────────────────────────────────────────────

// extractReadable converts raw HTML to clean readable text using Mozilla
// Readability (same library used by web_fetch native backend).
func extractReadable(html, rawURL string) string {
	parsedURL, _ := url.Parse(rawURL)
	article, err := readability.FromReader(bytes.NewReader([]byte(html)), parsedURL)
	if err == nil && article.Node != nil {
		var sb strings.Builder
		if article.RenderText(&sb) == nil {
			if t := strings.TrimSpace(sb.String()); t != "" {
				return t
			}
		}
	}
	// Fallback: strip tags.
	stripped := htmlTagRe.ReplaceAllString(html, " ")
	return strings.Join(strings.Fields(stripped), " ")
}

// ── Tool implementations ─────────────────────────────────────────────────────

// BrowserNavigateTool opens a URL and returns the page title + readable text.
type BrowserNavigateTool struct {
	Session *BrowserSession
}

func (t *BrowserNavigateTool) Name() string { return "browser_navigate" }

func (t *BrowserNavigateTool) Description() string {
	return "Open a URL in the browser and return the page title and readable text content. " +
		"Use this for JavaScript-rendered pages that web_fetch cannot access. " +
		"Keeps the browser session alive so subsequent browser_action or browser_content calls operate on the same page."
}

func (t *BrowserNavigateTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The URL to navigate to (http:// or https://).",
			},
			"wait_until": map[string]interface{}{
				"type":        "string",
				"description": "When to consider navigation complete: \"load\" (default), \"networkidle\", \"domcontentloaded\", or \"commit\".",
			},
		},
		"required": []string{"url"},
	}
}

func (t *BrowserNavigateTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		URL       string `json:"url"`
		WaitUntil string `json:"wait_until"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if p.URL == "" {
		return "", fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
		p.URL = "https://" + p.URL
	}

	title, text, err := t.Session.navigate(p.URL, p.WaitUntil, t.Session.timeout())
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	if title != "" {
		fmt.Fprintf(&sb, "Title: %s\n\n", title)
	}
	sb.WriteString(text)
	return sb.String(), nil
}

// BrowserActionTool interacts with elements on the current page.
type BrowserActionTool struct {
	Session *BrowserSession
}

func (t *BrowserActionTool) Name() string { return "browser_action" }

func (t *BrowserActionTool) Description() string {
	return "Perform an interaction on the current browser page (click, fill a form field, hover, scroll, etc.). " +
		"Must call browser_navigate first to open a page."
}

func (t *BrowserActionTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"type":        "string",
				"description": "Action to perform: \"click\", \"type\", \"fill\", \"hover\", \"scroll\", \"select\", \"press\", \"check\", \"uncheck\".",
			},
			"selector": map[string]interface{}{
				"type":        "string",
				"description": "CSS selector or text selector for the target element (e.g. \"#submit\", \"button:has-text('Login')\").",
			},
			"value": map[string]interface{}{
				"type":        "string",
				"description": "For type/fill: text to enter. For select: option value. For press: key name (e.g. \"Enter\"). For scroll: \"up\", \"down\", or pixel amount.",
			},
		},
		"required": []string{"action"},
	}
}

func (t *BrowserActionTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Action   string `json:"action"`
		Selector string `json:"selector"`
		Value    string `json:"value"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if p.Action == "" {
		return "", fmt.Errorf("action is required")
	}
	return t.Session.action(p.Action, p.Selector, p.Value, t.Session.timeout())
}

// BrowserContentTool retrieves content from the current browser page.
type BrowserContentTool struct {
	Session *BrowserSession
}

func (t *BrowserContentTool) Name() string { return "browser_content" }

func (t *BrowserContentTool) Description() string {
	return "Return content from the current browser page in a chosen format. " +
		"Use \"text\" for readable content, \"html\" for raw markup, or \"snapshot\" for the accessibility tree (useful for understanding interactive elements)."
}

func (t *BrowserContentTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"format": map[string]interface{}{
				"type":        "string",
				"description": "Content format: \"text\" (default, readable text via Readability), \"html\" (raw HTML), or \"snapshot\" (accessibility tree JSON).",
			},
			"selector": map[string]interface{}{
				"type":        "string",
				"description": "Optional CSS selector to restrict content to a specific element. Defaults to the full page.",
			},
		},
	}
}

func (t *BrowserContentTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Format   string `json:"format"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	return t.Session.content(p.Format, p.Selector, t.Session.timeout())
}
