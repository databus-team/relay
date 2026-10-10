package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRunCompletion(t *testing.T) {
	tests := []struct {
		shell       string
		mustContain string
	}{
		{"bash", "_relay_bash_autocomplete"},
		{"zsh", "#compdef relay"},
		{"fish", "__fish_relay_complete"},
		{"powershell", "Register-ArgumentCompleter"},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			oldStdout := os.Stdout
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe: %v", err)
			}
			os.Stdout = w

			*completionShell = tt.shell
			runCompletion()

			w.Close()
			os.Stdout = oldStdout

			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			out := buf.String()

			if !strings.Contains(out, tt.mustContain) {
				t.Errorf("completion for %s should contain %q, got: %s", tt.shell, tt.mustContain, out)
			}
		})
	}
}
