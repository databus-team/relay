package main

import (
	"strings"
	"testing"
)

// TestResolveTunnelMode 逐条钉住 relay run --type=tunnel 的模式矩阵(R14/R16):
// 恰好一个 -p、没有位置命令、不接受任务语义旗标、出口与策略归一。
// 纯函数,不触网、不读本机 node。
func TestResolveTunnelMode(t *testing.T) {
	cases := []struct {
		name    string
		in      tunnelModeInput
		want    *tunnelModeRequest
		wantErr string // 非空则要求错误信息包含该子串
	}{
		{
			name: "explicit loopback listen",
			in:   tunnelModeInput{Publish: []string{"127.0.0.1:1080"}, Executor: "site-a", BindHost: "127.0.0.1"},
			want: &tunnelModeRequest{Listen: &TunnelListenSpec{BindHost: "127.0.0.1", Port: 1080}, Egress: "site-a", Restart: tunRestartNo},
		},
		{
			name: "port only uses --bind-host",
			in:   tunnelModeInput{Publish: []string{"1080"}, Node: "site-b", BindHost: "0.0.0.0"},
			want: &tunnelModeRequest{Listen: &TunnelListenSpec{BindHost: "0.0.0.0", Port: 1080}, Egress: "site-b", Restart: tunRestartNo},
		},
		{
			name: "restart always",
			in:   tunnelModeInput{Publish: []string{"127.0.0.1:1080"}, Executor: "site-a", BindHost: "127.0.0.1", Restart: tunRestartAlways},
			want: &tunnelModeRequest{Listen: &TunnelListenSpec{BindHost: "127.0.0.1", Port: 1080}, Egress: "site-a", Restart: tunRestartAlways},
		},
		{
			name: "no-restart overrides restart always",
			in:   tunnelModeInput{Publish: []string{"127.0.0.1:1080"}, Executor: "site-a", BindHost: "127.0.0.1", Restart: tunRestartAlways, NoRestart: true},
			want: &tunnelModeRequest{Listen: &TunnelListenSpec{BindHost: "127.0.0.1", Port: 1080}, Egress: "site-a", Restart: tunRestartNo},
		},
		{
			name: "non-loopback bind is allowed by the decision",
			in:   tunnelModeInput{Publish: []string{"0.0.0.0:1080"}, Executor: "site-a", BindHost: "127.0.0.1"},
			want: &tunnelModeRequest{Listen: &TunnelListenSpec{BindHost: "0.0.0.0", Port: 1080}, Egress: "site-a", Restart: tunRestartNo},
		},
		{
			name:    "missing -p",
			in:      tunnelModeInput{Executor: "site-a", BindHost: "127.0.0.1"},
			wantErr: "exactly one -p",
		},
		{
			name:    "repeated -p",
			in:      tunnelModeInput{Publish: []string{"8080", "8081"}, Executor: "site-a", BindHost: "127.0.0.1"},
			wantErr: "exactly one -p",
		},
		{
			name:    "remote half rejected",
			in:      tunnelModeInput{Publish: []string{"8080:9000"}, Executor: "site-a", BindHost: "127.0.0.1"},
			wantErr: "no remote half",
		},
		{
			name:    "three parts rejected",
			in:      tunnelModeInput{Publish: []string{"0.0.0.0:8080:9000"}, Executor: "site-a", BindHost: "127.0.0.1"},
			wantErr: "[host:]port",
		},
		{
			name:    "invalid port",
			in:      tunnelModeInput{Publish: []string{"70000"}, Executor: "site-a", BindHost: "127.0.0.1"},
			wantErr: "invalid port",
		},
		{
			name:    "positional command rejected",
			in:      tunnelModeInput{Command: []string{"echo", "hi"}, Publish: []string{"8080"}, Executor: "site-a", BindHost: "127.0.0.1"},
			wantErr: "takes no command",
		},
		{
			name:    "detach rejected",
			in:      tunnelModeInput{Publish: []string{"8080"}, Executor: "site-a", BindHost: "127.0.0.1", Detach: true},
			wantErr: "--detach",
		},
		{
			name:    "no-sync rejected",
			in:      tunnelModeInput{Publish: []string{"8080"}, Executor: "site-a", BindHost: "127.0.0.1", NoSync: true},
			wantErr: "--no-sync",
		},
		{
			name:    "force-remote rejected",
			in:      tunnelModeInput{Publish: []string{"8080"}, Executor: "site-a", BindHost: "127.0.0.1", Force: true},
			wantErr: "--force-remote",
		},
		{
			name:    "watch rejected",
			in:      tunnelModeInput{Publish: []string{"8080"}, Watch: "ws1", BindHost: "127.0.0.1"},
			wantErr: "--watch",
		},
		{
			name:    "missing egress",
			in:      tunnelModeInput{Publish: []string{"8080"}, BindHost: "127.0.0.1"},
			wantErr: "egress node ID",
		},
		{
			name:    "invalid restart value",
			in:      tunnelModeInput{Publish: []string{"8080"}, Executor: "site-a", BindHost: "127.0.0.1", Restart: "sometimes"},
			wantErr: "invalid --restart",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTunnelMode(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveTunnelMode() = %+v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				if got != nil {
					t.Fatalf("resolveTunnelMode() returned %+v alongside error %v; validation must not produce a request", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTunnelMode(): %v", err)
			}
			if got.Listen.BindHost != tc.want.Listen.BindHost || got.Listen.Port != tc.want.Listen.Port {
				t.Errorf("listen = %s:%d, want %s:%d", got.Listen.BindHost, got.Listen.Port, tc.want.Listen.BindHost, tc.want.Listen.Port)
			}
			if got.Egress != tc.want.Egress {
				t.Errorf("egress = %q, want %q", got.Egress, tc.want.Egress)
			}
			if got.Restart != tc.want.Restart {
				t.Errorf("restart = %q, want %q", got.Restart, tc.want.Restart)
			}
		})
	}
}

// TestRequireTaskCommand 钉住任务模式仍必须有位置命令(KTD7:kingpin 层不再 Required,改由代码按模式校验)。
func TestRequireTaskCommand(t *testing.T) {
	if err := requireTaskCommand([]string{"echo", "hi"}); err != nil {
		t.Fatalf("requireTaskCommand(echo hi): %v", err)
	}
	err := requireTaskCommand(nil)
	if err == nil {
		t.Fatal("requireTaskCommand(nil) = nil, want error")
	}
	if !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("error = %q, want it to mention a required command", err.Error())
	}
}

// TestResolveForwardMode 逐条钉住 relay run --type=forward 的模式矩阵:恰好一个 -p(可含远端半段)、
// 没有位置命令、不接受任务语义旗标、出口与策略归一。纯函数,不触网。
func TestResolveForwardMode(t *testing.T) {
	cases := []struct {
		name    string
		in      forwardModeInput
		want    *forwardModeRequest
		wantErr string
	}{
		{
			name: "local:remote both given",
			in:   forwardModeInput{Publish: []string{"7001:7001"}, Executor: "eg-x", BindHost: "127.0.0.1"},
			want: &forwardModeRequest{Spec: &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: 7001, RemoteHost: "127.0.0.1", RemotePort: 7001}, Executor: "eg-x", Restart: tunRestartNo},
		},
		{
			name: "port only uses --bind-host",
			in:   forwardModeInput{Publish: []string{"7002"}, Node: "eg-y", BindHost: "0.0.0.0"},
			want: &forwardModeRequest{Spec: &PortForwardSpec{BindHost: "0.0.0.0", LocalPort: 7002, RemoteHost: "127.0.0.1", RemotePort: 7002}, Executor: "eg-y", Restart: tunRestartNo},
		},
		{
			name: "remote host form",
			in:   forwardModeInput{Publish: []string{"7003:db.local:5432"}, Executor: "eg-x", BindHost: "127.0.0.1"},
			want: &forwardModeRequest{Spec: &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: 7003, RemoteHost: "db.local", RemotePort: 5432}, Executor: "eg-x", Restart: tunRestartNo},
		},
		{
			name: "restart always",
			in:   forwardModeInput{Publish: []string{"7001:7001"}, Executor: "eg-x", BindHost: "127.0.0.1", Restart: tunRestartAlways},
			want: &forwardModeRequest{Spec: &PortForwardSpec{BindHost: "127.0.0.1", LocalPort: 7001, RemoteHost: "127.0.0.1", RemotePort: 7001}, Executor: "eg-x", Restart: tunRestartAlways},
		},
		{
			name:    "missing -p",
			in:      forwardModeInput{Executor: "eg-x", BindHost: "127.0.0.1"},
			wantErr: "exactly one -p",
		},
		{
			name:    "repeated -p",
			in:      forwardModeInput{Publish: []string{"7001", "7002"}, Executor: "eg-x", BindHost: "127.0.0.1"},
			wantErr: "exactly one -p",
		},
		{
			name:    "invalid port",
			in:      forwardModeInput{Publish: []string{"70000"}, Executor: "eg-x", BindHost: "127.0.0.1"},
			wantErr: "invalid port",
		},
		{
			name:    "positional command rejected",
			in:      forwardModeInput{Command: []string{"echo"}, Publish: []string{"7001"}, Executor: "eg-x", BindHost: "127.0.0.1"},
			wantErr: "takes no command",
		},
		{
			name:    "detach rejected",
			in:      forwardModeInput{Publish: []string{"7001"}, Executor: "eg-x", BindHost: "127.0.0.1", Detach: true},
			wantErr: "--detach",
		},
		{
			name:    "no-sync rejected",
			in:      forwardModeInput{Publish: []string{"7001"}, Executor: "eg-x", BindHost: "127.0.0.1", NoSync: true},
			wantErr: "--no-sync",
		},
		{
			name:    "force-remote rejected",
			in:      forwardModeInput{Publish: []string{"7001"}, Executor: "eg-x", BindHost: "127.0.0.1", Force: true},
			wantErr: "--force-remote",
		},
		{
			name:    "watch rejected",
			in:      forwardModeInput{Publish: []string{"7001"}, Watch: "ws1", BindHost: "127.0.0.1"},
			wantErr: "--watch",
		},
		{
			name:    "missing egress",
			in:      forwardModeInput{Publish: []string{"7001"}, BindHost: "127.0.0.1"},
			wantErr: "egress node ID",
		},
		{
			name:    "invalid restart value",
			in:      forwardModeInput{Publish: []string{"7001"}, Executor: "eg-x", BindHost: "127.0.0.1", Restart: "sometimes"},
			wantErr: "invalid --restart",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveForwardMode(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveForwardMode() = %+v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				if got != nil {
					t.Fatalf("resolveForwardMode() returned %+v alongside error %v; validation must not produce a request", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveForwardMode(): %v", err)
			}
			if got.Spec.LocalPort != tc.want.Spec.LocalPort || got.Spec.RemoteHost != tc.want.Spec.RemoteHost || got.Spec.RemotePort != tc.want.Spec.RemotePort || got.Spec.BindHost != tc.want.Spec.BindHost {
				t.Errorf("spec = %+v, want %+v", got.Spec, tc.want.Spec)
			}
			if got.Executor != tc.want.Executor {
				t.Errorf("executor = %q, want %q", got.Executor, tc.want.Executor)
			}
			if got.Restart != tc.want.Restart {
				t.Errorf("restart = %q, want %q", got.Restart, tc.want.Restart)
			}
		})
	}
}
