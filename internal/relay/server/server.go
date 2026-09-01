package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/user/relay/internal/relay/protocol"
)

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

	// executors 记录每个 watch 上注册的执行方客户端(远端 watcher)。
	executors map[string]string
	// executorVers 记录每个 watch 执行方的 relay 构建版本,供 relay version 对比。
	executorVers map[string]string
	executorMu   sync.RWMutex
	// reqOwner 记录被转发的 exec 请求(reqID)归属的请求方客户端,用于把执行方回流帧转回去。
	reqOwner map[string]string
	reqMu    sync.RWMutex

	// pushRelay 记录被转发给执行方的流式 push(streamID → 执行方/请求方),用于把内容帧原样透传。
	pushRelay   map[string]pushRelayInfo
	pushRelayMu sync.RWMutex

	// upgradeSwap 由接线方(如 cmd/relay)注入的「自升级换装」闭包:入参为已通过 sha256
	// 校验与自检、落盘好的新二进制路径;闭包内做停旧/.prev 备份/替换/重启。仅为 nil 时
	// (如测试)升级通道只回执 ACK 并清理暂存,不真实换装。
	upgradeSwap func(newBin string)
}

// pushRelayInfo 一次被中转发出的流式 push 的路由信息。
type pushRelayInfo struct {
	executorID  string // 接收内容流与执行 jobs 的远端执行方
	reqID       string // 关联回流的请求 ID(与 reqOwner 一致)
	requesterID string // 发起 push 的请求方
}

type Config struct {
	Addr      string
	TLS       TLSConfig
	Auth      AuthConfig
	WatchDirs []WatchDirConfig
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
	s := &Server{
		addr:         cfg.Addr,
		watchDirs:    make(map[string]string),
		watchCfgs:    cfg.WatchDirs,
		clients:      make(map[string]*Client),
		serverID:     "relay-" + uuid.New().String()[:8],
		auth:         cfg.Auth,
		tls:          cfg.TLS,
		subs:         make(map[string]map[string]bool),
		executors:    make(map[string]string),
		executorVers: make(map[string]string),
		reqOwner:     make(map[string]string),
		pushRelay:    make(map[string]pushRelayInfo),
	}

	for _, wd := range cfg.WatchDirs {
		s.watchDirs[wd.ID] = wd.Dir
	}

	s.upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	return s, nil
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

	s.UnsubscribeAll(clientID)
	s.cleanupClientState(clientID)

	s.clientMu.Lock()
	delete(s.clients, clientID)
	s.clientMu.Unlock()
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

// RegisterExecutor 将 clientID 注册为 watchID 的执行方,并记录其 relay 构建版本。
func (s *Server) RegisterExecutor(watchID, clientID, version string) {
	s.executorMu.Lock()
	defer s.executorMu.Unlock()
	s.executors[watchID] = clientID
	if version != "" {
		s.executorVers[watchID] = version
	} else {
		// 旧版执行方不携带 version,清空以表示未知,避免与真实值混淆。
		delete(s.executorVers, watchID)
	}
}

// UnregisterExecutor 若 clientID 是 watchID 的执行方则移除。
func (s *Server) UnregisterExecutor(watchID, clientID string) {
	s.executorMu.Lock()
	defer s.executorMu.Unlock()
	if s.executors[watchID] == clientID {
		delete(s.executors, watchID)
		delete(s.executorVers, watchID)
	}
}

// ExecutorVersions 返回各 watch 在线执行方的构建版本(watchID → version)。
func (s *Server) ExecutorVersions() map[string]string {
	s.executorMu.RLock()
	defer s.executorMu.RUnlock()
	out := make(map[string]string, len(s.executorVers))
	for k, v := range s.executorVers {
		out[k] = v
	}
	return out
}

// GetExecutor 返回 watchID 对应执行方客户端 ID。
func (s *Server) GetExecutor(watchID string) (string, bool) {
	s.executorMu.RLock()
	defer s.executorMu.RUnlock()
	e, ok := s.executors[watchID]
	return e, ok
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
	for watchID, e := range s.executors {
		if e == clientID {
			delete(s.executors, watchID)
			delete(s.executorVers, watchID)
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

func (s *Server) BroadcastFileEvent(event protocol.FileEvent) {
	s.BroadcastToSubscribers(event)
}

func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
