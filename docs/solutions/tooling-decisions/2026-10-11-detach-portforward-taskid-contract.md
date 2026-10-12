---
module: relay-cli
component: tooling
problem_type: tooling_decision
category: tooling-decisions
date: 2026-10-11
severity: low
title: detach 端口转发的 task ID 契约——marker 前缀 + 单向向后兼容
applies_when:
  - 决定 relay run -d -p 本地端口保持可达时,executor 如何把 detach 任务 ID 交回给 CLI
  - 评估 detach stdout 契约向后兼容(滚动升级期)是否值得单独处理
  - 在 docker CLI/dockerd unix socket 范式下设计 CLI->node 控制通道
related_components: [relay-cli, relay-node, relay-backend, relay-protocol]
---

# detach 端口转发的 task ID 契约——marker 前缀 + 单向向后兼容

## Context

`relay run -d [端口转发]` 在 detach(后台)模式下,本地端口需要在 CLI 退出后仍保持可达。为此本机常驻 `relay node`(agent 角色,等价 dockerd 的角色)持有本地监听与远端转发;CLI 经 unix socket(默认 `~/.relay/forward.sock`)投递转发规则给 node。

核心问题:CLI 拿到 detach 任务 ID 后才能向 node 注册转发。任务 ID 由远端 executor 回写,原本是**裸 ID 行**(形如 `t-<hash>`),CLI 从 streamed stdout 的第一个非空行兜底捕获。这个启发式不可靠——任务自身可能先打印输出,导致捕获到错误的 ID,端口转发注册就静默失败。

## Guidance

1. **executor 侧回写带 marker 前缀的 ID 行**:`sess.Write(true, fmt.Sprintf("%s%s\n", protocol.ForwardTaskIDMarker, task.Meta().ID))`。`ForwardTaskIDMarker = "__relay_task_id__"` 定义在 `internal/relay/protocol/message.go`,跨包共享。
2. **CLI 侧 marker 精确解析优先**:命中 marker 前缀时 `TrimPrefix` 后精确取 ID,并**抑制该行回显**给用户 stdout(它是控制信息,不是任务输出)。
3. **单向向后兼容(方案 A)**:CLI 若没命中 marker(旧 executor 回写裸 ID 行),回退到首个非空 stdout 行捕获。保持 `detachTaskID == ""` 才捕获,不清零已捕获值。
4. **detach 无 -p 时也要回显 ID**:否则用户拿不到 `--detach` 承诺返回的 TaskID,无法 `relay stop`。detach 结束处单独 echo 一次,勿与转发行重复。

## Why This Matters

- **任务 ID 捕获正确性是注册生命线**:ID 错则 node 不注册、本地端口不可达,用户还拿不到任何报错。marker 前缀让捕获从"猜首个非空行"变为"精确解析",消除对任务输出顺序的依赖。
- **单向后兼容比 out-of-band 更鲁棒**:把 ID 走 `ExecResponse.Stdout`(out-of-band)要求 CLI 与 executor **强版本绑定同版**,反而更脆弱。当前 CLI 同时识别 marker 和裸 ID 行,对"新 CLI + 旧 executor"这一混合态是自洽兼容的。
- **detach 任务输出不会混入 ID 流**:executor 把任务 stdout/stderr 重定向到任务日志(`task.pipeToLog`),exec session 只回写 ID 行。所以 CLI 的 onChunk 流在 detach exec 里**只携带 ID chunk**,旧式"首个非空行 = ID"在旧 executor 上也安全。

## When to Apply

- 任何版本探测(rolling upgrade / hash 不匹配)时,new CLI + old executor 混合态都要能干净降级,而不是报错丢数据。
- 设计 CLI->node 控制通道时,采用 docker CLI->dockerd 的 unix socket + JSON 消息范式,不引入新子命令/新入口/新 daemon。

## Examples

捕获逻辑(抽取为纯函数 `captureDetachTaskID`,便于单测):

```go
func captureDetachTaskID(data string, current string) (captured string, echo bool, suppress bool) {
	if strings.HasPrefix(data, protocol.ForwardTaskIDMarker) {
		id := strings.TrimSpace(strings.TrimPrefix(data, protocol.ForwardTaskIDMarker))
		if id != "" {
			return id, true, true
		}
		return current, false, false
	}
	if id := strings.TrimSpace(data); id != "" {
		// 旧版 executor 回写的 ID 行也是控制信息,应抑制不回显。
		return id, true, true
	}
	return current, false, false
}
```

## 已知边界(报告为接受,不修)

**#7(api-contract):marker 前缀是向后不兼容的 detach stdout 契约。** 影响仅限"新 executor + 旧消费方"(旧 CLI/logs 读到带 `__relay_task_id__` 前缀的行)这一种混合态。

接受理由:relay 是**全端同版本一起升级**的(transit/hub/extranet 同 `v0.2.2+ecdc467` 统一编排部署),不存在长期跨代混合。全量升级后消费方都是新版,marker 正常。out-of-band 方案反而要求 CLI 与 executor 强版本绑定,更脆弱。因此**滚动升级的一个瞬时窗口不构成单独改代码的充分理由**;防御靠部署流程"全端一起升",而非代码强制。