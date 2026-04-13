package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"ageage/config"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
)

// defaultUserAgent is a realistic browser User-Agent for web requests.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// htmlTagRe matches any HTML tag for stripping purposes.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// newRobustHTTPClient creates an HTTP client with comprehensive compatibility settings.
func newRobustHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          20,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    false,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// Follow redirects (default behavior).
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

// setStandardHeaders sets common request headers for web compatibility.
func setStandardHeaders(req *http.Request) {
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Connection", "keep-alive")
}

// =====================================================
// WebFetchTool
// =====================================================

// WebFetchTool fetches web page content via configurable backends.
type WebFetchTool struct {
	Cfg *config.WebFetchConfig
}

func (t *WebFetchTool) Name() string { return "web_fetch" }

func (t *WebFetchTool) Description() string {
	return "Fetch the content of a web page by URL. Returns the extracted text content."
}

func (t *WebFetchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"url": map[string]interface{}{
				"type":        "string",
				"description": "The URL to fetch.",
			},
		},
		"required": []string{"url"},
	}
}

func (t *WebFetchTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	rawURL := strings.TrimSpace(params.URL)
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}

	switch t.Cfg.Backend {
	case "jina":
		return t.fetchViaJina(rawURL)
	case "crawl4ai":
		return t.fetchViaCrawl4AI(rawURL)
	default: // "native"
		return t.fetchNative(rawURL)
	}
}

func (t *WebFetchTool) fetchNative(rawURL string) (string, error) {
	client := newRobustHTTPClient(30 * time.Second)

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	setStandardHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	maxChars := t.Cfg.MaxCharacters
	if maxChars <= 0 {
		maxChars = 20000
	}

	// Try Readability first — it strips navbars, sidebars, footers, ads, and
	// reconstructs only the primary article content (Mozilla Readability algorithm).
	pageURL, _ := url.Parse(rawURL)
	article, err := readability.FromReader(bytes.NewReader(body), pageURL)
	if err == nil && article.Node != nil {
		var sb strings.Builder
		if title := article.Title(); title != "" {
			sb.WriteString(title)
			sb.WriteString("\n\n")
		}
		if rerr := article.RenderText(&sb); rerr == nil && sb.Len() > 200 {
			return truncate(cleanText(sb.String()), maxChars), nil
		}
	}

	// Fallback: goquery-based extraction for pages without article-style structure.
	content, err := extractTextFromHTML(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to extract text: %w", err)
	}
	return truncate(content, maxChars), nil
}

func (t *WebFetchTool) fetchViaJina(rawURL string) (string, error) {
	jinaURL := "https://r.jina.ai/" + rawURL

	client := newRobustHTTPClient(60 * time.Second)

	req, err := http.NewRequest("GET", jinaURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create Jina request: %w", err)
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", defaultUserAgent)
	if t.Cfg.JinaAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.Cfg.JinaAPIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Jina fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("Jina HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read Jina response: %w", err)
	}

	maxChars := t.Cfg.MaxCharacters
	if maxChars <= 0 {
		maxChars = 20000
	}
	return truncate(string(body), maxChars), nil
}

func (t *WebFetchTool) fetchViaCrawl4AI(rawURL string) (string, error) {
	pythonCmd := t.Cfg.Crawl4AICmd
	if pythonCmd == "" {
		pythonCmd = "python"
	}

	pyScript := `
import asyncio
import os
import json
from crawl4ai import AsyncWebCrawler, CacheMode, CrawlerRunConfig
from crawl4ai.content_filter_strategy import PruningContentFilter
from crawl4ai.markdown_generation_strategy import DefaultMarkdownGenerator
async def main():
    url = os.environ.get("CRAWL4AI_URL")
    if not url:
        return
    pruning_filter = PruningContentFilter(
        threshold=0.45, 
        min_word_threshold=50
    )
    md_generator = DefaultMarkdownGenerator(
        content_filter=pruning_filter,
        options={
            "ignore_links": True,
            "ignore_images": True,
            "body_width": 0
        }
    )
    config = CrawlerRunConfig(
        cache_mode=CacheMode.BYPASS,
        markdown_generator=md_generator,
        word_count_threshold=10,
        page_timeout=60000
    )
    async with AsyncWebCrawler() as crawler:
        result = await crawler.arun(url=url, config=config)
        if result.success:
            md = result.markdown
            if hasattr(md, 'raw_markdown'):
                print(md.raw_markdown)
            else:
                print(md)
        else:
            print(f"Error: {result.error_message}")
asyncio.run(main())
`

	cmd := exec.Command(pythonCmd, "-c", pyScript)
	cmd.Env = append(os.Environ(), "CRAWL4AI_URL="+rawURL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Crawl4AI failed: %w\nOutput: %s", err, string(output))
	}

	maxChars := t.Cfg.MaxCharacters
	if maxChars <= 0 {
		maxChars = 20000
	}
	return truncate(string(output), maxChars), nil
}

// =====================================================
// WebSearchTool
// =====================================================

// WebSearchTool performs web searches via configurable backends.
type WebSearchTool struct {
	Cfg *config.WebSearchConfig
}

func (t *WebSearchTool) Name() string { return "web_search" }

func (t *WebSearchTool) Description() string {
	return "Search the web for information using a query. Returns search result snippets."
}

func (t *WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The search query.",
			},
		},
		"required": []string{"query"},
	}
}

func (t *WebSearchTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	query := strings.TrimSpace(params.Query)
	if query == "" {
		return "", fmt.Errorf("query cannot be empty")
	}

	switch t.Cfg.Backend {
	case "tavily":
		if t.Cfg.TavilyAPIKey == "" {
			fmt.Println("  ⚠  web_search: tavily_api_key not set, falling back to DuckDuckGo")
			return t.searchViaDuckDuckGo(query)
		}
		result, err := t.searchViaTavily(query)
		if err != nil {
			fmt.Printf("  ⚠  web_search: Tavily failed (%s), falling back to DuckDuckGo\n", err)
			return t.searchViaDuckDuckGo(query)
		}
		return result, nil
	case "brave":
		if t.Cfg.BraveAPIKey == "" {
			fmt.Println("  ⚠  web_search: brave_api_key not set, falling back to DuckDuckGo")
			return t.searchViaDuckDuckGo(query)
		}
		result, err := t.searchViaBrave(query)
		if err != nil {
			fmt.Printf("  ⚠  web_search: Brave Search failed (%s), falling back to DuckDuckGo\n", err)
			return t.searchViaDuckDuckGo(query)
		}
		return result, nil
	case "searxng":
		result, err := t.searchViaSearXNG(query)
		if err != nil {
			// Fallback to DuckDuckGo when SearXNG is unavailable or misconfigured.
			return t.searchViaDuckDuckGo(query)
		}
		return result, nil
	default: // "duckduckgo"
		return t.searchViaDuckDuckGo(query)
	}
}

func (t *WebSearchTool) isBlocked(rawURL string) bool {
	if len(t.Cfg.BlockedDomains) == 0 {
		return false
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	host := strings.ToLower(parsed.Host)
	for _, domain := range t.Cfg.BlockedDomains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" {
			continue
		}
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func (t *WebSearchTool) searchViaDuckDuckGo(query string) (string, error) {
	searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(query))

	client := newRobustHTTPClient(15 * time.Second)

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create search request: %w", err)
	}
	setStandardHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("web search failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read search results: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DuckDuckGo returned HTTP %d — may be rate-limited or blocked", resp.StatusCode)
	}

	// Parse structured search results instead of generic text extraction.
	results, err := parseDuckDuckGoResults(string(body))
	if err != nil {
		return "", fmt.Errorf("failed to parse DuckDuckGo results: %w", err)
	}

	var filtered []ddgResult
	for _, r := range results {
		if t.isBlocked(r.URL) {
			continue
		}
		filtered = append(filtered, r)
	}

	if len(filtered) == 0 {
		// Distinguish a genuine "no results" page from a scraping failure.
		// A valid DDG HTML response always contains at least one of these markers.
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "result__") && !strings.Contains(bodyStr, "no-results") {
			return "", fmt.Errorf("DuckDuckGo returned an unexpected page (possible bot-detection or page structure change); no results could be extracted")
		}
		return fmt.Sprintf("No results found for: %s", query), nil
	}

	maxResults := t.Cfg.MaxSearchResults
	if maxResults <= 0 {
		maxResults = 10
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for: %s\n\n", query)
	count := 0
	for _, r := range filtered {
		if count >= maxResults {
			break
		}
		fmt.Fprintf(&sb, "%d. %s\n", count+1, r.Title)
		if r.URL != "" {
			fmt.Fprintf(&sb, "   URL: %s\n", r.URL)
		}
		if r.Snippet != "" {
			fmt.Fprintf(&sb, "   %s\n", r.Snippet)
		}
		sb.WriteString("\n")
		count++
	}

	return sb.String(), nil
}

// ddgResult holds a single parsed DuckDuckGo result.
type ddgResult struct {
	Title   string
	URL     string
	Snippet string
}

// parseDuckDuckGoResults extracts structured results from DuckDuckGo HTML.
// This avoids extracting all the UI noise (region lists, time filters, etc.).
// 替换掉原有的 parseDuckDuckGoResults 以及那堆复杂的辅助函数
func parseDuckDuckGoResults(htmlStr string) ([]ddgResult, error) {
	var results []ddgResult
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		return nil, err
	}

	// DuckDuckGo 的结果块通常带有 .result 类
	doc.Find(".result").Each(func(i int, s *goquery.Selection) {
		titleNode := s.Find(".result__a").First()
		snippetNode := s.Find(".result__snippet").First()

		title := cleanInlineHTML(titleNode.Text())
		rawURL, _ := titleNode.Attr("href")
		snippet := cleanInlineHTML(snippetNode.Text())

		if title != "" && rawURL != "" {
			results = append(results, ddgResult{
				Title:   title,
				URL:     cleanDDGUrl(rawURL),
				Snippet: snippet,
			})
		}
	})

	return results, nil
}

// cleanDDGUrl extracts the actual URL from DuckDuckGo's redirect wrapper.
func cleanDDGUrl(rawURL string) string {
	if rawURL == "" {
		return ""
	}

	// DuckDuckGo wraps URLs: //duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com...
	if strings.Contains(rawURL, "uddg=") {
		parsed, err := url.Parse(rawURL)
		if err == nil {
			uddg := parsed.Query().Get("uddg")
			if uddg != "" {
				return uddg
			}
		}
	}

	// Handle protocol-relative URLs.
	if strings.HasPrefix(rawURL, "//") {
		return "https:" + rawURL
	}

	return rawURL
}

// cleanInlineHTML removes inline HTML tags and decodes common entities.
func cleanInlineHTML(s string) string {
	// Remove HTML tags (<b>, <span>, etc.).
	s = htmlTagRe.ReplaceAllString(s, "")

	// Decode common HTML entities.
	replacer := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
		"&#x27;", "'",
		"&nbsp;", " ",
		"&#x2F;", "/",
	)
	s = replacer.Replace(s)

	return strings.TrimSpace(s)
}

func (t *WebSearchTool) searchViaTavily(query string) (string, error) {
	maxResults := t.Cfg.MaxSearchResults
	if maxResults <= 0 {
		maxResults = 10
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"api_key":      t.Cfg.TavilyAPIKey,
		"query":        query,
		"max_results":  maxResults,
		"search_depth": "basic",
	})
	if err != nil {
		return "", fmt.Errorf("failed to build Tavily request: %w", err)
	}

	client := newRobustHTTPClient(15 * time.Second)
	req, err := http.NewRequest("POST", "https://api.tavily.com/search", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("failed to create Tavily request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Tavily request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read Tavily response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Surface the error message from the API body when available.
		var apiErr struct {
			Detail string `json:"detail"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(body, &apiErr) == nil {
			msg := apiErr.Detail
			if msg == "" {
				msg = apiErr.Error
			}
			if msg != "" {
				return "", fmt.Errorf("Tavily HTTP %d: %s", resp.StatusCode, msg)
			}
		}
		return "", fmt.Errorf("Tavily HTTP %d", resp.StatusCode)
	}

	var tavilyResp struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &tavilyResp); err != nil {
		return "", fmt.Errorf("failed to parse Tavily response: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for: %s\n\n", query)
	count := 0
	for _, r := range tavilyResp.Results {
		if count >= maxResults {
			break
		}
		if t.isBlocked(r.URL) {
			continue
		}
		fmt.Fprintf(&sb, "%d. %s\n", count+1, r.Title)
		fmt.Fprintf(&sb, "   URL: %s\n", r.URL)
		if r.Content != "" {
			fmt.Fprintf(&sb, "   %s\n", r.Content)
		}
		sb.WriteString("\n")
		count++
	}

	if count == 0 {
		return fmt.Sprintf("No results found for: %s", query), nil
	}
	return sb.String(), nil
}

func (t *WebSearchTool) searchViaBrave(query string) (string, error) {
	maxResults := t.Cfg.MaxSearchResults
	if maxResults <= 0 {
		maxResults = 10
	}
	if maxResults > 20 {
		maxResults = 20 // Brave API cap per request
	}

	searchURL := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d",
		url.QueryEscape(query), maxResults)

	client := newRobustHTTPClient(15 * time.Second)
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create Brave request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", t.Cfg.BraveAPIKey)
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Brave Search request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read Brave response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			return "", fmt.Errorf("Brave Search HTTP %d: %s", resp.StatusCode, apiErr.Message)
		}
		return "", fmt.Errorf("Brave Search HTTP %d", resp.StatusCode)
	}

	var braveResp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &braveResp); err != nil {
		return "", fmt.Errorf("failed to parse Brave response: %w", err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for: %s\n\n", query)
	count := 0
	for _, r := range braveResp.Web.Results {
		if count >= maxResults {
			break
		}
		if t.isBlocked(r.URL) {
			continue
		}
		fmt.Fprintf(&sb, "%d. %s\n", count+1, r.Title)
		fmt.Fprintf(&sb, "   URL: %s\n", r.URL)
		if r.Description != "" {
			fmt.Fprintf(&sb, "   %s\n", r.Description)
		}
		sb.WriteString("\n")
		count++
	}

	if count == 0 {
		return fmt.Sprintf("No results found for: %s", query), nil
	}
	return sb.String(), nil
}

func (t *WebSearchTool) searchViaSearXNG(query string) (string, error) {
	baseURL := strings.TrimRight(t.Cfg.SearXNGURL, "/")
	if baseURL == "" {
		return "", fmt.Errorf("SearXNG URL not configured. Set web.searxng_url in config.toml")
	}

	searchURL := fmt.Sprintf("%s/search?q=%s&format=json", baseURL, url.QueryEscape(query))

	client := newRobustHTTPClient(15 * time.Second)

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create SearXNG request: %w", err)
	}
	setStandardHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("SearXNG search failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read SearXNG results: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SearXNG HTTP %d: %s", resp.StatusCode, string(body))
	}

	var searxResp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}

	if err := json.Unmarshal(body, &searxResp); err != nil {
		return "", fmt.Errorf("failed to parse SearXNG response: %w", err)
	}

	maxResults := t.Cfg.MaxSearchResults
	if maxResults <= 0 {
		maxResults = 10
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for: %s\n\n", query)
	count := 0
	for _, r := range searxResp.Results {
		if count >= maxResults {
			break
		}
		if t.isBlocked(r.URL) {
			continue
		}
		fmt.Fprintf(&sb, "%d. %s\n", count+1, r.Title)
		fmt.Fprintf(&sb, "   URL: %s\n", r.URL)
		if r.Content != "" {
			fmt.Fprintf(&sb, "   %s\n", r.Content)
		}
		sb.WriteString("\n")
		count++
	}

	if count == 0 {
		return fmt.Sprintf("No results found for: %s", query), nil
	}

	return sb.String(), nil
}

// =====================================================
// Shared utilities
// =====================================================

// extractTextFromHTML robustly extracts the main content from an HTML stream.
func extractTextFromHTML(r io.Reader) (string, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return "", err
	}

	// 1. Remove noise elements.
	doc.Find("script, style, noscript, nav, footer, header, aside, iframe, svg, img, video, canvas, link, meta, form, .ads, .sidebar").Remove()

	// 2. Look for the "main" content container.
	// We try common selectors in order of specificity.
	var contentNode *goquery.Selection
	selectors := []string{
		"article",
		"main",
		"[role='main']",
		".content",
		".main",
		"#content",
		"#main",
		".post-content",
		".article-content",
		".entry-content",
	}

	for _, selector := range selectors {
		if node := doc.Find(selector); node.Length() > 0 {
			// Choose the one with the most text/children if multiple matches.
			if contentNode == nil || len(node.Text()) > len(contentNode.Text()) {
				contentNode = node
			}
		}
	}

	// 3. Fallback to body if no container found.
	if contentNode == nil || contentNode.Length() == 0 {
		contentNode = doc.Find("body")
	}

	// 4. Extract and clean text.
	var sb strings.Builder
	contentNode.Find("p, h1, h2, h3, h4, h5, h6, li, pre, div, td").Each(func(i int, s *goquery.Selection) {
		text := strings.TrimSpace(s.Text())
		if len(text) > 0 {
			sb.WriteString(text)
			sb.WriteString("\n\n")
		}
	})

	// 5. If specific tags didn't yield much, just get the whole thing.
	if sb.Len() < 100 {
		return cleanText(contentNode.Text()), nil
	}

	return cleanText(sb.String()), nil
}

// cleanText removes excessive whitespace and empty lines.
func cleanText(text string) string {
	lines := strings.Split(text, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	// Deduplicate consecutive identical lines (often from CSS/JS artifacts)
	var final []string
	for i, line := range cleaned {
		if i > 0 && line == cleaned[i-1] {
			continue
		}
		final = append(final, line)
	}
	return strings.Join(final, "\n")
}

// truncate limits content to maxLen characters using runes.
func truncate(content string, maxLen int) string {
	runes := []rune(content)
	if len(runes) > maxLen {
		return string(runes[:maxLen]) + "\n... (content truncated)"
	}
	return content
}
