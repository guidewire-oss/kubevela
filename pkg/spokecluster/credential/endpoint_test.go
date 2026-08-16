/*
Copyright 2026 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package credential

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Unit tests must not depend on ambient DNS. Hostnames resolve to TEST-NET-3
	// unless a case overrides lookupIPs (for rebinding coverage).
	lookupIPs = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("203.0.113.10")}, nil
	}
	os.Exit(m.Run())
}

func TestValidateSpokeEndpoint_allow(t *testing.T) {
	for _, ep := range []string{
		// Public / cloud API hostnames (AWS EKS shapes)
		"https://ABCDEF123.gr7.us-west-2.eks.amazonaws.com",
		"https://ABCDEF123.gr7.us-west-2.eks.amazonaws.com:443",
		"https://ABCDEF123.gr7.us-west-2.eks.amazonaws.com:6443",
		"https://my-cluster.privatelink.eks.amazonaws.com",
		"https://my-cluster.privatelink.us-east-1.eks.amazonaws.com",
		"https://ABCDEF.yl4.us-east-1.eks.amazonaws.com/",
		// Other cloud / on-prem FQDNs (deny-list must not block these)
		"https://api.example.com:6443",
		"https://k8s.corp.example:6443",
		"https://my-aks-dns-abc123.hcp.eastus.azmk8s.io:443",
		"https://xx.yy.zz.container.googleapis.com",
		// RFC1918 / Docker / k3d container IPs (real spoke API servers)
		"https://172.25.0.2:6443",
		"https://172.16.0.1:6443",
		"https://172.31.255.255:6443",
		"https://10.0.0.5:6443",
		"https://10.255.255.255:6443",
		"https://192.168.1.10:6443",
		"https://192.168.0.1:6443",
		"https://10.42.0.1:6443",
		"https://10.43.0.1:6443",
		// CGNAT / other non-blocked private-ish ranges we intentionally allow
		"https://100.64.0.1:6443",
		// Path/query on a legitimate endpoint must not change the verdict
		"https://api.example.com:6443/version",
		"https://172.25.0.2:6443/?x=1",
		// Trailing-dot FQDN (DNS root) should still allow non-blocked names
		"https://api.example.com.:6443",
		// Mixed-case hostname (policy lowercases for name checks)
		"https://API.Example.COM:6443",
		// Userinfo is unusual but host policy still applies to the host part
		"https://token@api.example.com:6443",
	} {
		if err := ValidateSpokeEndpoint(ep); err != nil {
			t.Errorf("ValidateSpokeEndpoint(%q) = %v, want nil", ep, err)
		}
	}
}

func TestValidateSpokeEndpoint_deny(t *testing.T) {
	cases := []struct {
		ep   string
		want string
	}{
		{"", "empty"},
		{"http://172.25.0.2:6443", "https"},
		{"HTTP://172.25.0.2:6443", "https"},
		{"ftp://api.example.com", "https"},
		{"https://", "no host"},
		{"https:///path-only", "no host"},
		// Loopback / unspecified
		{"https://127.0.0.1:6443", "blocked"},
		{"https://127.0.0.1", "blocked"},
		{"https://127.255.255.255:6443", "blocked"},
		{"https://0.0.0.0:6443", "unspecified"},
		{"https://0.0.0.0", "unspecified"},
		{"https://[::]:6443", "unspecified"},
		{"https://localhost:6443", "loopback"},
		{"https://LOCALHOST", "loopback"},
		{"https://LocalHost:443", "loopback"},
		{"https://[::1]:6443", "blocked"},
		{"https://[::1]", "blocked"},
		// Link-local / cloud metadata
		{"https://169.254.169.254/", "blocked"},
		{"https://169.254.169.254/latest/meta-data/", "blocked"},
		{"https://169.254.169.254:80/", "blocked"},
		{"https://169.254.0.1:6443", "blocked"},
		{"https://169.254.255.255/", "blocked"},
		{"https://100.100.100.200/", "blocked"},
		{"https://100.100.100.200:80/latest/meta-data/", "blocked"},
		{"https://[fc00::1]:6443", "blocked"},
		{"https://[fd12:3456:789a::1]:6443", "blocked"},
		{"https://[fec0::1]:6443", "blocked"},
		{"https://[fe80::1]:6443", "blocked"},
		{"https://[fe80::a00:27ff:fe4e:66a1]:6443", "blocked"},
		// Zone IDs must be percent-encoded as %25 in URLs; Go rejects bare "%eth0".
		{"https://[fe80::1%25eth0]:6443", "blocked"},
		{"https://168.63.129.16/", "blocked"},
		{"https://168.63.129.16:80/", "blocked"},
		{"https://[fd00:ec2::254]/", "blocked"},
		{"https://[fd00:ec2::254]:80/", "blocked"},
		{"https://metadata/", "blocked"},
		{"https://METADATA/", "blocked"},
		{"https://metadata.google.internal/", "blocked"},
		{"https://Metadata.Google.Internal:80/", "blocked"},
		// Hub kubernetes Service DNS (exact + suffix)
		{"https://kubernetes", "blocked"},
		{"https://kubernetes.default", "blocked"},
		{"https://kubernetes.default.svc", "blocked"},
		{"https://kubernetes.default.svc.cluster.local", "blocked"},
		{"https://kubernetes.default.svc:443", "blocked"},
		{"https://KUBERNETES.DEFAULT.SVC", "blocked"},
		{"https://kubernetes.default.svc.cluster.local.", "blocked"},
		{"https://my-svc.default.svc", "hub in-cluster"},
		{"https://foo.bar.svc.cluster.local", "hub in-cluster"},
		{"https://something.cluster.local", "hub in-cluster"},
		{"https://vela-core-cluster-gateway.vela-system.svc", "hub in-cluster"},
		{"https://vela-core-cluster-gateway.vela-system.svc.cluster.local:9443", "hub in-cluster"},
		{"https://headless.ns.svc:443", "hub in-cluster"},
		// Scheme smuggling / odd forms that still resolve to blocked hosts
		{"https://169.254.169.254#fragment", "blocked"},
		{"https://127.0.0.1:6443/version", "blocked"},
		{"https://kubernetes.default.svc/api", "blocked"},
	}
	for _, tc := range cases {
		err := ValidateSpokeEndpoint(tc.ep)
		if err == nil {
			t.Errorf("ValidateSpokeEndpoint(%q) = nil, want error containing %q", tc.ep, tc.want)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
			t.Errorf("ValidateSpokeEndpoint(%q) = %v, want substring %q", tc.ep, err, tc.want)
		}
	}
}

func TestValidateSpokeEndpoint_dnsRebinding(t *testing.T) {
	orig := lookupIPs
	t.Cleanup(func() { lookupIPs = orig })

	lookupIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "169.254.169.254.nip.io", "imds.example":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		case "loopback.example":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		case "unspecified.example":
			return []net.IP{net.ParseIP("0.0.0.0")}, nil
		default:
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		}
	}

	cases := []struct {
		ep   string
		want string
	}{
		{"https://169.254.169.254.nip.io/", "blocked"},
		{"https://imds.example:443/", "blocked"},
		{"https://loopback.example:6443", "blocked"},
		{"https://unspecified.example/", "unspecified"},
	}
	for _, tc := range cases {
		err := ValidateSpokeEndpoint(tc.ep)
		if err == nil {
			t.Errorf("ValidateSpokeEndpoint(%q) = nil, want error containing %q", tc.ep, tc.want)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
			t.Errorf("ValidateSpokeEndpoint(%q) = %v, want substring %q", tc.ep, err, tc.want)
		}
	}
}

func TestValidateSpokeEndpoint_boundaryCIDRs(t *testing.T) {
	allowJustOutside := []string{
		"https://126.255.255.255:6443", // below 127/8
		"https://128.0.0.1:6443",       // above 127/8
		"https://169.253.255.255:6443", // below 169.254/16
		"https://169.255.0.1:6443",     // above 169.254/16
		"https://168.63.129.15:6443",   // next to Azure IMDS
		"https://168.63.129.17:6443",
	}
	for _, ep := range allowJustOutside {
		if err := ValidateSpokeEndpoint(ep); err != nil {
			t.Errorf("boundary allow %q: %v", ep, err)
		}
	}
}

func TestValidateSpokeEndpoint_denyPrivate(t *testing.T) {
	prev := DenyPrivateEndpoints
	DenyPrivateEndpoints = true
	t.Cleanup(func() { DenyPrivateEndpoints = prev })

	private := []string{
		"https://10.0.0.1:6443",
		"https://172.16.5.1:6443",
		"https://192.168.1.10:6443",
		"https://100.64.0.1:6443",
	}
	for _, ep := range private {
		err := ValidateSpokeEndpoint(ep)
		if err == nil {
			t.Errorf("DenyPrivateEndpoints=%v ValidateSpokeEndpoint(%q) = nil, want error", true, ep)
			continue
		}
		if !strings.Contains(err.Error(), "private range") {
			t.Errorf("ValidateSpokeEndpoint(%q) = %v, want private range mention", ep, err)
		}
	}
	// Public still allowed.
	if err := ValidateSpokeEndpoint("https://8.8.8.8:6443"); err != nil {
		t.Errorf("public endpoint refused under deny-private: %v", err)
	}
}

func TestValidateSpokeEndpoint_tableDrivenPairs(t *testing.T) {
	hosts := []string{
		"172.27.0.2:6443",
		"ABCDEF123.gr7.us-west-2.eks.amazonaws.com",
		"api.example.com:6443",
		"10.0.0.1:6443",
	}
	for _, host := range hosts {
		httpsEP := "https://" + host
		httpEP := "http://" + host
		if err := ValidateSpokeEndpoint(httpsEP); err != nil {
			t.Errorf("https allow %q: %v", httpsEP, err)
		}
		if err := ValidateSpokeEndpoint(httpEP); err == nil {
			t.Errorf("http deny %q: want error", httpEP)
		} else if !strings.Contains(err.Error(), "https") {
			t.Errorf("http deny %q: %v, want https mention", httpEP, err)
		}
	}

	blockedHosts := []string{
		"169.254.169.254",
		"127.0.0.1:6443",
		"kubernetes.default.svc",
		"localhost:6443",
		"0.0.0.0:6443",
	}
	for _, host := range blockedHosts {
		ep := "https://" + host
		if err := ValidateSpokeEndpoint(ep); err == nil {
			t.Errorf("blocked host %q: want error", ep)
		}
	}
}

func TestValidateSpokeProxyURL_dnsRebinding(t *testing.T) {
	orig := lookupIPs
	t.Cleanup(func() { lookupIPs = orig })

	lookupIPs = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "imds.example":
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		case "loopback.example":
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		default:
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		}
	}

	for _, tc := range []struct {
		proxy string
		want  string
	}{
		{"http://imds.example:8080", "blocked"},
		{"http://loopback.example:8080", "blocked"},
	} {
		err := ValidateSpokeProxyURL(tc.proxy)
		if err == nil {
			t.Errorf("ValidateSpokeProxyURL(%q) = nil, want error containing %q", tc.proxy, tc.want)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
			t.Errorf("ValidateSpokeProxyURL(%q) = %v, want substring %q", tc.proxy, err, tc.want)
		}
	}
}

func TestValidateSpokeProxyURL_allow(t *testing.T) {
	for _, proxy := range []string{
		"",
		"http://10.0.0.1:8080",
		"https://10.0.0.1:8443",
		"http://192.168.1.10:3128",
		"http://172.16.0.1:8080",
		"http://proxy.example.com:8080",
		"https://proxy.corp.example:443",
	} {
		if err := ValidateSpokeProxyURL(proxy); err != nil {
			t.Errorf("ValidateSpokeProxyURL(%q) = %v, want nil", proxy, err)
		}
	}
}

func TestValidateSpokeProxyURL_deny(t *testing.T) {
	cases := []struct {
		proxy string
		want  string
	}{
		{"ftp://10.0.0.1:8080", "http or https"},
		{"http://", "no host"},
		{"http://127.0.0.1:8080", "blocked"},
		{"http://localhost:8080", "loopback"},
		{"http://169.254.169.254/", "blocked"},
		{"http://[fd00:ec2::254]/", "blocked"},
		{"http://168.63.129.16/", "blocked"},
		{"http://kubernetes.default.svc", "hub-internal"},
		{"http://squid.vela-system.svc.cluster.local:3128", "hub in-cluster"},
		{"http://0.0.0.0:8080", "unspecified"},
	}
	for _, tc := range cases {
		err := ValidateSpokeProxyURL(tc.proxy)
		if err == nil {
			t.Errorf("ValidateSpokeProxyURL(%q) = nil, want error containing %q", tc.proxy, tc.want)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
			t.Errorf("ValidateSpokeProxyURL(%q) = %v, want substring %q", tc.proxy, err, tc.want)
		}
	}
}

func TestValidateSpokeEndpoint_errorMentionsEndpoint(t *testing.T) {
	ep := "https://169.254.169.254/latest/meta-data/"
	err := ValidateSpokeEndpoint(ep)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), ep) {
		t.Fatalf("error %q should quote the endpoint for operator debugging", err)
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("error %q should say not permitted", err)
	}
}
