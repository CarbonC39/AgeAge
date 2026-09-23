package tools

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// networkPolicy is the default egress policy used by tools which fetch URLs.
// It deliberately has no allow-list: public HTTP(S) is allowed, while local,
// private, link-local and otherwise non-routable destinations are denied.
//
// The IP check is performed both before a request and from the transport's
// DialContext. The latter is important because DNS answers can change between
// validation and connection (DNS rebinding).
type networkPolicy struct {
	resolver       ipResolver
	allowPrivate   bool
	allowedDomains []string
}

type ipResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

var defaultNetworkPolicy = &networkPolicy{resolver: net.DefaultResolver}

func (p *networkPolicy) validateURL(ctx context.Context, rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q (only http and https are allowed)", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("URL has no host")
	}
	if u.User != nil {
		return nil, fmt.Errorf("URL userinfo is not allowed")
	}
	// Zone identifiers are not needed for public HTTP(S), and accepting them
	// makes it easier to smuggle a link-local IPv6 address through parsing.
	if strings.Contains(u.Hostname(), "%") {
		return nil, fmt.Errorf("IPv6 zone identifiers are not allowed")
	}
	if !p.domainAllowed(u.Hostname()) {
		return nil, fmt.Errorf("destination host %q is not in the network allowlist", u.Hostname())
	}
	if err := p.validateHost(ctx, u.Hostname()); err != nil {
		return nil, err
	}
	return u, nil
}

func (p *networkPolicy) validateHost(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("empty URL host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !p.allowPrivate && isBlockedIP(ip) {
			return fmt.Errorf("destination %q is not allowed", host)
		}
		return nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ips, err := p.resolver.LookupIP(lookupCtx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve %q returned no addresses", host)
	}
	for _, ip := range ips {
		if !p.allowPrivate && isBlockedIP(ip) {
			return fmt.Errorf("destination %q resolves to a blocked address", host)
		}
	}
	return nil
}

func (p *networkPolicy) domainAllowed(host string) bool {
	if len(p.allowedDomains) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, allowed := range p.allowedDomains {
		allowed = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(allowed, ".")))
		if allowed != "" && (host == allowed || strings.HasSuffix(host, "."+allowed)) {
			return true
		}
	}
	return false
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// Include RFC 1918, loopback, link-local, unspecified, multicast and
		// other special-use ranges commonly reachable only from the host.
		return v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() ||
			v4.IsLinkLocalMulticast() || v4.IsUnspecified() || v4.IsMulticast() ||
			v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 || // RFC 6598
			v4[0] == 198 && v4[1] >= 18 && v4[1] <= 19 // RFC 2544 benchmark
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// dialContext resolves and checks the host on every connection attempt, then
// dials the checked IP directly. This closes the DNS-rebinding gap in which a
// hostname first resolves publicly and subsequently resolves to localhost.
func (p *networkPolicy) dialContext(ctx context.Context, d net.Dialer, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host = address
		port = ""
	}
	if strings.Contains(host, "%") {
		return nil, fmt.Errorf("IPv6 zone identifiers are not allowed")
	}
	if !p.domainAllowed(host) {
		return nil, fmt.Errorf("destination host %q is not in the network allowlist", host)
	}
	ips, err := p.resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	var lastErr error
	for _, ip := range ips {
		if !p.allowPrivate && isBlockedIP(ip) {
			return nil, fmt.Errorf("destination %q resolves to a blocked address", host)
		}
		target := ip.String()
		if port != "" {
			target = net.JoinHostPort(target, port)
		}
		conn, dialErr := d.DialContext(ctx, network, target)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("resolve %q returned no addresses", host)
	}
	return nil, lastErr
}

func configuredNetworkPolicy(allowPrivate bool, allowedDomains []string) *networkPolicy {
	return &networkPolicy{
		resolver:       net.DefaultResolver,
		allowPrivate:   allowPrivate,
		allowedDomains: append([]string(nil), allowedDomains...),
	}
}

func validateNetworkURL(ctx context.Context, rawURL string) error {
	_, err := defaultNetworkPolicy.validateURL(ctx, rawURL)
	return err
}
