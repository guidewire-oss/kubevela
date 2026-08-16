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
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ValidateSpokeEndpoint refuses endpoints that would let a SpokeCluster coerce
// cluster-gateway into dialing hub-internal or cloud-metadata targets (SSRF).
//
// Policy is a deny-list, not an allow-list: https is required, but RFC1918 /
// Docker/k3d private IPs and public cloud API hostnames (for example
// *.eks.amazonaws.com) remain allowed so real spokes keep working. Set
// DenyPrivateEndpoints to also refuse RFC1918/CGNAT when every spoke is expected
// to be reachable on a public (or otherwise non-private) address.
//
// Hostnames are resolved and every returned address is checked against the same
// IP deny-list, so spellings like 169.254.169.254.nip.io cannot bypass the
// literal-IP checks. DNS rebinding after this check remains a residual risk for
// the dial that cluster-gateway performs (validation is point-in-time and the
// gateway has no dial-time denylist). The SpokeCluster reconciler re-runs this
// check on every pass, including credential-cache hits, and revokes the gateway
// Secret when revalidation fails.
func ValidateSpokeEndpoint(endpoint string) error {
	return validateSpokeEndpoint(context.Background(), endpoint)
}

// ValidateSpokeProxyURL applies the same host/IP denylist as ValidateSpokeEndpoint
// to a kubeconfig proxy-url. Empty is valid (no proxy). Unlike the spoke API
// endpoint, http is allowed and RFC1918 destinations remain allowed, so a
// corporate or in-VPC forward proxy can sit between cluster-gateway and the
// spoke. Loopback, unspecified, link-local/IMDS, and hub in-cluster DNS are
// still refused.
func ValidateSpokeProxyURL(proxy string) error {
	return validateSpokeProxyURL(context.Background(), proxy)
}

func validateSpokeProxyURL(ctx context.Context, proxy string) error {
	if proxy == "" {
		return nil
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return fmt.Errorf("spoke proxy-url %q is not a valid URL: %w", proxy, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("spoke proxy-url %q must use http or https (got %q)", proxy, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("spoke proxy-url %q has no host", proxy)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("spoke proxy-url %q has no host", proxy)
	}
	if err := denyHubOrMetadataHost(ctx, host); err != nil {
		return fmt.Errorf("spoke proxy-url %q is not permitted: %w", proxy, err)
	}
	return nil
}

func validateSpokeEndpoint(ctx context.Context, endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("spoke endpoint is empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("spoke endpoint %q is not a valid URL: %w", endpoint, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("spoke endpoint %q must use https (got %q)", endpoint, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("spoke endpoint %q has no host", endpoint)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("spoke endpoint %q has no host", endpoint)
	}
	if err := denyHubOrMetadataHost(ctx, host); err != nil {
		return fmt.Errorf("spoke endpoint %q is not permitted: %w", endpoint, err)
	}
	return nil
}

func denyHubOrMetadataHost(ctx context.Context, host string) error {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))

	if lower == "localhost" {
		return fmt.Errorf("host %q is loopback", host)
	}
	for _, name := range blockedExactHosts {
		if lower == name {
			return fmt.Errorf("host %q is a blocked metadata or hub-internal name", host)
		}
	}
	for _, suffix := range blockedHostSuffixes {
		if lower == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(lower, suffix) {
			return fmt.Errorf("host %q is hub in-cluster DNS and is not permitted", host)
		}
	}

	// IPv6 zone identifiers (fe80::1%eth0) are not accepted by net.ParseIP. Strip the
	// zone and evaluate the address; a bare "%zone" that is not an IP is rejected.
	ipHost := host
	if i := strings.Index(host, "%"); i >= 0 {
		ipHost = host[:i]
		if ipHost == "" {
			return fmt.Errorf("host %q has an IPv6 zone identifier and is not permitted", host)
		}
	}
	if ip := net.ParseIP(ipHost); ip != nil {
		return denyBlockedIP(ip, host)
	}
	if strings.Contains(host, "%") {
		return fmt.Errorf("host %q has an IPv6 zone identifier and is not permitted", host)
	}

	ips, err := lookupIPs(ctx, lower)
	if err != nil {
		return fmt.Errorf("host %q could not be resolved for SSRF checks: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("host %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if err := denyBlockedIP(ip, host); err != nil {
			return err
		}
	}
	return nil
}

// DenyPrivateEndpoints, when true, also refuses RFC1918 and CGNAT destinations
// so a SpokeCluster cannot coerce cluster-gateway into dialing hub-LAN or
// cloud-private API addresses from a public hub. Off by default: k3d, kind,
// and many VPC-peered installs publish private spoke API endpoints that must
// keep working. Enable via --spoke-endpoint-deny-private on cluster-core when
// every spoke endpoint is expected to be public (or otherwise outside those ranges).
var DenyPrivateEndpoints bool

func denyBlockedIP(ip net.IP, host string) error {
	if ip.IsUnspecified() {
		return fmt.Errorf("host %q is an unspecified address", host)
	}
	for _, cidr := range blockedCIDRs {
		if cidr.Contains(ip) {
			return fmt.Errorf("host %q is in blocked range %s", host, cidr)
		}
	}
	for _, blocked := range blockedIPs {
		if ip.Equal(blocked) {
			return fmt.Errorf("host %q is a blocked metadata address", host)
		}
	}
	if DenyPrivateEndpoints {
		for _, cidr := range privateCIDRs {
			if cidr.Contains(ip) {
				return fmt.Errorf("host %q is in private range %s (spoke-endpoint-deny-private is enabled)", host, cidr)
			}
		}
	}
	return nil
}

// lookupIPs resolves a hostname for SSRF checks. Tests replace this to avoid network
// dependency and to simulate rebinding targets.
var lookupIPs = defaultLookupIPs

// SetLookupIPsForTest replaces the DNS resolver used by ValidateSpokeEndpoint.
// Callers must restore via the returned function. Intended for tests only.
func SetLookupIPsForTest(fn func(context.Context, string) ([]net.IP, error)) (restore func()) {
	prev := lookupIPs
	lookupIPs = fn
	return func() { lookupIPs = prev }
}

func defaultLookupIPs(ctx context.Context, host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

var blockedExactHosts = []string{
	"metadata",
	"metadata.google.internal",
	"kubernetes",
	"kubernetes.default",
	"kubernetes.default.svc",
	"kubernetes.default.svc.cluster.local",
}

// Hub Service DNS always ends in one of these. Spoke API servers are not on the
// hub's cluster DNS, so anything under these suffixes is treated as SSRF.
var blockedHostSuffixes = []string{
	".svc",
	".svc.cluster.local",
	".cluster.local",
}

var blockedCIDRs = mustParseCIDRs(
	"127.0.0.0/8",     // IPv4 loopback
	"::1/128",         // IPv6 loopback
	"169.254.0.0/16",  // IPv4 link-local / AWS+GCP IMDS
	"fe80::/10",       // IPv6 link-local
	"fc00::/7",        // IPv6 unique local (ULA)
	"fec0::/10",       // IPv6 site-local (deprecated, still routable on some nets)
)

// privateCIDRs are refused only when DenyPrivateEndpoints is set. They stay
// allowed under the default deny-list so RFC1918 / CGNAT spoke APIs work.
var privateCIDRs = mustParseCIDRs(
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10", // CGNAT / shared address space
)

var blockedIPs = []net.IP{
	net.ParseIP("168.63.129.16"),  // Azure IMDS wire server
	net.ParseIP("fd00:ec2::254"),  // AWS IMDS IPv6
	net.ParseIP("100.100.100.200"), // Alibaba Cloud IMDS
}

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("invalid blocked CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}
