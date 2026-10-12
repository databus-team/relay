package backend

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	bk "github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
)

func newLogsStreamFixture(t *testing.T) (bk.TaskSupervisorBackend, bk.GitSyncExecBackend) {
	t.Helper()
	SetExecutorRole(true)
	t.Cleanup(func() { SetExecutorRole(false) })
	t.Cleanup(func() { client.CloseAll() })
	withSupervisorBaseDir(t)
	t.Cleanup(Cleanup)

	watchDir := t.TempDir()
	ts, wsURL := newTestHub(t, watchDir)
	t.Cleanup(ts.Close)

	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": "node-1", "watch_dir": ".", "executor": true,
	}); err != nil {
		t.Fatalf("executor backend: %v", err)
	}
	req, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": "node-1", "watch_dir": ".",
	})
	if err != nil {
		t.Fatalf("requester backend: %v", err)
	}
	return req.(bk.TaskSupervisorBackend), req.(bk.GitSyncExecBackend)
}

func startDetachedTask(t *testing.T, gb bk.GitSyncExecBackend, cmd string) string {
	t.Helper()
	var out strings.Builder
	exit, err := gb.ExecStreamWithPorts(context.Background(), "node-1", cmd, "", 120, nil, true, nil, "", func(c bk.ExecChunk) {
		out.WriteString(c.Data)
	})
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if exit != 0 {
		t.Fatalf("detach exit=%d", exit)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out.String()), protocol.ForwardTaskIDMarker))
	if !strings.HasPrefix(id, "t-") {
		t.Fatalf("unexpected task id %q", id)
	}
	return id
}

// 长耗时任务里没有换行的输出(进度条/长时间不换行)必须边跑边到,不能压到换行才吐出来。
func TestEndToEnd_TaskLogsPartialLineRealtime(t *testing.T) {
	tb, gb := newLogsStreamFixture(t)
	ctx := context.Background()

	id := startDetachedTask(t, gb, `i=0; while [ $i -lt 25 ]; do printf '\rprogress %d%%' $((i*4)); sleep 0.2; i=$((i+1)); done; printf '\ndone\n'`)

	start := time.Now()
	var firstAt time.Duration
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan struct{})
	go func() {
		_ = tb.TaskLogs(fctx, "node-1", id, true, 0, func(c bk.ExecChunk) {
			if firstAt == 0 {
				firstAt = time.Since(start)
			}
			if strings.Contains(c.Data, "done") {
				select {
				case <-got:
				default:
					close(got)
				}
			}
		})
	}()

	select {
	case <-got:
	case <-time.After(30 * time.Second):
		t.Fatalf("等不到任务输出")
	}
	cancel()

	if firstAt > 2*time.Second {
		t.Errorf("第一片迟到了 %s:无换行的输出被压到扫描缓冲区里了", firstAt)
	}
}

// 慢消费者只会落后,不能丢行:突发输出 + 每块 2ms 的消费速度下必须一行不少。
func TestEndToEnd_TaskLogsFollowDoesNotDropLines(t *testing.T) {
	tb, gb := newLogsStreamFixture(t)
	ctx := context.Background()

	const lines = 8000
	id := startDetachedTask(t, gb, fmt.Sprintf(
		`i=0; while [ $i -lt %d ]; do echo "LINE-$i-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; i=$((i+1)); done; sleep 2; echo FINAL-MARK`, lines))

	var sb strings.Builder
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = tb.TaskLogs(fctx, "node-1", id, true, 0, func(c bk.ExecChunk) {
			sb.WriteString(c.Data)
			time.Sleep(2 * time.Millisecond)
			if strings.Contains(c.Data, "FINAL-MARK") {
				close(done)
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(180 * time.Second):
		t.Fatalf("超时:只收到 %d 字节", sb.Len())
	}
	cancel()

	body := sb.String()
	missing := 0
	for i := 0; i < lines; i++ {
		if !strings.Contains(body, fmt.Sprintf("LINE-%d-", i)) {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("慢消费者丢了 %d/%d 行", missing, lines)
	}
}

// 分片边界切开多字节字符时不能产生 U+FFFD。
func TestEndToEnd_TaskLogsUTF8AcrossChunks(t *testing.T) {
	tb, gb := newLogsStreamFixture(t)
	ctx := context.Background()

	const lines = 6000
	id := startDetachedTask(t, gb, fmt.Sprintf(
		`i=0; while [ $i -lt %d ]; do echo "中文测试行内容甲乙丙丁戊己庚辛"; i=$((i+1)); done; sleep 1; echo FINAL-MARK`, lines))

	var sb strings.Builder
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = tb.TaskLogs(fctx, "node-1", id, true, 0, func(c bk.ExecChunk) {
			sb.WriteString(c.Data)
			if strings.Contains(c.Data, "FINAL-MARK") {
				close(done)
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(120 * time.Second):
		t.Fatalf("超时")
	}
	cancel()

	body := sb.String()
	if strings.ContainsRune(body, '\uFFFD') {
		t.Errorf("UTF-8 被分片切断后替换成 U+FFFD")
	}
	if n := strings.Count(body, "中文测试行内容甲乙丙丁戊己庚辛"); n != lines {
		t.Errorf("中文行数 = %d, want %d", n, lines)
	}
}
