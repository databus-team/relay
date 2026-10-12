package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 无换行的输出必须即时送达,不能压到换行才吐出来。
func TestSupervisor_StreamLogs_PartialLineRealtime(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	// 无换行的输出先到,3 秒后才补换行。
	task, err := sup.StartTask(`printf 'partial-1'; sleep 3; printf 'partial-2\n'`, "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var sb strings.Builder
	var firstAt time.Duration
	gotFull := make(chan struct{})
	start := time.Now()
	go func() {
		_ = sup.StreamLogs(ctx, task.Meta().ID, true, 10, func(chunk string) {
			mu.Lock()
			if firstAt == 0 {
				firstAt = time.Since(start)
			}
			sb.WriteString(chunk)
			full := strings.Contains(sb.String(), "partial-2")
			mu.Unlock()
			if full {
				select {
				case <-gotFull:
				default:
					close(gotFull)
				}
			}
		})
	}()

	select {
	case <-gotFull:
	case <-time.After(8 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("等不到完整输出,已收到 %q", sb.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if got := sb.String(); got != "partial-1partial-2\n" {
		t.Errorf("拼接结果 = %q, want %q", got, "partial-1partial-2\n")
	}
	// 第一片必须在换行之前到达。
	if firstAt > 2*time.Second {
		t.Errorf("无换行的输出迟到了 %s:按行切会一直压到换行,必须按块即时送达", firstAt)
	}
}

// 历史只回放最后 tailLines 行。
func TestSupervisor_StreamLogs_TailIsSuffix(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask(`printf 'l1\nl2\nl3\nl4\nl5\n'; sleep 10`, "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(task.Meta().LogPath); err == nil && strings.Count(string(b), "\n") >= 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var sb strings.Builder
	go func() {
		_ = sup.StreamLogs(ctx, task.Meta().ID, true, 2, func(chunk string) { sb.WriteString(chunk) })
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()

	if got := sb.String(); got != "l4\nl5\n" {
		t.Errorf("tail=2 回放 = %q, want %q", got, "l4\nl5\n")
	}
}

// 尾部读取不整读文件,且按块发送。
func TestSupervisor_StreamLogs_TailBigFile(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}

	task, err := sup.StartTask("echo start; sleep 30", "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	logPath := task.Meta().LogPath
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 50000; i++ { // ~5MB
		if _, err := f.WriteString(line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_ = f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	var chunks int
	var lines int
	err = sup.StreamLogs(ctx, task.Meta().ID, false, 100, func(chunk string) {
		chunks++
		lines += strings.Count(chunk, "\n")
	})
	if err != nil {
		t.Fatalf("stream logs: %v", err)
	}
	elapsed := time.Since(start)

	if lines != 100 {
		t.Errorf("tail=100 回放 %d 行, want 100", lines)
	}
	// 按块合并后应只有个位数分片。
	if chunks > 10 {
		t.Errorf("tail=100 发了 %d 个分片,预期按块合并(<=10)", chunks)
	}
	if elapsed > 2*time.Second {
		t.Errorf("tail=100 耗时 %s,过慢", elapsed)
	}
}

// 无换行的输出也要立刻落盘,跟随者从文件里才读得到。
func TestSupervisor_LogFileKeepsPartialBytes(t *testing.T) {
	dir := t.TempDir()
	sup, err := New(dir)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	task, err := sup.StartTask(`printf 'no-newline-yet'; sleep 5`, "", nil)
	if err != nil {
		t.Fatalf("start task: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(dir, task.Meta().ID, "run.log")); err == nil &&
			strings.Contains(string(b), "no-newline-yet") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(dir, task.Meta().ID, "run.log"))
	t.Errorf("run.log 里没有无换行的输出: %q", string(b))
}

func TestTailStartOffset(t *testing.T) {
	tests := []struct {
		name    string
		content string
		lines   int
		want    string
	}{
		{name: "最后一行", content: "a\nb\nc\n", lines: 1, want: "c\n"},
		{name: "最后两行", content: "a\nb\nc\n", lines: 2, want: "b\nc\n"},
		{name: "无尾换行", content: "a\nb\nc", lines: 1, want: "c"},
		{name: "无尾换行两行", content: "a\nb\nc", lines: 2, want: "b\nc"},
		{name: "只有一行", content: "only", lines: 3, want: "only"},
		{name: "空文件", content: "", lines: 3, want: ""},
		{name: "空行", content: "a\n\n\n", lines: 1, want: "\n"},
		{name: "全量", content: "a\nb\n", lines: 0, want: "a\nb\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.log")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			st, err := f.Stat()
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			off := tailStartOffset(f, st.Size(), tt.lines)
			if off < 0 || off > st.Size() {
				t.Fatalf("offset %d 越界(文件 %d 字节)", off, st.Size())
			}
			if got := tt.content[off:]; got != tt.want {
				t.Errorf("tailStartOffset(%q, %d) = %d → %q, want %q", tt.content, tt.lines, off, got, tt.want)
			}
		})
	}
}
