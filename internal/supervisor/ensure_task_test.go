package supervisor

import (
	"testing"

	"github.com/user/relay/internal/portfwd"
)

// 相同 cmd+cwd+ports 的第二次 ensure 必须复用同一 task id,不产生第二个实例;
// 本次请求的 restart 策略覆盖既有记录。
func TestEnsureTaskWithPolicyReusesRunning(t *testing.T) {
	sup, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	t.Cleanup(sup.StopAll)

	first, reused, err := sup.EnsureTaskWithPolicy("sleep 10", "", nil, RestartNo, nil)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if reused {
		t.Fatal("first ensure should create a task")
	}

	second, reused, err := sup.EnsureTaskWithPolicy("sleep 10", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if !reused {
		t.Fatal("second ensure should reuse the running task")
	}
	if second.Meta().ID != first.Meta().ID {
		t.Fatalf("task id = %s, want reuse of %s", second.Meta().ID, first.Meta().ID)
	}
	if got := len(sup.ListTasks(true)); got != 1 {
		t.Fatalf("task count = %d, want 1", got)
	}
	if got := second.Meta().Restart; got != RestartAlways {
		t.Fatalf("restart policy = %q, want %q", got, RestartAlways)
	}
}

// 已有记录但已停止时,ensure 复用原 id 重新拉起。
func TestEnsureTaskWithPolicyRestartsStopped(t *testing.T) {
	sup, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	t.Cleanup(sup.StopAll)

	first, _, err := sup.EnsureTaskWithPolicy("sleep 10", "", nil, RestartNo, nil)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := sup.StopTask(first.Meta().ID); err != nil {
		t.Fatalf("stop task: %v", err)
	}

	second, reused, err := sup.EnsureTaskWithPolicy("sleep 10", "", nil, RestartNo, nil)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if !reused {
		t.Fatal("ensure should reuse the existing record after a stop")
	}
	if second.Meta().ID != first.Meta().ID {
		t.Fatalf("task id = %s, want reuse of %s", second.Meta().ID, first.Meta().ID)
	}
	if got := second.Meta().Status; got != StatusRunning {
		t.Fatalf("status = %s, want %s", got, StatusRunning)
	}
}

// 存在多条同规格记录时,ensure 必须复用运行中的那条:不能为已停止的重复记录重启,
// 把正在服务的实例晾在一边(旧实现按 map 顺序取第一条,存在重复记录时会随机选错)。
func TestEnsureTaskWithPolicyPrefersRunningDuplicate(t *testing.T) {
	sup, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	t.Cleanup(sup.StopAll)

	stopped, err := sup.StartTaskWithPolicy("sleep 10", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start stopped duplicate: %v", err)
	}
	if err := sup.StopTask(stopped.Meta().ID); err != nil {
		t.Fatalf("stop duplicate: %v", err)
	}
	running, err := sup.StartTaskWithPolicy("sleep 10", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("start running duplicate: %v", err)
	}

	got, reused, err := sup.EnsureTaskWithPolicy("sleep 10", "", nil, RestartAlways, nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !reused {
		t.Fatal("ensure should reuse an existing task")
	}
	if got.Meta().ID != running.Meta().ID {
		t.Fatalf("ensure chose %s, want the running duplicate %s", got.Meta().ID, running.Meta().ID)
	}
	if status := stopped.Meta().Status; status != StatusStopped {
		t.Fatalf("stopped duplicate changed to %s; ensure must not restart it", status)
	}
}

// 身份只认 cmd+cwd+ports:端口集合或工作目录不同则新建;端口顺序与省略默认主机不影响匹配。
func TestEnsureTaskWithPolicyIdentity(t *testing.T) {
	sup, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	t.Cleanup(sup.StopAll)

	portsA := portfwd.Spec{BindHost: "127.0.0.1", LocalPort: 18080, RemoteHost: "127.0.0.1", RemotePort: 18080}
	portsB := portfwd.Spec{BindHost: "127.0.0.1", LocalPort: 18081, RemoteHost: "127.0.0.1", RemotePort: 18081}
	base, reused, err := sup.EnsureTaskWithPolicy("sleep 10", "", []portfwd.Spec{portsA, portsB}, RestartNo, nil)
	if err != nil {
		t.Fatalf("base ensure: %v", err)
	}
	if reused {
		t.Fatal("base ensure should create a task")
	}

	// 反序 + 省略绑定主机:规格集合相同,应复用。
	reordered := []portfwd.Spec{
		{LocalPort: 18081, RemoteHost: portfwd.DefaultBind, RemotePort: 18081},
		{LocalPort: 18080, RemoteHost: portfwd.DefaultBind, RemotePort: 18080},
	}
	same, reused, err := sup.EnsureTaskWithPolicy("sleep 10", "", reordered, RestartNo, nil)
	if err != nil {
		t.Fatalf("reordered ensure: %v", err)
	}
	if !reused || same.Meta().ID != base.Meta().ID {
		t.Fatalf("reordered ports should reuse %s, got reused=%v id=%s", base.Meta().ID, reused, same.Meta().ID)
	}

	// 端口集合不同:新建。
	other, reused, err := sup.EnsureTaskWithPolicy("sleep 10", "", []portfwd.Spec{portsA}, RestartNo, nil)
	if err != nil {
		t.Fatalf("different ports ensure: %v", err)
	}
	if reused || other.Meta().ID == base.Meta().ID {
		t.Fatalf("different ports should create a new task, got reused=%v id=%s", reused, other.Meta().ID)
	}

	// 工作目录不同:新建。
	inDir, reused, err := sup.EnsureTaskWithPolicy("sleep 10", t.TempDir(), []portfwd.Spec{portsA, portsB}, RestartNo, nil)
	if err != nil {
		t.Fatalf("different cwd ensure: %v", err)
	}
	if reused || inDir.Meta().ID == base.Meta().ID {
		t.Fatalf("different cwd should create a new task, got reused=%v id=%s", reused, inDir.Meta().ID)
	}
}
