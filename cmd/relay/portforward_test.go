package main

import (
	"testing"
)

func TestParsePortForwardSpec(t *testing.T) {
	tests := []struct {
		raw         string
		defaultBind string
		wantBind    string
		wantLocal   int
		wantRemoteH string
		wantRemoteP int
		wantErr     bool
	}{
		{
			raw:         "8080",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 8080,
			wantErr:     false,
		},
		{
			raw:         "8080:9000",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 9000,
			wantErr:     false,
		},
		{
			raw:         "0.0.0.0:8080:9000",
			defaultBind: "127.0.0.1",
			wantBind:    "0.0.0.0",
			wantLocal:   8080,
			wantRemoteH: "127.0.0.1",
			wantRemoteP: 9000,
			wantErr:     false,
		},
		{
			raw:         "8080:db.local:5432",
			defaultBind: "127.0.0.1",
			wantBind:    "127.0.0.1",
			wantLocal:   8080,
			wantRemoteH: "db.local",
			wantRemoteP: 5432,
			wantErr:     false,
		},
		{
			raw:     "invalid",
			wantErr: true,
		},
		{
			raw:     "0:8080",
			wantErr: true,
		},
		{
			raw:     "8080:70000",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		spec, err := parsePortForwardSpec(tt.raw, tt.defaultBind)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parsePortForwardSpec(%q) expected error, got nil", tt.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parsePortForwardSpec(%q) unexpected error: %v", tt.raw, err)
		}
		if spec.BindHost != tt.wantBind {
			t.Errorf("BindHost = %q, want %q", spec.BindHost, tt.wantBind)
		}
		if spec.LocalPort != tt.wantLocal {
			t.Errorf("LocalPort = %d, want %d", spec.LocalPort, tt.wantLocal)
		}
		if spec.RemoteHost != tt.wantRemoteH {
			t.Errorf("RemoteHost = %q, want %q", spec.RemoteHost, tt.wantRemoteH)
		}
		if spec.RemotePort != tt.wantRemoteP {
			t.Errorf("RemotePort = %d, want %d", spec.RemotePort, tt.wantRemoteP)
		}
	}
}

func TestPfInstanceName(t *testing.T) {
	cases := []struct {
		executor  string
		localPort int
		want      string
	}{
		{"node-1", 8080, "pf-node-1-8080"},
		{"win.dev@lab", 3000, "pf-win_dev_lab-3000"},
	}
	for _, c := range cases {
		got := pfInstanceName(c.executor, c.localPort)
		if got != c.want {
			t.Errorf("pfInstanceName(%q, %d) = %q, want %q", c.executor, c.localPort, got, c.want)
		}
	}
}
