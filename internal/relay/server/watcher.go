package server

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/user/relay/internal/relay/protocol"
)

type FileWatcher struct {
	fsw        *fsnotify.Watcher
	server     *Server
	debounce   map[string]*time.Timer
	debounceMu sync.Mutex
	watchMap   map[string]string
}

func NewFileWatcher(server *Server) (*FileWatcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	wm := make(map[string]string)
	for watchID, dir := range server.watchDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			log.Printf("[watcher] skip watch %s: %v", watchID, err)
			continue
		}
		wm[absDir] = watchID
	}

	return &FileWatcher{
		fsw:      fsw,
		server:   server,
		debounce: make(map[string]*time.Timer),
		watchMap: wm,
	}, nil
}

func (fw *FileWatcher) Start(ctx context.Context) error {
	defer fw.fsw.Close()

	for dir := range fw.watchMap {
		if err := fw.fsw.Add(dir); err != nil {
			log.Printf("[watcher] failed to add watch %s: %v", dir, err)
		} else {
			log.Printf("[watcher] watching %s", dir)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-fw.fsw.Events:
			if !ok {
				return nil
			}
			fw.handleEvent(event)
		case err, ok := <-fw.fsw.Errors:
			if !ok {
				return nil
			}
			log.Printf("[watcher] error: %v", err)
		}
	}
}

func (fw *FileWatcher) handleEvent(event fsnotify.Event) {
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
		return
	}

	watchID := fw.resolveWatchID(event.Name)
	if watchID == "" {
		return
	}

	fw.debounceMu.Lock()
	if timer, exists := fw.debounce[event.Name]; exists {
		timer.Stop()
	}
	fw.debounce[event.Name] = time.AfterFunc(200*time.Millisecond, func() {
		fw.debounceMu.Lock()
		delete(fw.debounce, event.Name)
		fw.debounceMu.Unlock()

		fw.emitEvent(event, watchID)
	})
	fw.debounceMu.Unlock()
}

func (fw *FileWatcher) resolveWatchID(eventPath string) string {
	absPath, err := filepath.Abs(eventPath)
	if err != nil {
		return ""
	}
	dir := filepath.Dir(absPath)

	if wid, ok := fw.watchMap[dir]; ok {
		return wid
	}

	for baseDir, wid := range fw.watchMap {
		if len(absPath) > len(baseDir) && absPath[:len(baseDir)] == baseDir {
			return wid
		}
	}
	return ""
}

func (fw *FileWatcher) emitEvent(event fsnotify.Event, watchID string) {
	name := filepath.Base(event.Name)
	watchDir := fw.findWatchDir(watchID)
	relPath, _ := filepath.Rel(watchDir, event.Name)
	if relPath == "" {
		relPath = name
	}

	var op protocol.FileOp
	switch {
	case event.Op&fsnotify.Create != 0:
		op = protocol.OpCreate
	case event.Op&fsnotify.Write != 0:
		op = protocol.OpModify
	case event.Op&fsnotify.Remove != 0:
		op = protocol.OpDelete
	case event.Op&fsnotify.Rename != 0:
		op = protocol.OpRename
	default:
		op = protocol.OpModify
	}

	var size int64
	var modTime int64
	if info, err := os.Stat(event.Name); err == nil {
		size = info.Size()
		modTime = info.ModTime().UnixMilli()
	}

	fe := protocol.FileEvent{
		EventID: uuid.New().String(),
		WatchID: watchID,
		Path:    relPath,
		Name:    name,
		Dir:     filepath.Dir(relPath),
		Op:      op,
		Size:    size,
		ModTime: modTime,
	}

	fw.server.BroadcastToSubscribers(fe)
}

func (fw *FileWatcher) findWatchDir(watchID string) string {
	for dir, wid := range fw.watchMap {
		if wid == watchID {
			return dir
		}
	}
	return ""
}
