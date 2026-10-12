package backend

import (
	"context"
	"testing"
	"time"
)

// `relay ps` 的出口探测成本:原来问的是「整座部署」,应答要等最慢的那次探针;改成只问自己
// 那几个隧道出口后,无关的慢执行方不再计费。布局与 status_probe_test.go 一致:
// 5 个健康执行方 + 1 个在线但不应答探针的 eg-hung。
//
//	go test ./internal/relay/backend/ -run '^$' -bench 'BenchmarkPsEgressQuery' -benchtime=3x
func BenchmarkPsEgressQuery(b *testing.B) {
	rb := newStatusProbeFixture(b)

	b.Run("whole-deployment-1.5s-cap", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			_, _ = rb.StatusWithUsage(ctx, false)
			cancel()
		}
	})

	b.Run("targeted-egress-800ms-budget", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = rb.StatusTargeted(ctx, false, []string{"eg-fast"}, 800*time.Millisecond)
			cancel()
		}
	})

	// 最坏情况:出口自己回不动探针。有预算就不至于拖到本地上限,还拿得到一个诚实的 notready。
	b.Run("targeted-unresponsive-egress-800ms-budget", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = rb.StatusTargeted(ctx, false, []string{"eg-hung"}, 800*time.Millisecond)
			cancel()
		}
	})
}
