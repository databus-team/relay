package exchange

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestCmdFileWithOpAndPayload(t *testing.T) {
	// Test that Op and Payload fields are preserved during marshal/unmarshal
	cmd := CmdFile{
		ID:      "test-id-123",
		Op:      ConfigSyncOp,
		Payload: "aGVsbG8gd29ybGQ=", // "hello world" base64 encoded
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("Failed to marshal CmdFile: %v", err)
	}

	var parsed CmdFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal CmdFile: %v", err)
	}

	if parsed.ID != cmd.ID {
		t.Errorf("ID mismatch: got %q, want %q", parsed.ID, cmd.ID)
	}
	if parsed.Op != cmd.Op {
		t.Errorf("Op mismatch: got %q, want %q", parsed.Op, cmd.Op)
	}
	if parsed.Payload != cmd.Payload {
		t.Errorf("Payload mismatch: got %q, want %q", parsed.Payload, cmd.Payload)
	}
}

func TestLegacyCmdFileBackwardCompat(t *testing.T) {
	// Test that legacy cmd files without Op/Payload still parse correctly
	legacyJSON := `{"id":"legacy-456","cmd":"echo hello","cwd":"/tmp","timeout":30}`

	var parsed CmdFile
	if err := json.Unmarshal([]byte(legacyJSON), &parsed); err != nil {
		t.Fatalf("Failed to unmarshal legacy CmdFile: %v", err)
	}

	if parsed.ID != "legacy-456" {
		t.Errorf("ID mismatch: got %q, want %q", parsed.ID, "legacy-456")
	}
	if parsed.Cmd != "echo hello" {
		t.Errorf("Cmd mismatch: got %q, want %q", parsed.Cmd, "echo hello")
	}
	if parsed.Cwd != "/tmp" {
		t.Errorf("Cwd mismatch: got %q, want %q", parsed.Cwd, "/tmp")
	}
	if parsed.Timeout != 30 {
		t.Errorf("Timeout mismatch: got %d, want %d", parsed.Timeout, 30)
	}
	// Op and Payload should be empty for legacy files
	if parsed.Op != "" {
		t.Errorf("Op should be empty for legacy file, got %q", parsed.Op)
	}
	if parsed.Payload != "" {
		t.Errorf("Payload should be empty for legacy file, got %q", parsed.Payload)
	}
}

func TestBuildConfigSyncCmd(t *testing.T) {
	configContent := []byte("name: test\nversion: 1\nwatch: []\ninterval_seconds: 60")
	cmd := BuildConfigSyncCmd(configContent)

	if cmd.ID == "" {
		t.Error("BuildConfigSyncCmd should generate a non-empty ID")
	}
	if cmd.Op != ConfigSyncOp {
		t.Errorf("Op mismatch: got %q, want %q", cmd.Op, ConfigSyncOp)
	}

	// Verify base64 decoding
	decoded, err := base64.StdEncoding.DecodeString(cmd.Payload)
	if err != nil {
		t.Fatalf("Failed to decode payload: %v", err)
	}
	if string(decoded) != string(configContent) {
		t.Errorf("Payload content mismatch: got %q, want %q", string(decoded), string(configContent))
	}

	// Cmd/Cwd/Timeout should be empty for config-sync commands
	if cmd.Cmd != "" {
		t.Errorf("Cmd should be empty for config-sync, got %q", cmd.Cmd)
	}
	if cmd.Cwd != "" {
		t.Errorf("Cwd should be empty for config-sync, got %q", cmd.Cwd)
	}
	if cmd.Timeout != 0 {
		t.Errorf("Timeout should be 0 for config-sync, got %d", cmd.Timeout)
	}
}

func TestConfigSyncOpValue(t *testing.T) {
	if ConfigSyncOp != "relay:config-sync" {
		t.Errorf("ConfigSyncOp = %q, want %q", ConfigSyncOp, "relay:config-sync")
	}
}