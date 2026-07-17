package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrRedirectBlocked = errors.New("relay upstream redirects are disabled")

var blockedRelayPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("fe80::/10"),
}

type hostRule struct {
	host string
	port string
}

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// EndpointPolicy validates relay endpoints and protects connections against DNS rebinding.
type EndpointPolicy struct {
	allowlist []hostRule
	resolver  ipResolver
	dial      func(context.Context, string, string) (net.Conn, error)
}

// NewEndpointPolicy creates a policy from host or host:port allowlist entries.
func NewEndpointPolicy(entries []string) (*EndpointPolicy, error) {
	rules := make([]hostRule, 0, len(entries))
	for _, entry := range entries {
		rule, err := parseHostRule(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid RELAY_PRIVATE_HOST_ALLOWLIST entry %q: %w", entry, err)
		}
		rules = append(rules, rule)
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &EndpointPolicy{allowlist: rules, resolver: net.DefaultResolver, dial: dialer.DialContext}, nil
}

// ValidateEndpoint performs the URL checks used before saving or sending a relay request.
func (p *EndpointPolicy) ValidateEndpoint(endpoint string) error {
	u, err := url.ParseRequestURI(strings.TrimSpace(endpoint))
	if err != nil || u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		return errors.New("endpoint must be an absolute HTTP(S) URL")
	}
	if u.User != nil {
		return errors.New("endpoint must not contain user credentials")
	}
	if u.Fragment != "" {
		return errors.New("endpoint must not contain a fragment")
	}
	if err := validateURLPort(u); err != nil {
		return err
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if p != nil && p.isAllowed(u.Hostname(), u.Port()) {
			return nil
		}
		return errors.New("HTTP endpoint host is not listed in RELAY_PRIVATE_HOST_ALLOWLIST; public endpoints must use HTTPS")
	default:
		return errors.New("endpoint scheme must be HTTP or HTTPS")
	}
}

func (p *EndpointPolicy) httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           p.dialContext,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirectBlocked
		},
	}
}

func (p *EndpointPolicy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid relay upstream address: %w", err)
	}
	addresses, err := p.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve relay upstream %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("relay upstream %q resolved to no addresses", host)
	}
	allowed := p.isAllowed(host, port)
	for _, address := range addresses {
		if !allowed && isBlockedIP(address.IP) {
			return nil, fmt.Errorf("relay upstream %q resolved to blocked address %s", host, address.IP)
		}
	}
	return p.dial(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
}

func (p *EndpointPolicy) isAllowed(host, port string) bool {
	host = normalizeHost(host)
	for _, rule := range p.allowlist {
		if rule.host == host && (rule.port == "" || rule.port == port) {
			return true
		}
	}
	return false
}

func parseHostRule(raw string) (hostRule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return hostRule{}, errors.New("entry is empty")
	}
	if strings.ContainsAny(raw, "/?#@ 	\r\n") {
		return hostRule{}, errors.New("entry must contain only a host and optional port")
	}
	host, port := raw, ""
	if strings.HasPrefix(raw, "[") || strings.Count(raw, ":") == 1 {
		if parsedHost, parsedPort, err := net.SplitHostPort(raw); err == nil {
			host, port = parsedHost, parsedPort
		} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			host = strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
		} else if strings.Count(raw, ":") == 1 {
			return hostRule{}, errors.New("port is invalid")
		}
	}
	host = normalizeHost(host)
	if host == "" || (strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return hostRule{}, errors.New("host is invalid")
	}
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return hostRule{}, errors.New("port must be between 1 and 65535")
		}
		port = strconv.Itoa(value)
	}
	return hostRule{host: host, port: port}, nil
}

func validateURLPort(u *url.URL) error {
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return errors.New("endpoint port is invalid")
		}
		return nil
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return errors.New("endpoint port must be between 1 and 65535")
	}
	return nil
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

func isBlockedIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return true
	}
	for _, prefix := range blockedRelayPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}
