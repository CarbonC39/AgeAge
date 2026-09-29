package tools

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ageage/config"
)

type staticIPResolver struct {
	ips []net.IP
}

type testContextKey string

func (r staticIPResolver) LookupIP(context.Context, string, string) ([]net.IP, error) {
	return r.ips, nil
}

func TestNetworkPolicyBlocksSpecialUseIPv4AndIPv6(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"100.64.0.1",
		"198.18.0.1",
		"[::1]",
		"[fc00::1]",
		"[fe80::1]",
		"[ff02::1]",
		"[::]",
	}
	for _, host := range blocked {
		t.Run(host, func(t *testing.T) {
			if err := validateNetworkURL(context.Background(), "http://"+host+"/"); err == nil {
				t.Fatalf("expected %s to be blocked", host)
			}
		})
	}
}

func TestNetworkPolicyAllowsPublicIPLiteral(t *testing.T) {
	if err := validateNetworkURL(context.Background(), "https://1.1.1.1/"); err != nil {
		t.Fatalf("public address was blocked: %v", err)
	}
}

func TestNetworkPolicyRejectsNonHTTPURLsAndUserinfo(t *testing.T) {
	for _, rawURL := range []string{
		"file:///etc/passwd",
		"ftp://example.com/file",
		"https://user:pass@example.com/",
	} {
		t.Run(rawURL, func(t *testing.T) {
			if err := validateNetworkURL(context.Background(), rawURL); err == nil {
				t.Fatalf("expected URL to be rejected: %s", rawURL)
			}
		})
	}
}

func TestNetworkPolicyAllowlistAndPrivateOverride(t *testing.T) {
	private := configuredNetworkPolicy(true, []string{"127.0.0.1"})
	if _, err := private.validateURL(context.Background(), "http://127.0.0.1/"); err != nil {
		t.Fatalf("allow_private policy rejected loopback: %v", err)
	}
	allowlist := configuredNetworkPolicy(false, []string{"example.com"})
	if _, err := allowlist.validateURL(context.Background(), "https://1.1.1.1/"); err == nil {
		t.Fatal("expected URL outside allowlist to be rejected")
	}
}

func TestNetworkPolicyRejectsAnyBlockedAddressInMixedDNSAnswer(t *testing.T) {
	policy := &networkPolicy{
		resolver: staticIPResolver{ips: []net.IP{
			net.ParseIP("1.1.1.1"),
			net.ParseIP("127.0.0.1"),
		}},
	}
	if err := policy.validateHost(context.Background(), "mixed.example"); err == nil {
		t.Fatal("expected mixed public/private DNS answer to be rejected")
	}
}

func TestNetworkPolicyRedirectRejectsPrivateTarget(t *testing.T) {
	client := newRobustHTTPClient(5 * time.Second)
	redirect := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if err := client.CheckRedirect(redirect, nil); err == nil {
		t.Fatal("expected redirect to loopback to be rejected")
	}
}

func TestWebFetchCancellationReachesHTTPRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	tool := &WebFetchTool{Cfg: &config.WebFetchConfig{Backend: "native", AllowPrivate: true}}
	result := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, []byte(`{"url":"`+server.URL+`"}`))
		result <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("web fetch did not stop after context cancellation")
	}
}

func TestBrowserNavigateHonorsCanceledContextBeforeOpeningSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := &BrowserNavigateTool{Session: NewBrowserSession(nil)}
	_, err := tool.Execute(ctx, []byte(`{"url":"https://example.com"}`))
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestPlaywrightBackendNetworkContextUsesLatestToolCall(t *testing.T) {
	backend := &playwrightBackend{}
	first := context.WithValue(context.Background(), testContextKey("run"), "first")
	second := context.WithValue(context.Background(), testContextKey("run"), "second")
	backend.setNetworkContext(first)
	backend.setNetworkContext(second)
	if got := backend.networkContext().Value(testContextKey("run")); got != "second" {
		t.Fatalf("route context = %v, want latest tool context", got)
	}
}

func TestAgentBrowserQuarantinesAfterBlockedActionURL(t *testing.T) {
	binary := fakeAgentBrowser(t, "http://127.0.0.1/")
	backend, err := newAgentBrowserBackend(&config.BrowserConfig{AgentBin: binary}, defaultNetworkPolicy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Action(context.Background(), "click", "#submit", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked action URL, got %v", err)
	}
	if _, err := backend.Content(context.Background(), "text", "", time.Second); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected quarantined session, got %v", err)
	}
}

func TestAgentBrowserQuarantinesBeforeBlockedContentOutput(t *testing.T) {
	binary := fakeAgentBrowser(t, "http://127.0.0.1/")
	backend, err := newAgentBrowserBackend(&config.BrowserConfig{AgentBin: binary}, defaultNetworkPolicy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Content(context.Background(), "text", "", time.Second)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked content URL, got %v", err)
	}
}

func fakeAgentBrowser(t *testing.T, currentURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-browser")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"success\":true,\"data\":{\"url\":\"" + currentURL + "\",\"text\":\"secret\"}}'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
