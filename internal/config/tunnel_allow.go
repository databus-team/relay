package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// 出网白名单(network_allow):relay 隧道的目标地址白名单,在 **executor 侧强制**。
// 语义钉死(SOCKS5 基座 KTD3/R5):
//   - 主机可用 域名 / IP 字面量 / CIDR;端口可用 "443"、"80,443"、"8000-9000";缺省(@ 后为空)= 任意端口。
//   - 默认拒绝:未在 network_allow 显式放行的目标一律拒绝。
//   - 空的/缺省的 network_allow → 拒绝一切 MsgTunnelConnect(fail-closed)。
//   - host 条目只是解析提示:实际以解析出的目标 IP 是否命中某条(cidr/ip)或同名解析结果为准,
//     建连一律用已校验的 IP 字面量(避免 DNS 重绑 TOCTOU)。

// TunnelRule 一条出站白名单规则。
type TunnelRule struct {
	net   *net.IPNet // CIDR 条目
	ip    net.IP     // IP 字面量条目
	host  string     // hostname 条目(解析提示)
	ports []int      // 放行端口;空 = 任意端口
}

// AllowedByPort 报告 given 端口是否在本规则放行之列(空端口表 = 任意)。
func (r *TunnelRule) AllowedByPort(port int) bool {
	if len(r.ports) == 0 {
		return true
	}
	for _, p := range r.ports {
		if p == port {
			return true
		}
	}
	return false
}

// matchesIP 报告给定 IP 是否命中本规则的网段/IP字面量;hostname 条目单独按「同名解析结果」匹配。
func (r *TunnelRule) matchesIP(ip net.IP) bool {
	if r.net != nil {
		return r.net.Contains(ip)
	}
	if r.ip != nil {
		return r.ip.Equal(ip)
	}
	return false
}

// ParseNetworkAllowlist 解析 network_allow 配置为可执行规则表。非法条目返回错误。
// 对显式 @ 端口串做严格解析(fail-closed):带 @ 但解析不出任何合法端口 → 配置错误,绝不静默
// 放宽成任意端口(空端口表 = 任意端口仅用于「无 @」的整机放行,不适用带 @ 的写法)。
func ParseNetworkAllowlist(entries []string) ([]TunnelRule, error) {
	var rules []TunnelRule
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		host, hadAt, ports := splitPortSpec(e)
		if hadAt && len(ports) == 0 {
			return nil, fmt.Errorf("network_allow entry %q: invalid port list", e)
		}
		r, err := parseTunnelHost(host)
		if err != nil {
			return nil, fmt.Errorf("network_allow entry %q: %w", e, err)
		}
		r.ports = ports
		rules = append(rules, r)
	}
	return rules, nil
}

// splitPortSpec 拆出 "host@ports",返回是否带 @(用于「显式端口但解析不出 → 配置错误」的判定)。
// 用 @ 而非 ':' 作分隔,避免与 IPv6 字面量地址里的 ':' 冲突。
func splitPortSpec(entry string) (host string, hadAt bool, ports []int) {
	if i := strings.LastIndex(entry, "@"); i >= 0 {
		return strings.TrimSpace(entry[:i]), true, parsePortList(entry[i+1:])
	}
	host = entry
	return host, false, nil
}

// parsePortList 解析端口列表/范围:"443" -> [443];"80,443" -> [80,443];"8000-9000" -> 展开。
func parsePortList(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, lerr := strconv.Atoi(strings.TrimSpace(lo))
			h, herr := strconv.Atoi(strings.TrimSpace(hi))
			if lerr == nil && herr == nil && l > 0 && h >= l && h <= 65535 {
				for p := l; p <= h; p++ {
					out = append(out, p)
				}
			}
			continue
		}
		if p, err := strconv.Atoi(part); err == nil && p > 0 && p <= 65535 {
			out = append(out, p)
		}
	}
	return out
}

// parseTunnelHost 解析单条主机条目:CIDR / IP 字面量 / hostname。
func parseTunnelHost(host string) (TunnelRule, error) {
	if strings.Contains(host, "/") {
		_, ipnet, err := net.ParseCIDR(host)
		if err != nil {
			return TunnelRule{}, fmt.Errorf("bad CIDR %q", host)
		}
		return TunnelRule{net: ipnet}, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return TunnelRule{ip: ip}, nil
	}
	return TunnelRule{host: host}, nil
}

// CheckTunnelTarget 校验建连目标(host+port)是否命中白名单。命中则返回**已校验的 IP 字面量**
// (dial 必须用它、不得回退到原始 hostname 重解析),未命中返回 nil。hostname 解析失败视为不命中。
func CheckTunnelTarget(rules []TunnelRule, host string, port int) (net.IP, bool) {
	ips := resolveTargetHosts(host)
	for _, ip := range ips {
		for i := range rules {
			if ruleAllows(&rules[i], ip, port) {
				return ip, true
			}
		}
	}
	return nil, false
}

// resolveTargetHosts 把目标解析为 IP 列表:IP 字面量直接返回;hostname 用 LookupIP。
// 解析失败(域名不存在)返回空切片,由调用方按不命中处理。
func resolveTargetHosts(host string) []net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil
	}
	return ips
}

func ruleAllows(r *TunnelRule, ip net.IP, port int) bool {
	if !r.AllowedByPort(port) {
		return false
	}
	if r.matchesIP(ip) {
		return true
	}
	// hostname 条目:其解析解析出的 IP 与本目标 IP 一致才放行(host 仅解析提示,不直接逐名放行)。
	if r.host != "" {
		for _, hip := range resolveTargetHosts(r.host) {
			if hip.Equal(ip) {
				return true
			}
		}
	}
	return false
}
