package config

import "testing"

func TestNetworkAllow_EmptyDeniesAll(t *testing.T) {
	// 空/缺省 network_allow → 拒绝一切建连(fail-closed,AE6)。
	for _, entries := range [][]string{nil, {}} {
		rules, err := ParseNetworkAllowlist(entries)
		if err != nil {
			t.Fatalf("parse empty: %v", err)
		}
		if _, ok := CheckTunnelTarget(rules, "10.1.2.3", 80); ok {
			t.Errorf("empty allowlist must deny all, but allowed 10.1.2.3:80")
		}
		if _, ok := CheckTunnelTarget(rules, "example.com", 443); ok {
			t.Errorf("empty allowlist must deny all, but allowed example.com:443")
		}
	}
}

func TestNetworkAllowlist_IPExact(t *testing.T) {
	rules, err := ParseNetworkAllowlist([]string{"192.168.0.5@22"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := CheckTunnelTarget(rules, "192.168.0.5", 22); !ok {
		t.Error("192.168.0.5:22 should be allowed")
	}
	if _, ok := CheckTunnelTarget(rules, "192.168.0.5", 80); ok {
		t.Error("192.168.0.5:80 must be denied (port not allowed)")
	}
	if _, ok := CheckTunnelTarget(rules, "192.168.0.6", 22); ok {
		t.Error("192.168.0.6:22 must be denied (ip not allowed)")
	}
}

func TestNetworkAllowlist_CIDRAndPorts(t *testing.T) {
	rules, err := ParseNetworkAllowlist([]string{"10.0.0.0/8@80,443"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := CheckTunnelTarget(rules, "10.1.2.3", 443); !ok {
		t.Error("10.1.2.3:443 should be allowed by 10.0.0.0/8@80,443")
	}
	if _, ok := CheckTunnelTarget(rules, "10.9.9.9", 80); !ok {
		t.Error("10.9.9.9:80 should be allowed by 10.0.0.0/8@80,443")
	}
	if _, ok := CheckTunnelTarget(rules, "10.1.2.3", 22); ok {
		t.Error("10.1.2.3:22 must be denied (port not allowed)")
	}
	if _, ok := CheckTunnelTarget(rules, "11.0.0.1", 80); ok {
		t.Error("11.0.0.1:80 must be denied (not in CIDR)")
	}
}

func TestNetworkAllowlist_PortRange(t *testing.T) {
	rules, err := ParseNetworkAllowlist([]string{"10.0.0.0/8@8000-9000"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := CheckTunnelTarget(rules, "10.1.1.1", 8500); !ok {
		t.Error("10.1.1.1:8500 should be allowed by range 8000-9000")
	}
	if _, ok := CheckTunnelTarget(rules, "10.1.1.1", 7999); ok {
		t.Error("10.1.1.1:7999 must be denied (below range)")
	}
	if _, ok := CheckTunnelTarget(rules, "10.1.1.1", 9001); ok {
		t.Error("10.1.1.1:9001 must be denied (above range)")
	}
}

func TestNetworkAllowlist_AnyPort(t *testing.T) {
	// 无 @ 端口 = 任意端口放行。
	rules, err := ParseNetworkAllowlist([]string{"192.168.0.5"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, port := range []int{22, 80, 443, 12345} {
		if _, ok := CheckTunnelTarget(rules, "192.168.0.5", port); !ok {
			t.Errorf("192.168.0.5:%d should be allowed (any port)", port)
		}
	}
}

func TestNetworkAllowlist_BadCIDR(t *testing.T) {
	if _, err := ParseNetworkAllowlist([]string{"not-a-cidr/99"}); err == nil {
		t.Error("bad CIDR should error")
	}
}