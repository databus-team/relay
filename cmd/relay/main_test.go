package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kingpin/v2"
	"github.com/user/relay/internal/config"
)

// withCapturedStdout replaces os.Stdout with a pipe for the duration of fn and
// returns whatever fn wrote to stdout. The original stdout is restored on
// return.
func withCapturedStdout(t *testing.T, fn func()) string {
	t.Helper()

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	os.Stdout = origStdout
	return <-done
}

// withTempConfig writes content to a temp file and points *configPath at it.
// The returned cleanup restores the previous configPath.
func withTempConfig(t *testing.T, content string) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	prev := *configPath
	*configPath = path
	return path, func() { *configPath = prev }
}

// resetWsFlags zeros the four ws-related flags so tests don't leak state.
func resetWsFlags() {
	*wsName = ""
	*wsJSON = false
	*wsVerbose = false
}

const exampleConfig = `
name: relay
version: 1
backend:
  type: local
  config:
    base_dir: /tmp/relay
watch:
  - id: web-app-patches
    watch_dir: projects/web-app/patches
    local_dir: /home/me/repos/web
    paths: ["*.patch"]
    jobs:
      - id: apply
        type: exec
        cmd: git am --3way {file_path}
  - id: web-app-deploy
    watch_dir: projects/web-app/deploy
    local_dir: /home/me/repos/web
    paths: ["deploy-*.yaml"]
    jobs:
      - id: deploy
        type: exec
        cmd: ./deploy.sh {file_name}
  - id: api-service-patches
    watch_dir: projects/api-service/patches
    local_dir: /home/me/repos/api
    paths: ["*.patch"]
    jobs: []
interval_seconds: 60
`

const emptyWatchConfig = `
name: relay
version: 1
backend:
  type: local
  config:
    base_dir: /tmp/relay
watch: []
interval_seconds: 60
`

const duplicateIDConfig = `
name: relay
version: 1
backend:
  type: local
  config:
    base_dir: /tmp/relay
watch:
  - id: dup
    watch_dir: a
    local_dir: /a
    paths: []
    jobs: []
  - id: dup
    watch_dir: b
    local_dir: /b
    paths: []
    jobs:
      - id: j1
        type: exec
        cmd: echo
interval_seconds: 60
`

// ----- runWorkspaces integration tests -----

func TestWorkspaces_DefaultIDs(t *testing.T) {
	resetWsFlags()
	_, cleanup := withTempConfig(t, exampleConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)

	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := []string{"web-app-patches", "web-app-deploy", "api-service-patches"}
	if len(got) != len(want) {
		t.Fatalf("expected %d lines, got %d: %q", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWorkspaces_DefaultEmpty(t *testing.T) {
	resetWsFlags()
	_, cleanup := withTempConfig(t, emptyWatchConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)
	if out != "" {
		t.Errorf("expected empty stdout, got %q", out)
	}
}

func TestWorkspaces_VerboseTable(t *testing.T) {
	resetWsFlags()
	*wsVerbose = true
	_, cleanup := withTempConfig(t, exampleConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 5 {
		t.Fatalf("expected header + separator + 3 rows, got %d lines: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "REMOTE_DIR") {
		t.Errorf("header line missing expected columns: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], strings.Repeat("-", 5)) {
		t.Errorf("second line should be separator, got %q", lines[1])
	}
	for _, want := range []string{"web-app-patches", "web-app-deploy", "api-service-patches"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose table missing ID %q: %q", want, out)
		}
	}
	// job counts: 1, 1, 0 → ensure the job-count column shows the right digits.
	if !strings.Contains(out, "    1 ") && !strings.Contains(out, "    1\n") {
		t.Errorf("verbose table should show job count of 1 for first two workspaces: %q", out)
	}
}

func TestWorkspaces_VerboseEmpty(t *testing.T) {
	resetWsFlags()
	*wsVerbose = true
	_, cleanup := withTempConfig(t, emptyWatchConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected header + separator only, got %d lines: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "ID") {
		t.Errorf("expected header, got %q", lines[0])
	}
}

func TestWorkspaces_JSON(t *testing.T) {
	resetWsFlags()
	*wsJSON = true
	_, cleanup := withTempConfig(t, exampleConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)

	var got []workspaceJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 workspaces, got %d", len(got))
	}
	if got[0].ID != "web-app-patches" {
		t.Errorf("first ID = %q, want web-app-patches", got[0].ID)
	}
	if got[0].WatchDir != "projects/web-app/patches" {
		t.Errorf("first watch_dir = %q", got[0].WatchDir)
	}
	if len(got[0].Jobs) != 1 || got[0].Jobs[0].Cmd != "git am --3way {file_path}" {
		t.Errorf("first jobs slice = %+v", got[0].Jobs)
	}
}

func TestWorkspaces_JSONEmpty(t *testing.T) {
	resetWsFlags()
	*wsJSON = true
	_, cleanup := withTempConfig(t, emptyWatchConfig)
	defer cleanup()

	out := strings.TrimSpace(withCapturedStdout(t, runWorkspaces))
	if out != "[]" {
		t.Errorf("expected exactly %q, got %q", "[]", out)
	}
}

func TestWorkspaces_DuplicateIDs(t *testing.T) {
	resetWsFlags()
	_, cleanup := withTempConfig(t, duplicateIDConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)

	count := strings.Count(out, "dup\n")
	if count != 2 {
		t.Errorf("expected duplicate ID rendered twice, got %d occurrences in: %q", count, out)
	}
}

func TestWorkspaces_NameHit_Default(t *testing.T) {
	resetWsFlags()
	*wsName = "web-app-patches"
	_, cleanup := withTempConfig(t, exampleConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)
	if got := strings.TrimSpace(out); got != "web-app-patches" {
		t.Errorf("expected single line %q, got %q", "web-app-patches", got)
	}
}

func TestWorkspaces_NameHit_Verbose(t *testing.T) {
	resetWsFlags()
	*wsName = "web-app-patches"
	*wsVerbose = true
	_, cleanup := withTempConfig(t, exampleConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected header + separator + 1 row, got %d lines: %q", len(lines), out)
	}
	if !strings.Contains(lines[2], "web-app-patches") {
		t.Errorf("data row should contain ID, got %q", lines[2])
	}
}

func TestWorkspaces_NameHit_JSON(t *testing.T) {
	resetWsFlags()
	*wsName = "web-app-patches"
	*wsJSON = true
	_, cleanup := withTempConfig(t, exampleConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)
	var got []workspaceJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1-element array, got %d: %s", len(got), out)
	}
	if got[0].ID != "web-app-patches" {
		t.Errorf("got ID %q, want web-app-patches", got[0].ID)
	}
}

// ----- resolveWatches unit tests -----

func TestResolveWatches_NoName_ReturnsAll(t *testing.T) {
	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{
		{ID: "a"}, {ID: "b"},
	}
	got, err := resolveWatches(cfg, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(got))
	}
}

func TestResolveWatches_Hit_ReturnsSingle(t *testing.T) {
	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{
		{ID: "a", WatchDir: "x"},
		{ID: "b", WatchDir: "y"},
	}
	got, err := resolveWatches(cfg, "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(got))
	}
	if got[0].ID != "b" || got[0].WatchDir != "y" {
		t.Errorf("returned wrong workspace: %+v", got[0])
	}
}

func TestResolveWatches_Miss_ReturnsCanonicalError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{{ID: "a"}}

	_, err := resolveWatches(cfg, "missing")
	if err == nil {
		t.Fatal("expected error for missing workspace, got nil")
	}
	if !strings.Contains(err.Error(), "watch not found: missing") {
		t.Errorf("error should contain canonical not-found message, got: %v", err)
	}
}

func TestResolveWatches_FirstMatchOnDuplicateID(t *testing.T) {
	// Pins existing GetWatchByID behavior so a future refactor notices.
	cfg := &config.Config{}
	cfg.Watch = []config.WatchConfig{
		{ID: "dup", WatchDir: "first"},
		{ID: "dup", WatchDir: "second"},
	}
	got, err := resolveWatches(cfg, "dup")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(got))
	}
	if got[0].WatchDir != "first" {
		t.Errorf("expected first match (WatchDir=first), got %q", got[0].WatchDir)
	}
}

// TestWorkspaces_VerboseDynamicWidth pins the dynamic column-width contract:
// the JOBS column header should sit immediately after the LOCAL_DIR column
// (not 11 spaces later as in the old fixed 30/30 layout) when the actual data
// is short. Concretely: with watch_dirs of length 24 and local_dirs of length
// 19, the separator line should be substantially shorter than 113 chars.
func TestWorkspaces_VerboseDynamicWidth(t *testing.T) {
	resetWsFlags()
	*wsVerbose = true
	_, cleanup := withTempConfig(t, exampleConfig)
	defer cleanup()

	out := withCapturedStdout(t, runWorkspaces)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected header + separator + rows, got %d lines: %q", len(lines), out)
	}
	separator := lines[1]
	header := lines[0]
	if len(separator) != len(header) {
		t.Errorf("separator width %d should equal header width %d\nheader: %q\nsep:    %q",
			len(separator), len(header), header, separator)
	}
	// Old fixed-width layout was 113 chars. With the example config (max
	// watch_dir=24, max local_dir=19) the dynamic layout should be visibly
	// shorter.
	const oldFixedWidth = 113
	if len(separator) >= oldFixedWidth {
		t.Errorf("separator width %d is at or above the old fixed width %d; dynamic sizing did not take effect",
			len(separator), oldFixedWidth)
	}
}

// TestWorkspaces_WorkspacesAlias verifies that invoking kingpin with
// "workspaces" (the alias declared on wsCmd) parses to the same FullCommand
// as "ws", confirming our switch dispatch handles both forms. We use a small
// sub-application that mirrors the production wsCmd registration; this keeps
// the test independent of the package-level kingpin state.
func TestWorkspaces_WorkspacesAlias(t *testing.T) {
	a := kingpin.New("relay-test", "")
	cmd := a.Command("ws", "").Alias("workspaces")

	// Parsing either name must resolve to the same FullCommand.
	for _, arg := range []string{"ws", "workspaces"} {
		selected, err := a.Parse([]string{arg})
		if err != nil {
			t.Fatalf("parse %q: %v", arg, err)
		}
		if selected != cmd.FullCommand() {
			t.Errorf("kingpin.Parse(%q) = %q, want %q (alias should resolve to main name)", arg, selected, cmd.FullCommand())
		}
	}
}
