package server

import (
	"testing"

	"github.com/user/relay/internal/relay/protocol"
)

// TestKnownExecutorLedgerLifecycle 验证已知 executor 台账的生命周期:注册在线 → 断线离线 →
// 重连在线 → 断线后 PruneOfflineExecutors 清空。
func TestKnownExecutorLedgerLifecycle(t *testing.T) {
	s, err := New(Config{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res := protocol.ResourceStats{CPUCores: 4, MemTotalMB: 8000}

	s.RegisterExecutor("e1", "c1", "v1", "linux", "amd64", res)
	led := s.ExecutorLedger()
	if len(led) != 1 || led[0].ID != "e1" || !led[0].Online {
		t.Fatalf("after register: want 1 online e1, got %+v", led)
	}

	// 断线 → 保留在台账,标记离线
	s.cleanupClientState("c1")
	led = s.ExecutorLedger()
	if len(led) != 1 || led[0].Online {
		t.Fatalf("after disconnect: want e1 offline kept, got %+v", led)
	}

	// 重连 → 翻回在线
	s.RegisterExecutor("e1", "c1", "v1", "linux", "amd64", res)
	if !s.ExecutorLedger()[0].Online {
		t.Fatalf("after reconnect: want e1 online, got %+v", s.ExecutorLedger())
	}

	// 断线后清理
	s.cleanupClientState("c1")
	if n := s.PruneOfflineExecutors(); n != 1 {
		t.Fatalf("want prune 1, got %d", n)
	}
	if len(s.ExecutorLedger()) != 0 {
		t.Fatalf("want empty ledger after prune, got %+v", s.ExecutorLedger())
	}
}

// TestKnownExecutorLedgerPersistence 验证台账跨中转重启持久化:先注册再断线,重建中转后
// 离线记录仍在(启动时全部标记离线,待重连翻回在线)。
func TestKnownExecutorLedgerPersistence(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(Config{StateDir: dir})
	s.RegisterExecutor("e1", "c1", "v1", "linux", "amd64", protocol.ResourceStats{CPUCores: 2})
	s.cleanupClientState("c1") // 离线并落盘

	s2, err := New(Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	led := s2.ExecutorLedger()
	if len(led) != 1 || led[0].ID != "e1" || led[0].Online {
		t.Fatalf("want persisted offline e1 (loaded as offline), got %+v", led)
	}
}

// TestKnownExecutorLedgerHubKept 验证 hub 自身的在线 executor 也计入台账。
func TestKnownExecutorLedgerHubKept(t *testing.T) {
	s, _ := New(Config{StateDir: t.TempDir()})
	s.RegisterExecutor("hub", "hub-client", "v1", "linux", "amd64", protocol.ResourceStats{CPUCores: 4})
	led := s.ExecutorLedger()
	if len(led) != 1 || led[0].ID != "hub" || !led[0].Online {
		t.Fatalf("want hub online in ledger, got %+v", led)
	}
}
