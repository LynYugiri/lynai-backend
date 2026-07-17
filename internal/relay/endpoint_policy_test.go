package relay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

type staticResolver map[string][]net.IPAddr

func (r staticResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addresses, ok := r[host]
	if !ok {
		return nil, errors.New("host not found")
	}
	return addresses, nil
}

func TestEndpointPolicyValidateEndpoint(t *testing.T) {
	policy, err := NewEndpointPolicy([]string{"localhost:11434", "ollama.internal"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		endpoint string
		wantErr  string
	}{
		{name: "public HTTPS", endpoint: "https://api.example.com/v1"},
		{name: "allowlisted HTTP port", endpoint: "http://LOCALHOST:11434"},
		{name: "allowlisted HTTP any port", endpoint: "http://ollama.internal:8080"},
		{name: "public HTTP", endpoint: "http://api.example.com/v1", wantErr: "must use HTTPS"},
		{name: "wrong allowlisted port", endpoint: "http://localhost:8080", wantErr: "ALLOWLIST"},
		{name: "credentials", endpoint: "https://user:pass@example.com", wantErr: "credentials"},
		{name: "invalid scheme", endpoint: "ftp://example.com", wantErr: "HTTP or HTTPS"},
		{name: "relative URL", endpoint: "/v1", wantErr: "absolute"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := policy.ValidateEndpoint(test.endpoint)
			if test.wantErr == "" && err != nil {
				t.Fatalf("ValidateEndpoint() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("ValidateEndpoint() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestEndpointPolicyDialRejectsBlockedResolvedAddress(t *testing.T) {
	policy, err := NewEndpointPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	policy.resolver = staticResolver{
		"private.example": {{IP: net.ParseIP("10.0.0.2")}},
		"mixed.example":   {{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}},
	}
	policy.dial = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dial must not be called for blocked addresses")
		return nil, nil
	}
	for _, address := range []string{"private.example:443", "mixed.example:443"} {
		if _, err := policy.dialContext(context.Background(), "tcp", address); err == nil || !strings.Contains(err.Error(), "blocked address") {
			t.Fatalf("dialContext(%q) error = %v, want blocked address", address, err)
		}
	}
}

func TestEndpointPolicyPublicUnicastClassification(t *testing.T) {
	tests := []struct {
		address string
		blocked bool
	}{
		{address: "8.8.8.8"},
		{address: "2606:4700:4700::1111"},
		{address: "0.1.2.3", blocked: true},
		{address: "100.64.0.1", blocked: true},
		{address: "192.0.2.1", blocked: true},
		{address: "198.18.0.1", blocked: true},
		{address: "203.0.113.1", blocked: true},
		{address: "240.0.0.1", blocked: true},
		{address: "64:ff9b::808:808", blocked: true},
		{address: "100::1", blocked: true},
		{address: "2001:2::1", blocked: true},
		{address: "2001:db8::1", blocked: true},
		{address: "2002:0808:0808::1", blocked: true},
		{address: "2620:4f:8000::1", blocked: true},
		{address: "3fff::1", blocked: true},
		{address: "5f00::1", blocked: true},
		{address: "fc00::1", blocked: true},
		{address: "fec0::1", blocked: true},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			if got := isBlockedIP(net.ParseIP(test.address)); got != test.blocked {
				t.Fatalf("isBlockedIP(%q) = %t, want %t", test.address, got, test.blocked)
			}
		})
	}
}

func TestEndpointPolicyDialAllowsExplicitPrivateHost(t *testing.T) {
	policy, err := NewEndpointPolicy([]string{"localhost:11434"})
	if err != nil {
		t.Fatal(err)
	}
	policy.resolver = staticResolver{"localhost": {{IP: net.ParseIP("127.0.0.1")}}}
	var dialed string
	policy.dial = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errors.New("test dial")
	}
	_, err = policy.dialContext(context.Background(), "tcp", "localhost:11434")
	if err == nil || err.Error() != "test dial" {
		t.Fatalf("dialContext() error = %v", err)
	}
	if dialed != "127.0.0.1:11434" {
		t.Fatalf("dialed address = %q", dialed)
	}
}

func TestEndpointPolicyAllowlistIsExact(t *testing.T) {
	policy, err := NewEndpointPolicy([]string{"private.example:443"})
	if err != nil {
		t.Fatal(err)
	}
	policy.resolver = staticResolver{
		"private.example":     {{IP: net.ParseIP("100.64.0.1")}},
		"sub.private.example": {{IP: net.ParseIP("100.64.0.2")}},
	}
	policy.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("test dial")
	}
	if _, err := policy.dialContext(context.Background(), "tcp", "private.example:443"); err == nil || err.Error() != "test dial" {
		t.Fatalf("allowlisted dial error = %v", err)
	}
	for _, address := range []string{"private.example:444", "sub.private.example:443"} {
		if _, err := policy.dialContext(context.Background(), "tcp", address); err == nil || !strings.Contains(err.Error(), "blocked address") {
			t.Fatalf("dialContext(%q) error = %v, want blocked address", address, err)
		}
	}
}

func TestEndpointPolicyClientRejectsRedirects(t *testing.T) {
	policy, err := NewEndpointPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := policy.httpClient()
	request, err := http.NewRequest(http.MethodGet, "https://example.com/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CheckRedirect(request, nil); !errors.Is(err, ErrRedirectBlocked) {
		t.Fatalf("CheckRedirect() error = %v", err)
	}
}

func TestNewEndpointPolicyRejectsInvalidAllowlist(t *testing.T) {
	for _, entry := range []string{"https://localhost", "localhost:0", "localhost:not-a-port", "user@localhost"} {
		if _, err := NewEndpointPolicy([]string{entry}); err == nil {
			t.Fatalf("NewEndpointPolicy(%q) succeeded", entry)
		}
	}
}
