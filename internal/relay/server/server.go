package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
)

// statusProbeTimeout 中转等待执行方探针回包的上限。单次 `relay status` 内对执行方测 RTT,
// 超时即把「中转→执行方」段标为不可用(整条命令仍成功),不无限阻塞请求方连接。
const statusProbeTimeout = 5 * time.Second

type Server struct {
	addr      string
	watchDirs map[string]string
	watchCfgs []WatchDirConfig
	clients   map[string]*Client
	clientMu  sync.RWMutex
	upgrader  websocket.Upgrader
	serverID  string
	auth      AuthConfig
	tls       TLSConfig

	subs  map[string]map[string]bool
	subMu sync.RWMutex

	// executors 记录每个 executor_id 上注册的执行方客户端(远端 watcher)。
	executors map[string]string
	// executorVers 记录每个 executor 执行方的 relay 构建版本,供 relay version 对比。
	executorVers map[string]string
	executorMu   sync.RWMutex
	// reqOwner 记录被转发的 exec 请求(reqID)归属的请求方客户端,用于把执行方回流帧转回去。
	reqOwner map[string]string
	reqMu    sync.RWMutex

	// pending 记录中转发起的「待回包」探针(probeID → 回包通道):中转向执行方发探针后在此
	// 带超时等 MsgPong 回包,从而测得 transit→executor 段时延(镜像客户端的 pending 用法)。
	pending   map[string]chan protocol.Message
	pendingMu sync.RWMutex

	// pushRelay 记录被转发给执行方的流式 push(streamID → 执行方/请求方),用于把内容帧原样透传。
	pushRelay   map[string]pushRelayInfo
	pushRelayMu sync.RWMutex

	// tunnels 记录在转的隧道流(streamID → 两端):请求方(本地 client)与执行方(client)。
	// 每个隧道独立、互不串扰,天然支持多 executor 多隧道并行(多 watch 一对一)。
	tunnels       map[string]*tunnelEntry
	tunnelMu      sync.RWMutex
	tunnelEnabled bool // 隧道通道:KTD4 默认关闭,显式开启才放行 MsgTunnel*
	maxTunnels    int  // 并发隧道上限(0/缺省=默认 256),防 token 持有者无界开隧道(DoS)

	// upgradeSwap 由接线方(如 cmd/relay)注入的「自升级换装」闭包:入参为已通过 sha256
	// 校验与自检、落盘好的新二进制路径;闭包内做停旧/.prev 备份/替换/重启。仅为 nil 时
	// (如测试)升级通道只回执 ACK 并清理暂存,不真实换装。
	upgradeSwap func(newBin string)
	// upgradeMu 串行化换装:仅允许一条 upgrade 会话到达 ReplaceBinaryWithKeep + 重启,
	// 防止两个并发 token 会话在同一 os.Executable 上竞争(双 Exec / .prev 竞争)。
	upgradeMu sync.Mutex
}

// pushRelayInfo 一次被中转发出的流式 push 的路由信息。
type pushRelayInfo struct {
	executorID  string // 接收内容流与执行 jobs 的远端执行方
	reqID       string // 关联回流的请求 ID(与 reqOwner 一致)
	requesterID string // 发起 push 的请求方
}

// tunnelEntry 一条在转隧道的两端 client ID。字节流按 streamID 在中转两侧原样透传,不落盘。
type tunnelEntry struct {
	requesterID string // 隧道本地请求方(经本地 SOCKS5 的 CLI)
	executorID  string // 隧道出口执行方(白名单校验 + 真实连接)
}

// defaultMaxTunnels 是并发隧道上限的缺省值(Config.MaxTunnels<=0 时取用)。
// 限制 token 持有者可同时维持的隧道数,防止未授权的内网扫描/资源耗尽(镜像 KTD7)。
const defaultMaxTunnels = 256

type Config struct {
	Addr          string
	TLS           TLSConfig
	Auth          AuthConfig
	WatchDirs     []WatchDirConfig
	TunnelEnabled bool // KTD4:隧道通道默认关闭,显式开启才放行 MsgTunnel*
	MaxTunnels    int  // 并发隧道上限(<=0 时用 defaultMaxTunnels)
}

type TLSConfig struct {
	Enabled  bool
	CertFile string
	KeyFile  string
}

type AuthConfig struct {
	Type   string
	Tokens []string
}

type WatchDirConfig struct {
	ID  string        `yaml:"id"`
	Dir string        `yaml:"dir"`
	TTL time.Duration `yaml:"ttl"`
}

func New(cfg Config) (*Server, error) {
	maxTunnels := cfg.MaxTunnels
	if maxTunnels <= 0 {
		maxTunnels = defaultMaxTunnels
	}
	s := &Server{
		addr:          cfg.Addr,
		watchDirs:     make(map[string]string),
		watchCfgs:     cfg.WatchDirs,
		clients:       make(map[string]*Client),
		serverID:      "relay-" + uuid.New().String()[:8],
		auth:          cfg.Auth,
		tls:           cfg.TLS,
		subs:          make(map[string]map[string]bool),
		executors:     make(map[string]string),
		executorVers:  make(map[string]string),
		reqOwner:      make(map[string]string),
		pending:       make(map[string]chan protocol.Message),
		pushRelay:     make(map[string]pushRelayInfo),
		tunnels:       make(map[string]*tunnelEntry),
		tunnelEnabled: cfg.TunnelEnabled,
		maxTunnels:    maxTunnels,
	}

	for _, wd := range cfg.WatchDirs {
		s.watchDirs[wd.ID] = wd.Dir
	}

	s.upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	log.Printf("[server] up: id=%s addrs=%d dirs=%v tunnel_enabled=%t max_tunnels=%d",
		s.serverID, len(s.watchDirs), watchIDs(s.watchDirs), cfg.TunnelEnabled, maxTunnels)

	return s, nil
}

// watchIDs 抽取启动摘要里要展示的 watch 登目录;顺序固定便于日志对齐。
func watchIDs(m map[string]string) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Server) StartFileWatcher(ctx context.Context) error {
	fw, err := NewFileWatcher(s)
	if err != nil {
		return err
	}
	go fw.Start(ctx)
	return nil
}

func (s *Server) StartTTL(ctx context.Context) {
	go s.ttlCleanupLoop(ctx)
}

func (s *Server) ttlCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupExpiredFiles()
		}
	}
}

func (s *Server) cleanupExpiredFiles() {
	now := time.Now()
	for _, cfg := range s.watchCfgs {
		if cfg.TTL <= 0 {
			continue
		}

		entries, err := os.ReadDir(cfg.Dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				continue
			}

			if now.Sub(info.ModTime()) > cfg.TTL {
				filePath := filepath.Join(cfg.Dir, entry.Name())
				if err := os.Remove(filePath); err != nil {
					log.Printf("[ttl] failed to remove %s: %v", filePath, err)
				} else {
					log.Printf("[ttl] expired: %s (age: %v, ttl: %v)", filePath, now.Sub(info.ModTime()).Round(time.Second), cfg.TTL)
				}
			}
		}
	}
}

// SetUpgradeSwap 注入换装闭包(自检通过后执行停旧/.prev/换装/重启)。可为 nil。
func (s *Server) SetUpgradeSwap(fn func(newBin string)) {
	s.upgradeSwap = fn
}

func (s *Server) Serve(ctx context.Context) error {
	httpServer := &http.Server{Addr: s.addr, Handler: s}

	fw, err := NewFileWatcher(s)
	if err != nil {
		return fmt.Errorf("create file watcher: %w", err)
	}

	go func() {
		if err := fw.Start(ctx); err != nil {
			log.Printf("[server] file watcher error: %v", err)
		}
	}()

	s.StartTTL(ctx)

	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	if s.tls.Enabled {
		log.Printf("[server] TLS enabled: %s", s.tls.CertFile)
		return httpServer.ListenAndServeTLS(s.tls.CertFile, s.tls.KeyFile)
	}

	return httpServer.ListenAndServe()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/relay" && r.URL.Path != "/relay/" {
		http.NotFound(w, r)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	s.handleConnection(conn)
}

func (s *Server) handleConnection(conn *websocket.Conn) {
	client := NewClient(conn, s)

	var msg protocol.Message
	if err := conn.ReadJSON(&msg); err != nil {
		conn.Close()
		return
	}

	if msg.Type != protocol.MsgConnect {
		conn.WriteJSON(protocol.Message{
			Type:    protocol.MsgConnectAck,
			ID:      uuid.New().String(),
			Payload: protocol.ConnectResponse{OK: false, Error: "first message must be connect"},
		})
		conn.Close()
		return
	}

	payload, _ := msg.Payload.(map[string]interface{})
	token := toString(payload["token"])

	if !s.validateToken(token) {
		log.Printf("[session] rejected connect from %s: invalid token", conn.RemoteAddr())
		conn.WriteJSON(protocol.Message{
			Type:    protocol.MsgConnectAck,
			ID:      uuid.New().String(),
			Payload: protocol.ConnectResponse{OK: false, Error: "invalid token"},
		})
		conn.Close()
		return
	}

	clientID := toString(payload["client_id"])
	if clientID == "" {
		clientID = uuid.New().String()
	}
	client.SetID(clientID)

	s.clientMu.Lock()
	s.clients[clientID] = client
	s.clientMu.Unlock()

	log.Printf("[session] %s connected (id=%s)", conn.RemoteAddr(), clientID)

	watchDirs := make([]string, 0, len(s.watchDirs))
	for id := range s.watchDirs {
		watchDirs = append(watchDirs, id)
	}

	conn.WriteJSON(protocol.Message{
		Type:      protocol.MsgConnectAck,
		ID:        uuid.New().String(),
		RequestID: msg.ID,
		Payload:   protocol.ConnectResponse{OK: true, ServerID: s.serverID, ServerVersion: 1, WatchDirs: watchDirs},
	})

	go client.Run()
	<-client.CloseCh()

	client.cleanupStreams() // 清理断连时未完成流的临时文件(如自升级 .incoming)
	s.UnsubscribeAll(clientID)
	s.cleanupClientState(clientID)
	s.removeClientTunnels(clientID) // 拆除该客户端所在隧道,向对端发 MsgTunnelEnd

	s.clientMu.Lock()
	delete(s.clients, clientID)
	s.clientMu.Unlock()

	log.Printf("[session] %s disconnected (id=%s)", conn.RemoteAddr(), clientID)
}

func (s *Server) validateToken(token string) bool {
	if len(s.auth.Tokens) == 0 {
		return true
	}
	for _, t := range s.auth.Tokens {
		if t == token {
			return true
		}
	}
	return false
}

func (s *Server) GetWatchDir(watchID string) (string, bool) {
	dir, ok := s.watchDirs[watchID]
	return dir, ok
}

func (s *Server) SendTo(clientID string, msg protocol.Message) error {
	s.clientMu.RLock()
	client, ok := s.clients[clientID]
	s.clientMu.RUnlock()

	if !ok {
		return fmt.Errorf("client not found: %s", clientID)
	}

	return client.Send(msg)
}

func (s *Server) Subscribe(clientID, watchID string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.subs[watchID] == nil {
		s.subs[watchID] = make(map[string]bool)
	}
	s.subs[watchID][clientID] = true
}

func (s *Server) Unsubscribe(clientID, watchID string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.subs[watchID] != nil {
		delete(s.subs[watchID], clientID)
		if len(s.subs[watchID]) == 0 {
			delete(s.subs, watchID)
		}
	}
}

func (s *Server) UnsubscribeAll(clientID string) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for watchID, clients := range s.subs {
		delete(clients, clientID)
		if len(clients) == 0 {
			delete(s.subs, watchID)
		}
	}
}

// RegisterExecutor 将 clientID 注册为 executorID 的执行方,并记录其 relay 构建版本。
// 返回(旧执行方连接, 是否发生了覆盖),供上层做替换告警。
func (s *Server) RegisterExecutor(executorID, clientID, version string) (prev string, replaced bool) {
	s.executorMu.Lock()
	defer s.executorMu.Unlock()
	prev = s.executors[executorID]
	replaced = prev != "" && prev != clientID
	s.executors[executorID] = clientID
	if version != "" {
		s.executorVers[executorID] = version
	} else {
		// 旧版执行方不携带 version,清空以表示未知,避免与真实值混淆。
		delete(s.executorVers, executorID)
	}
	return prev, replaced
}

// UnregisterExecutor 若 clientID 是 executorID 的执行方则移除。
func (s *Server) UnregisterExecutor(executorID, clientID string) {
	s.executorMu.Lock()
	defer s.executorMu.Unlock()
	if s.executors[executorID] == clientID {
		delete(s.executors, executorID)
		delete(s.executorVers, executorID)
	}
}

// ExecutorVersions 返回所有 executor 在线执行方的构建版本(executorID → version)。
func (s *Server) ExecutorVersions() map[string]string {
	s.executorMu.RLock()
	defer s.executorMu.RUnlock()
	out := make(map[string]string, len(s.executorVers))
	for k, v := range s.executorVers {
		out[k] = v
	}
	return out
}

// GetExecutor 返回 executorID 对应执行方客户端 ID。
func (s *Server) GetExecutor(executorID string) (string, bool) {
	s.executorMu.RLock()
	defer s.executorMu.RUnlock()
	e, ok := s.executors[executorID]
	return e, ok
}

// ExecutorEndpoint 中转记录的单个已注册执行方端点(身份 + 构建版本)。
type ExecutorEndpoint struct {
	ClientID string
	Version  string
}

// ExecutorEndpoints 返回全部已注册执行方的快照(executorID → 端点)。供 `relay status`
// 对整座部署逐执行方探针;副本返回以保证调用方并发安全。
func (s *Server) ExecutorEndpoints() map[string]ExecutorEndpoint {
	s.executorMu.RLock()
	defer s.executorMu.RUnlock()
	out := make(map[string]ExecutorEndpoint, len(s.executors))
	for eid, cid := range s.executors {
		out[eid] = ExecutorEndpoint{ClientID: cid, Version: s.executorVers[eid]}
	}
	return out
}

// registerPending 登记一个中转探针的「待回包」通道(probeID → chan)。执行方回 MsgPong
// 时按 RequestID 命中并投递,配合超时实现对 execution 段的 RTT 测量。
func (s *Server) registerPending(probeID string) chan protocol.Message {
	ch := make(chan protocol.Message, 1)
	s.pendingMu.Lock()
	s.pending[probeID] = ch
	s.pendingMu.Unlock()
	return ch
}

// resolvePending 查 probeID 对应的待回包通道;存在则返回 true。
func (s *Server) resolvePending(probeID string) (chan protocol.Message, bool) {
	s.pendingMu.RLock()
	defer s.pendingMu.RUnlock()
	ch, ok := s.pending[probeID]
	return ch, ok
}

// deletePending 移除探针的待回包登记(超时/失败/正常返回后严格清理,不残留到关闭)。
func (s *Server) deletePending(probeID string) {
	s.pendingMu.Lock()
	delete(s.pending, probeID)
	s.pendingMu.Unlock()
}

// SetReqOwner 记录被转发 exec 请求的归属请求方。
func (s *Server) SetReqOwner(reqID, clientID string) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	s.reqOwner[reqID] = clientID
}

// GetReqOwner 读取 exec 请求的归属请求方;存在则返回 true。
func (s *Server) GetReqOwner(reqID string) (string, bool) {
	s.reqMu.RLock()
	defer s.reqMu.RUnlock()
	owner, ok := s.reqOwner[reqID]
	return owner, ok
}

// ClearReqOwner 移除 exec 请求归属(收尾后调用)。
func (s *Server) ClearReqOwner(reqID string) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	delete(s.reqOwner, reqID)
}

// SetPushRelay 记录一次被转发给执行方的流式 push(streamID → 路由)。
func (s *Server) SetPushRelay(streamID string, info pushRelayInfo) {
	s.pushRelayMu.Lock()
	defer s.pushRelayMu.Unlock()
	s.pushRelay[streamID] = info
}

// GetPushRelay 读取流式 push 路由;存在则返回 true。
func (s *Server) GetPushRelay(streamID string) (pushRelayInfo, bool) {
	s.pushRelayMu.RLock()
	defer s.pushRelayMu.RUnlock()
	info, ok := s.pushRelay[streamID]
	return info, ok
}

// DeletePushRelay 移除流式 push 路由(结束时调用)。
func (s *Server) DeletePushRelay(streamID string) {
	s.pushRelayMu.Lock()
	defer s.pushRelayMu.Unlock()
	delete(s.pushRelay, streamID)
}

// TunnelEnabled 返回隧道通道是否开启(KTD4)。
func (s *Server) TunnelEnabled() bool {
	s.tunnelMu.RLock()
	defer s.tunnelMu.RUnlock()
	return s.tunnelEnabled
}

// registerTunnel 登记一条在转隧道(streamID → 请求方/执行方)。超过并发上限返回 false。
// 隧道路由天然按 streamID 独立,多 executor 多隧道互不干扰。
func (s *Server) registerTunnel(streamID, requesterID, executorID string) bool {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	if len(s.tunnels) >= s.maxTunnels {
		return false
	}
	s.tunnels[streamID] = &tunnelEntry{requesterID: requesterID, executorID: executorID}
	return true
}

// getTunnel 查询一条隧道的两端。
func (s *Server) getTunnel(streamID string) (tunnelEntry, bool) {
	s.tunnelMu.RLock()
	defer s.tunnelMu.RUnlock()
	e, ok := s.tunnels[streamID]
	if !ok {
		return tunnelEntry{}, false
	}
	return *e, true
}

// deleteTunnel 移除一条隧道(结束/任一端断连)。
func (s *Server) deleteTunnel(streamID string) {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	delete(s.tunnels, streamID)
}

// removeClientTunnels 断连时拆除该客户端所在(作为请求方或作为执行方)的全部隧道,并向对端
// 发 MsgTunnelEnd 关停;确保多隧道清理不泄漏、不乱绕(SOCKS5 方案的「任一端断开关隧道」语义)。
func (s *Server) removeClientTunnels(clientID string) {
	s.tunnelMu.Lock()
	var notify []tunnelEndNotify
	for streamID, e := range s.tunnels {
		if e.requesterID == clientID || e.executorID == clientID {
			other := e.executorID
			if e.executorID == clientID {
				other = e.requesterID
			}
			delete(s.tunnels, streamID)
			notify = append(notify, tunnelEndNotify{streamID: streamID, clientID: other})
		}
	}
	s.tunnelMu.Unlock()

	for _, n := range notify {
		_ = s.SendTo(n.clientID, protocol.Message{
			Type:     protocol.MsgTunnelEnd,
			ID:       uuid.New().String(),
			StreamID: n.streamID,
			Payload:  protocol.TunnelEnd{StreamID: n.streamID, Reason: "peer disconnected"},
		})
	}
}

// tunnelEndNotify 拆除一条隧道时向对端告知关闭的内部消息。
type tunnelEndNotify struct {
	streamID string
	clientID string
}

// sendProbe 向某执行方发送一条带写超时的探针(中转测 transit→executor 段)。
// 写超时兜底,防止 half-open 连接把探针发送卡死在 TCP 层(statusProbeTimeout 同时约束发送与等待)。
func (s *Server) sendProbe(clientID string, msg protocol.Message, timeout time.Duration) error {
	s.clientMu.RLock()
	client, ok := s.clients[clientID]
	s.clientMu.RUnlock()
	if !ok {
		return fmt.Errorf("client not found: %s", clientID)
	}
	return client.sendWithDeadline(msg, timeout)
}

// SendToBinary 向某客户端发送一条 JSON 消息后紧跟一个二进制帧(供流式块透传)。
func (s *Server) SendToBinary(clientID string, msg protocol.Message, raw []byte) error {
	s.clientMu.RLock()
	client, ok := s.clients[clientID]
	s.clientMu.RUnlock()
	if !ok {
		return fmt.Errorf("client not found: %s", clientID)
	}
	return client.SendBinary(msg, raw)
}

// cleanupClientState 断连时清理该 client 注册的执行方、待转发请求归属与流式 push 路由。
func (s *Server) cleanupClientState(clientID string) {
	s.executorMu.Lock()
	for eid, e := range s.executors {
		if e == clientID {
			delete(s.executors, eid)
			delete(s.executorVers, eid)
		}
	}
	s.executorMu.Unlock()

	s.reqMu.Lock()
	for reqID, owner := range s.reqOwner {
		if owner == clientID {
			delete(s.reqOwner, reqID)
		}
	}
	s.reqMu.Unlock()

	s.pushRelayMu.Lock()
	for streamID, info := range s.pushRelay {
		if info.executorID == clientID || info.requesterID == clientID {
			delete(s.pushRelay, streamID)
		}
	}
	s.pushRelayMu.Unlock()
}

func (s *Server) BroadcastToSubscribers(event protocol.FileEvent) {
	s.subMu.RLock()
	clients := make([]string, 0)
	if s.subs[event.WatchID] != nil {
		for clientID := range s.subs[event.WatchID] {
			clients = append(clients, clientID)
		}
	}
	s.subMu.RUnlock()

	msg := protocol.Message{
		Type:    protocol.MsgFileEvent,
		ID:      event.EventID,
		Payload: event,
	}

	for _, clientID := range clients {
		_ = s.SendTo(clientID, msg)
	}
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
