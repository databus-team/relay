package daemon

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 并发取同一把启动锁必须串行:否则两个 CLI 会各自看到 daemon 未运行并重复拉起。
func TestWithLockFileSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")

	var mu sync.Mutex
	active, maxActive := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := withLockFile(path, func() error {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()

				time.Sleep(50 * time.Millisecond)

				mu.Lock()
				active--
				mu.Unlock()
				return nil
			}); err != nil {
				t.Errorf("withLockFile: %v", err)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lock acquisition timed out")
	}
	if maxActive != 1 {
		t.Fatalf("max concurrent lock holders = %d, want 1", maxActive)
	}
}
