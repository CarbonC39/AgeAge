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
	Navigate(ctx context.Context, url, waitUntil string, timeout time.Duration) (title, text string, err error)
	// Action performs a single interaction on the page.
	Action(ctx context.Context, action, selector, value string, timeout time.Duration) (string, error)
	// Content returns the current page content in the requested format.
	Content(ctx context.Context, format, selector string, timeout time.Duration) (string, error)
	// Close releases all browser resources.
	Close()
}

// contextAwareBackend lets a session update the context used by asynchronous
// browser request routes for each tool call.
type contextAwareBackend interface {
	setNetworkContext(context.Context)
}

// ── BrowserSession ───────────────────────────────────────────────────────────

// BrowserSession is a single browser lifecycle shared by all three browser_*
// tools for one agent Run() call. It is created lazily on first use and
// closed by the agent's deferred cleanup block.
type BrowserSession struct {
	cfg     *config.BrowserConfig
	policy  *networkPolicy
	backend browserBackend
	mu      sync.Mutex
	once    sync.Once
	openErr error
}

// NewBrowserSession creates a session bound to the given config.
// The browser is NOT opened yet; it opens on the first tool call.
func NewBrowserSession(cfg *config.BrowserConfig) *BrowserSession {
	policy := defaultNetworkPolicy
	if cfg != nil {
		policy = configuredNetworkPolicy(cfg.AllowPrivate, cfg.AllowedDomains)
	}
	return &BrowserSession{cfg: cfg, policy: policy}
}

// open initialises the backend exactly once. Subsequent calls are no-ops.
func (s *BrowserSession) open(ctx context.Context) error {
	s.once.Do(func() {
		switch s.cfg.Backend {
		case "agent-browser":
			b, err := newAgentBrowserBackend(s.cfg, s.policy)
			if err != nil {
				s.openErr = err
				return
			}
			s.backend = b
		default: // "playwright"
			b, err := newPlaywrightBackend(s.cfg, s.policy)
			if err != nil {
				s.openErr = err
				return
			}
			s.backend = b
		}
	})
	return s.openErr
}

// Close shuts down the browser and resets the session so registered tools can
// lazily open a fresh backend on a later Agent.Run call. Safe to call multiple
// times.
func (s *BrowserSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backend != nil {
		s.backend.Close()
		s.backend = nil
	}
	s.openErr = nil
	s.once = sync.Once{}
}

func (s *BrowserSession) navigate(ctx context.Context, url, waitUntil string, timeout time.Duration) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx); err != nil {
		return "", "", err
	}
	s.setNetworkContext(ctx)
	return s.backend.Navigate(ctx, url, waitUntil, timeout)
}

func (s *BrowserSession) action(ctx context.Context, action, selector, value string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx); err != nil {
		return "", err
	}
	s.setNetworkContext(ctx)
	return s.backend.Action(ctx, action, selector, value, timeout)
}

func (s *BrowserSession) content(ctx context.Context, format, selector string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx); err != nil {
		return "", err
	}
	s.setNetworkContext(ctx)
	return s.backend.Content(ctx, format, selector, timeout)
}

func (s *BrowserSession) setNetworkContext(ctx context.Context) {
	if backend, ok := s.backend.(contextAwareBackend); ok {
		backend.setNetworkContext(ctx)
	}
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
	policy      *networkPolicy
	contextMu   sync.RWMutex
	activeCtx   context.Context
}

func (b *playwrightBackend) setNetworkContext(ctx context.Context) {
	b.contextMu.Lock()
	b.activeCtx = ctx
	b.contextMu.Unlock()
}

func (b *playwrightBackend) networkContext() context.Context {
	b.contextMu.RLock()
	ctx := b.activeCtx
	b.contextMu.RUnlock()
	if ctx == nil {
		return context.TODO()
	}
	return ctx
}

// watchPlaywrightContext interrupts an in-flight Playwright operation when
// the caller cancels its context. Playwright's Go API exposes per-operation
// timeouts but no context parameter, so closing the page is the only reliable
// way to interrupt a navigation or locator wait immediately.
func watchPlaywrightContext(ctx context.Context, page playwright.Page) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = page.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func newPlaywrightBackend(cfg *config.BrowserConfig, policy *networkPolicy) (*playwrightBackend, error) {
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

	var browserContext playwright.BrowserContext
	switch cfg.BrowserType {
	case "firefox":
		browserContext, err = pw.Firefox.LaunchPersistentContext(userDataDir, launchOpts)
	case "webkit":
		browserContext, err = pw.WebKit.LaunchPersistentContext(userDataDir, launchOpts)
	default: // "chromium"
		browserContext, err = pw.Chromium.LaunchPersistentContext(userDataDir, launchOpts)
	}
	if err != nil {
		os.RemoveAll(userDataDir) //nolint:errcheck
		pw.Stop()                 //nolint:errcheck
		return nil, fmt.Errorf("playwright: failed to launch browser context: %w", err)
	}

	pages := browserContext.Pages()
	var page playwright.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, err = browserContext.NewPage()
		if err != nil {
			browserContext.Close()    //nolint:errcheck
			os.RemoveAll(userDataDir) //nolint:errcheck
			pw.Stop()                 //nolint:errcheck
			return nil, fmt.Errorf("playwright: failed to create page: %w", err)
		}
	}
	backend := &playwrightBackend{pw: pw, context: browserContext, page: page, userDataDir: userDataDir, policy: policy}
	// Intercept every browser request, including redirects and subresources.
	// The initial navigation is checked by BrowserNavigateTool as well; this
	// route closes the gap where a public page embeds a private URL.
	if err := browserContext.Route("**/*", func(route playwright.Route) {
		rawURL := route.Request().URL()
		if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
			if _, err := policy.validateURL(backend.networkContext(), rawURL); err != nil {
				_ = route.Abort("blockedbyclient")
				return
			}
		}
		_ = route.Continue()
	}); err != nil {
		page.Close()              //nolint:errcheck
		browserContext.Close()    //nolint:errcheck
		os.RemoveAll(userDataDir) //nolint:errcheck
		pw.Stop()                 //nolint:errcheck
		return nil, fmt.Errorf("playwright: failed to install network policy: %w", err)
	}

	return backend, nil
}

func (b *playwrightBackend) Navigate(ctx context.Context, rawURL, waitUntil string, timeout time.Duration) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	stopWatch := watchPlaywrightContext(ctx, b.page)
	defer stopWatch()
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
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
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

func (b *playwrightBackend) Action(ctx context.Context, action, selector, value string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stopWatch := watchPlaywrightContext(ctx, b.page)
	defer stopWatch()
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

func (b *playwrightBackend) Content(ctx context.Context, format, selector string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stopWatch := watchPlaywrightContext(ctx, b.page)
	defer stopWatch()
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
	policy  *networkPolicy
	stateMu sync.Mutex
	closed  bool
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

func newAgentBrowserBackend(cfg *config.BrowserConfig, policy *networkPolicy) (*agentBrowserBackend, error) {
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
		policy:  policy,
	}, nil
}

// run executes a single agent-browser command and returns the parsed data object.
func (b *agentBrowserBackend) run(parent context.Context, timeout time.Duration, args ...string) (abData, error) {
	b.stateMu.Lock()
	closed := b.closed
	b.stateMu.Unlock()
	if closed && (len(args) == 0 || args[0] != "close") {
		return abData{}, fmt.Errorf("agent-browser session is closed")
	}
	// Build: <bin> [binArgs...] [--headed] --session <s> --json <args...>
	cmdArgs := append([]string{}, b.binArgs...)
	if b.headed {
		cmdArgs = append(cmdArgs, "--headed")
	}
	cmdArgs = append(cmdArgs, "--session", b.session, "--json")
	cmdArgs = append(cmdArgs, args...)

	ctx, cancel := context.WithTimeout(parent, timeout)
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

// validateCurrentURL asks the CLI for the URL that is actually loaded. This
// catches navigations caused by clicks/forms, which are not visible in the
// original action arguments.
func (b *agentBrowserBackend) validateCurrentURL(ctx context.Context, timeout time.Duration) error {
	data, err := b.run(ctx, timeout, "get", "url")
	if err != nil {
		return err
	}
	if data.URL == "" {
		return fmt.Errorf("agent-browser returned an empty current URL")
	}
	if b.policy == nil {
		return nil
	}
	if _, err := b.policy.validateURL(ctx, data.URL); err != nil {
		return fmt.Errorf("current browser URL is blocked: %w", err)
	}
	return nil
}

// quarantine closes a session after a policy violation. The closed flag is
// set before invoking the CLI so later tool calls cannot read from the page
// even if the external process fails to close cleanly.
func (b *agentBrowserBackend) quarantine() {
	b.stateMu.Lock()
	if b.closed {
		b.stateMu.Unlock()
		return
	}
	b.closed = true
	b.stateMu.Unlock()
	b.run(context.TODO(), 30*time.Second, "close") //nolint:errcheck
}

func (b *agentBrowserBackend) Navigate(ctx context.Context, rawURL, _ string, timeout time.Duration) (string, string, error) {
	data, err := b.run(ctx, timeout, "open", rawURL)
	if err != nil {
		return "", "", err
	}
	if err := b.validateCurrentURL(ctx, timeout); err != nil {
		b.quarantine()
		return "", "", err
	}
	// snapshot is the documented AI-friendly content command; it returns the
	// accessibility tree which works reliably on SPAs and auth-gated pages.
	snapData, err := b.run(ctx, timeout, "snapshot")
	if err != nil {
		return data.Title, "", fmt.Errorf("get page content: %w", err)
	}
	// Recheck after reading: a page can navigate asynchronously between the
	// pre-snapshot check and the CLI response.
	if err := b.validateCurrentURL(ctx, timeout); err != nil {
		b.quarantine()
		return "", "", err
	}
	content := snapData.Snapshot
	if content == "" {
		content = "(page loaded but no content could be extracted)"
	}
	return data.Title, content, nil
}

func (b *agentBrowserBackend) Action(ctx context.Context, action, selector, value string, timeout time.Duration) (string, error) {
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
	if _, err := b.run(ctx, timeout, args...); err != nil {
		return "", err
	}
	if err := b.validateCurrentURL(ctx, timeout); err != nil {
		b.quarantine()
		return "", err
	}
	return fmt.Sprintf("%s done", action), nil
}

func (b *agentBrowserBackend) Content(ctx context.Context, format, selector string, timeout time.Duration) (string, error) {
	if err := b.validateCurrentURL(ctx, timeout); err != nil {
		b.quarantine()
		return "", err
	}
	validateAfterContent := func(content string) (string, error) {
		if err := b.validateCurrentURL(ctx, timeout); err != nil {
			b.quarantine()
			return "", err
		}
		return content, nil
	}
	switch format {
	case "html":
		target := "body"
		if selector != "" {
			target = selector
		}
		data, err := b.run(ctx, timeout, "get", "html", target)
		if err != nil {
			return "", err
		}
		return validateAfterContent(data.HTML)
	case "snapshot":
		data, err := b.run(ctx, timeout, "snapshot", "-i")
		if err != nil {
			return "", err
		}
		return validateAfterContent(data.Snapshot)
	default: // "text"
		target := "body"
		if selector != "" {
			target = selector
		}
		data, err := b.run(ctx, timeout, "get", "text", target)
		if err != nil {
			return "", err
		}
		return validateAfterContent(data.Text)
	}
}

func (b *agentBrowserBackend) Close() {
	b.quarantine()
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

func (t *BrowserNavigateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
	policy := defaultNetworkPolicy
	if t.Session != nil && t.Session.policy != nil {
		policy = t.Session.policy
	}
	if _, err := policy.validateURL(ctx, p.URL); err != nil {
		return "", fmt.Errorf("blocked URL: %w", err)
	}

	title, text, err := t.Session.navigate(ctx, p.URL, p.WaitUntil, t.Session.timeout())
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

func (t *BrowserActionTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
	return t.Session.action(ctx, p.Action, p.Selector, p.Value, t.Session.timeout())
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

func (t *BrowserContentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Format   string `json:"format"`
		Selector string `json:"selector"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	return t.Session.content(ctx, p.Format, p.Selector, t.Session.timeout())
}
