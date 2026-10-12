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
