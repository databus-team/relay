package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// LockFile 返回 <name>.lock 的路径,与 PidFile/LogFile/SockFile 共用同一 baseDir。
func LockFile(name string) string { return filepath.Join(baseDir(), name+".lock") }

// WithStartLock 在 <name>.lock 上持排他锁执行 fn,把「检查 pid 文件 → 拉起 daemon」
// 串行化。并发 CLI(如 mise 的并行 depends)若各自看到 daemon 未运行,会重复拉起多个
// 进程;后起的进程还会摘掉先起进程的控制 socket,导致控制面与数据面分属两个进程。
func WithStartLock(name string, fn func() error) error {
	return withLockFile(LockFile(name), fn)
}

func withLockFile(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", path, err)
	}
	defer f.Close()

	if err := lockExclusive(f); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer unlockFile(f)
	return fn()
}
