# relay detach 端口转发 — node 承载实现(rev5 落定)

**架构**:把 dockerd 语义落到本机 node。CLI 与 node 用 unix socket 通信(对齐 docker CLI→dockerd 的 socket API)。

```
relay run -d -p <spec>  ──unix socket──> 本机 relay node (agent, dockerd 角色)
                                            |- 持有端口映射表
                                            |- net.Listen 本地端口 + 冲突检测
                                            |- TunnelOpenPortForward 连远端 executor
                                            |- 每 spec 一个转发 goroutine (docker-proxy)
```

## 待办(按依赖顺序)

### Unit 1: node 侧转发持有者 + unix socket 监听
- **新增** `~/.relay/forward.sock` 监听(在 node agent 主循环启动),收 JSON 行式规则。
- node 持有:规则 map(task_id -> specs),每 spec net.Listen + TunnelOpenPortForward + 转发 pump(复用 handlePortForwardConn)。
- **验**:node 起后 socket 就绪;收到规则后本地端口 listen。

### Unit 2: run -d -p 接入(先任务后登记,经 socket)
- runExec detach + pfSpecs:ensure node(不在则 daemonStart) → 启动远端任务(wait TaskID via onChunk) → 逐 spec 经 socket 发 register(task_id) → 打印 TaskID。
- 同端口冲突:register 时 node 返回错误,拒绝该 spec。
- **验**:`relay run --node hub -d -p 18083:8899 '<server>'` 后 lsof 有监听 + curl 通。

### Unit 3: relay stop 联动
- runStop:远端 stop 成功 → 经 socket 发 unregister(task_id) → node 移除对应转发。
- **验**:`relay stop <taskid>` 后 lsof 不再监听。

### Unit 4: relay ps -a 增强
- runPs 汇总展示本机转发规则(含失效标注)。
- **验**:ps -a 列多条。

关键复用:`cmd/relay/portforward.go` 的 `handlePortForwardConn`(转发泵)、`internal/relay/backend/relay.go:415` `TunnelOpenPortForward`、`internal/daemon` 的 `daemonStartArgs`。
