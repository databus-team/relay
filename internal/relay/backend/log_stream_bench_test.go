package backend

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bk "github.com/user/relay/internal/backend"
	"github.com/user/relay/internal/relay/client"
	"github.com/user/relay/internal/relay/protocol"
	"github.com/user/relay/internal/relay/server"
)

// 任务日志链路的基准。跑法:
//
//	go test ./internal/relay/backend/ -run '^$' -bench 'BenchmarkTaskLogs' -benchtime=3x
//
// 只用到两个版本都有的接口,便于在改动前后各跑一次做对比。

const benchNode = "bench-node"

func benchHub(b *testing.B) (bk.TaskSupervisorBackend, bk.GitSyncExecBackend, string) {
	b.Helper()
	SetExecutorRole(true)
	b.Cleanup(func() { SetExecutorRole(false) })
	b.Cleanup(func() { client.CloseAll() })

	baseDir := b.TempDir()
	prevDir := supervisorBaseDir
	supervisorBaseDir = baseDir
	b.Cleanup(func() { supervisorBaseDir = prevDir })
	b.Cleanup(Cleanup)

	srv, err := server.New(server.Config{
		Addr:      ":0",
		WatchDirs: []server.WatchDirConfig{{ID: "bench", Dir: b.TempDir()}},
		Auth:      server.AuthConfig{Type: "token", Tokens: []string{"tok-exe", "tok-req"}},
	})
	if err != nil {
		b.Fatalf("create server: %v", err)
	}
	ts := httptest.NewServer(srv)
	b.Cleanup(ts.Close)

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/relay"
	if _, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-exe", "executor_id": benchNode, "watch_dir": ".", "executor": true,
	}); err != nil {
		b.Fatalf("executor backend: %v", err)
	}
	req, err := NewRelayBackend(map[string]interface{}{
		"url": wsURL, "token": "tok-req", "executor_id": benchNode, "watch_dir": ".",
	})
	if err != nil {
		b.Fatalf("requester backend: %v", err)
	}
	return req.(bk.TaskSupervisorBackend), req.(bk.GitSyncExecBackend), baseDir
}

func benchStartTask(b *testing.B, gb bk.GitSyncExecBackend, cmd string) string {
	b.Helper()
	var out strings.Builder
	exit, err := gb.ExecStreamWithPorts(context.Background(), benchNode, cmd, "", 300, nil, true, nil, "", func(c bk.ExecChunk) {
		out.WriteString(c.Data)
	})
	if err != nil {
		b.Fatalf("detach: %v", err)
	}
	if exit != 0 {
		b.Fatalf("detach exit=%d", exit)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out.String()), protocol.ForwardTaskIDMarker))
	if !strings.HasPrefix(id, "t-") {
		b.Fatalf("bad task id %q", id)
	}
	return id
}

// 无换行的输出(进度条/长时间不换行)从「产生」到「跟随端可见」的时延。
func BenchmarkTaskLogsFirstChunkLatency(b *testing.B) {
	tb, gb, _ := benchHub(b)
	var total time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := benchStartTask(b, gb, `printf 'no-newline'; sleep 1; printf ' tail\n'`)

		ctx, cancel := context.WithCancel(context.Background())
		first := make(chan time.Duration, 1)
		start := time.Now()
		go func() {
			fired := false
			_ = tb.TaskLogs(ctx, benchNode, id, true, 0, func(c bk.ExecChunk) {
				if !fired {
					fired = true
					first <- time.Since(start)
				}
			})
		}()
		select {
		case d := <-first:
			total += d
		case <-time.After(30 * time.Second):
			cancel()
			b.Fatalf("跟随端 30s 内没看到任何输出")
		}
		cancel()
	}
	b.StopTimer()
	b.ReportMetric(float64(total.Microseconds())/float64(b.N)/1000, "ms-first-chunk")
}

// 突发输出的投递:固定 20000 行,快消费者,量到收完最后一行为止。
func BenchmarkTaskLogsBurstDelivery(b *testing.B) {
	tb, gb, _ := benchHub(b)
	payload := strings.Repeat("x", 60)
	cmd := fmt.Sprintf(`i=0; while [ $i -lt 20000 ]; do echo "LINE-$i-%s"; i=$((i+1)); done; sleep 0.3; echo FINAL-MARK`, payload)

	var totalElapsed, totalAfterFirst time.Duration
	var totalChunks, totalLines, completed int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := benchStartTask(b, gb, cmd)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var chunks, lines int
		var firstAt time.Duration
		start := time.Now()
		go func() {
			_ = tb.TaskLogs(ctx, benchNode, id, true, 0, func(c bk.ExecChunk) {
				if chunks == 0 {
					firstAt = time.Since(start)
				}
				chunks++
				lines += strings.Count(c.Data, "\n")
				if strings.Contains(c.Data, "FINAL-MARK") {
					select {
					case <-done:
					default:
						close(done)
					}
				}
			})
		}()
		sawEnd := true
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			sawEnd = false
		}
		elapsed := time.Since(start)
		cancel()

		totalElapsed += elapsed
		totalAfterFirst += elapsed - firstAt
		totalChunks += chunks
		totalLines += lines
		if sawEnd {
			completed++
		}
	}
	b.StopTimer()

	n := float64(b.N)
	b.ReportMetric(float64(totalElapsed.Microseconds())/n/1000, "ms-to-end")
	b.ReportMetric(float64(totalAfterFirst.Microseconds())/n/1000, "ms-after-first")
	b.ReportMetric(float64(totalChunks)/n, "ws-msgs")
	b.ReportMetric(float64(totalLines)/n, "lines")
	b.ReportMetric(float64(completed)/n*100, "%complete")
}

// 慢消费者(每片 2ms)下的完整性:量丢了多少行。
func BenchmarkTaskLogsSlowConsumerLoss(b *testing.B) {
	tb, gb, _ := benchHub(b)
	cmd := `i=0; while [ $i -lt 8000 ]; do echo "LINE-$i-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; i=$((i+1)); done; sleep 2; echo FINAL-MARK`

	var totalLines int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := benchStartTask(b, gb, cmd)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var body strings.Builder
		go func() {
			_ = tb.TaskLogs(ctx, benchNode, id, true, 0, func(c bk.ExecChunk) {
				body.WriteString(c.Data)
				time.Sleep(2 * time.Millisecond)
				if strings.Contains(c.Data, "FINAL-MARK") {
					select {
					case <-done:
					default:
						close(done)
					}
				}
			})
		}()
		select {
		case <-done:
		case <-time.After(120 * time.Second):
		}
		cancel()

		got := body.String()
		lines := 0
		for j := 0; j < 8000; j++ {
			if strings.Contains(got, fmt.Sprintf("LINE-%d-", j)) {
				lines++
			}
		}
		totalLines += lines
	}
	b.StopTimer()
	b.ReportMetric(8000-float64(totalLines)/float64(b.N), "lost-lines")
	b.ReportMetric(float64(totalLines)/float64(b.N), "lines")
}

// 大日志取尾部:20MB run.log,tail=100。
func BenchmarkTaskLogsTailRead(b *testing.B) {
	tb, gb, baseDir := benchHub(b)
	id := benchStartTask(b, gb, "echo start; sleep 600")

	logPath := filepath.Join(baseDir, id, "run.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		b.Fatalf("open log: %v", err)
	}
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 200000; i++ {
		if _, err := f.WriteString(line); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	_ = f.Close()
	if st, err := os.Stat(logPath); err == nil {
		b.Logf("run.log = %d 字节", st.Size())
	}

	var totalElapsed time.Duration
	var totalChunks, totalLines int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var chunks, lines int
		start := time.Now()
		if err := tb.TaskLogs(ctx, benchNode, id, false, 100, func(c bk.ExecChunk) {
			chunks++
			lines += strings.Count(c.Data, "\n")
		}); err != nil {
			cancel()
			b.Fatalf("tail: %v", err)
		}
		cancel()
		totalElapsed += time.Since(start)
		totalChunks += chunks
		totalLines += lines
	}
	b.StopTimer()
	n := float64(b.N)
	b.ReportMetric(float64(totalElapsed.Microseconds())/n/1000, "ms")
	b.ReportMetric(float64(totalChunks)/n, "ws-msgs")
	b.ReportMetric(float64(totalLines)/n, "lines")

	_ = tb.TaskStop(context.Background(), benchNode, id)
}

// 全量读取大日志(20MB)：逐行发消息 vs 按块合并。
func BenchmarkTaskLogsFullRead(b *testing.B) {
	tb, gb, baseDir := benchHub(b)
	id := benchStartTask(b, gb, "echo start; sleep 600")

	logPath := filepath.Join(baseDir, id, "run.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		b.Fatalf("open log: %v", err)
	}
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 200000; i++ {
		if _, err := f.WriteString(line); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	_ = f.Close()

	var totalElapsed time.Duration
	var totalChunks, totalLines int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		var chunks, lines int
		start := time.Now()
		if err := tb.TaskLogs(ctx, benchNode, id, false, 0, func(c bk.ExecChunk) {
			chunks++
			lines += strings.Count(c.Data, "\n")
		}); err != nil {
			cancel()
			b.Fatalf("full read: %v", err)
		}
		cancel()
		totalElapsed += time.Since(start)
		totalChunks += chunks
		totalLines += lines
	}
	b.StopTimer()
	n := float64(b.N)
	b.ReportMetric(float64(totalElapsed.Microseconds())/n/1000, "ms")
	b.ReportMetric(float64(totalChunks)/n, "ws-msgs")
	b.ReportMetric(float64(totalLines)/n, "lines")

	_ = tb.TaskStop(context.Background(), benchNode, id)
}
