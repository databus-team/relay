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

	subs   map[string]map[string]bool
	subMu  sync.RWMutex
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
		addr:      cfg.Addr,
		watchDirs: make(map[string]string),
		watchCfgs: cfg.WatchDirs,
		clients:   make(map[string]*Client),
		serverID:  "relay-" + uuid.New().String()[:8],
		auth:      cfg.Auth,
		tls:       cfg.TLS,
		subs:      make(map[string]map[string]bool),
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
	if s, ok := v.(string); ok { return s }
	return ""
}
