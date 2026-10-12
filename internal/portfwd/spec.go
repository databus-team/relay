// Package portfwd 是端口转发规格的规范类型与其解析语法的叶子包。
// CLI、协议、后端与 supervisor 共用这一份规格,避免每层各写一份近似结构再互相映射(KTD9);
// 它只依赖标准库,因此不会让 supervisor 与 protocol 之间产生导入关系。
package portfwd

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// DefaultBind 是未指明绑定主机时的历史默认值(仅本机可连)。
const DefaultBind = "127.0.0.1"

// defaultRemote 是未指明远端主机时的默认值:远端就在执行方本机,与绑定面的默认各是一回事。
const defaultRemote = DefaultBind

// Spec 描述一条端口转发规则。json tag 使它与 node<->CLI 控制 socket 的既有 wire 形态一致。
type Spec struct {
	BindHost   string `json:"bind"`
	LocalPort  int    `json:"local_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort int    `json:"remote_port"`
}

// String 返回规格的展示字符串,例如 "8080"、"8080:9000" 或 "8080:db.local:5432"。
// 它按设计不输出绑定主机:只用于展示,记录与重登记一律走结构字段(KTD9、R21)。
func (s Spec) String() string {
	if s.RemoteHost != "" && s.RemoteHost != defaultRemote && s.RemoteHost != "localhost" {
		return fmt.Sprintf("%d:%s:%d", s.LocalPort, s.RemoteHost, s.RemotePort)
	}
	if s.LocalPort == s.RemotePort {
		return strconv.Itoa(s.LocalPort)
	}
	return fmt.Sprintf("%d:%d", s.LocalPort, s.RemotePort)
}

// Parse 解析形如 "8080", "8080:8080", "127.0.0.1:8080", "0.0.0.0:8080:8080",
// "8080:db.local:5432" 的规格。两段式首个元素非纯数字即视为绑定主机(host:port),与三段式的判定一致
func Parse(raw, defaultBind string) (*Spec, error) {
	if defaultBind == "" {
		defaultBind = DefaultBind
	}
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 1:
		// "8080"
		p, err := strconv.Atoi(parts[0])
		if err != nil || p <= 0 || p > 65535 {
			return nil, fmt.Errorf("invalid port: %q", parts[0])
		}
		return &Spec{
			BindHost:   defaultBind,
			LocalPort:  p,
			RemoteHost: defaultRemote,
			RemotePort: p,
		}, nil
	case 2:
		lp, lerr := strconv.Atoi(parts[0])
		if lerr != nil && parts[0] != "" {
			// "host:port": 绑定主机 + 本地端口，远端默认同本地端口
			p, perr := strconv.Atoi(parts[1])
			if perr != nil || p <= 0 || p > 65535 {
				return nil, fmt.Errorf("invalid local port: %q", parts[1])
			}
			return &Spec{
				BindHost:   parts[0],
				LocalPort:  p,
				RemoteHost: defaultRemote,
				RemotePort: p,
			}, nil
		}
		// "8080:9000"
		rp, err2 := strconv.Atoi(parts[1])
		if lerr != nil || lp <= 0 || lp > 65535 {
			return nil, fmt.Errorf("invalid local port: %q", parts[0])
		}
		if err2 != nil || rp <= 0 || rp > 65535 {
			return nil, fmt.Errorf("invalid remote port: %q", parts[1])
		}
		return &Spec{
			BindHost:   defaultBind,
			LocalPort:  lp,
			RemoteHost: defaultRemote,
			RemotePort: rp,
		}, nil
	case 3:
		// "0.0.0.0:8080:9000" 或 "8080:remotehost:9000"
		if lp, err := strconv.Atoi(parts[0]); err == nil {
			// 首段是数字: 8080:remotehost:9000
			rp, rerr := strconv.Atoi(parts[2])
			if rerr != nil || rp <= 0 || rp > 65535 {
				return nil, fmt.Errorf("invalid remote port: %q", parts[2])
			}
			return &Spec{
				BindHost:   defaultBind,
				LocalPort:  lp,
				RemoteHost: parts[1],
				RemotePort: rp,
			}, nil
		}
		// 首段是 host: 0.0.0.0:8080:9000
		lp, err1 := strconv.Atoi(parts[1])
		rp, err2 := strconv.Atoi(parts[2])
		if err1 != nil || lp <= 0 || lp > 65535 || err2 != nil || rp <= 0 || rp > 65535 {
			return nil, fmt.Errorf("invalid port in %q", raw)
		}
		return &Spec{
			BindHost:   parts[0],
			LocalPort:  lp,
			RemoteHost: defaultRemote,
			RemotePort: rp,
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized port format %q (expected [local:]remote or local:remoteHost:remotePort)", raw)
	}
}

// UnmarshalJSON 同时接受结构化对象与历史的展示字符串形态。
// 老执行方上行的任务简报、以及升级前落盘的 meta.json,ports 都是 String() 的产物([]string);
// 那些字符串丢掉的只有绑定面,因此按历史默认绑定 127.0.0.1 还原。
func (s *Spec) UnmarshalJSON(data []byte) error {
	if trimmed := strings.TrimSpace(string(data)); strings.HasPrefix(trimmed, `"`) {
		var legacy string
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		parsed, err := Parse(legacy, DefaultBind)
		if err != nil {
			return fmt.Errorf("legacy port spec %q: %w", legacy, err)
		}
		*s = *parsed
		return nil
	}
	// 用别名类型解码,避免再次进入本方法造成递归。
	type objectSpec Spec
	var obj objectSpec
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*s = Spec(obj)
	return nil
}
