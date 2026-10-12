package main

import "testing"

// resolveTaskRestartPolicy 校验任务模式 --restart/--no-restart:取值复用 task/tunnel 共用的
// restartPolicyValue 纯函数;任一旗标显式给出时都要求 -d/--detach,前台执行带它直接报错(R5)。
func TestResolveTaskRestartPolicy(t *testing.T) {
	tests := []struct {
		name      string
		policy    string
		noRestart bool
		detach    bool
		want      string
		wantErr   bool
	}{
		{name: "no flag foreground defaults to no", policy: "", detach: false, want: tunRestartNo},
		{name: "detach always", policy: "always", detach: true, want: tunRestartAlways},
		{name: "detach no", policy: "no", detach: true, want: tunRestartNo},
		{name: "no-restart wins over always", policy: "always", noRestart: true, detach: true, want: tunRestartNo},
		{name: "value is normalized", policy: "  ALWAYS ", detach: true, want: tunRestartAlways},
		{name: "invalid value", policy: "sometimes", detach: true, wantErr: true},
		{name: "always without detach", policy: "always", detach: false, wantErr: true},
		{name: "explicit no without detach", policy: "no", detach: false, wantErr: true},
		{name: "no-restart without detach", policy: "", noRestart: true, detach: false, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTaskRestartPolicy(tc.policy, tc.noRestart, tc.detach)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got policy %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("policy = %q, want %q", got, tc.want)
			}
		})
	}
}
