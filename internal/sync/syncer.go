package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	gosync "sync"
	"time"

	"github.com/torsync/node/internal/config"
	"github.com/torsync/node/internal/gossip"
	"github.com/torsync/node/internal/mcclient"
	"github.com/torsync/node/internal/store"
	"github.com/torsync/node/internal/torrent"
	"github.com/torsync/node/internal/watcher"
)

// Syncer is the main coordinator that ties together the watcher, torrent engine,
// gossip server, MC client, and local store.
type Syncer struct {
	cfg     *config.Config
	store   *store.Store
	watcher *watcher.Watcher
	engine  *torrent.Engine
	gossip  *gossip.GossipServer
	mc      *mcclient.Client

	mu      gosync.RWMutex
	paused  bool
	stopped chan struct{}
}

// New creates a Syncer wiring all components together.
func New(
	cfg *config.Config,
	st *store.Store,
	w *watcher.Watcher,
	eng *torrent.Engine,
	gs *gossip.GossipServer,
	mc *mcclient.Client,
) *Syncer {
	return &Syncer{
		cfg:     cfg,
		store:   st,
		watcher: w,
		engine:  eng,
		gossip:  gs,
		mc:      mc,
		stopped: make(chan struct{}),
	}
}

// Start registers handlers and begins the sync process.
// Filesystem events are delivered via HandleFileEvent, which is wired up by
// the caller through the watcher's onChange callback.
func (s *Syncer) Start(ctx context.Context) error {
	// Register gossip message handler.
	s.gossip.OnMessage(func(msg gossip.Message) {
		s.handleGossipMessage(msg)
	})

	// Start MC heartbeat loop.
	go s.runHeartbeatLoop(ctx)

	<-ctx.Done()
	close(s.stopped)
	return nil
}

// HandleFileEvent processes a filesystem event from the watcher.
// This is called by the watcher's onChange callback.
func (s *Syncer) HandleFileEvent(event watcher.Event) {
	s.mu.RLock()
	paused := s.paused
	s.mu.RUnlock()

	if paused {
		return
	}

	if event.IsDir {
		return
	}

	switch event.Type {
	case watcher.EventCreate, watcher.EventModify:
		s.handleFileChanged(event.Path)
	case watcher.EventDelete, watcher.EventRename:
		s.handleFileDeleted(event.Path)
	}
}

// handleFileChanged computes the file hash, creates a torrent if changed, and
// broadcasts an announce to peers.
func (s *Syncer) handleFileChanged(filePath string) {
	hash, err := hashFile(filePath)
	if err != nil {
		log.Printf("sync: hash file %s: %v", filePath, err)
		return
	}

	existing, err := s.store.GetFile(filePath)
	if err != nil {
		log.Printf("sync: get file record %s: %v", filePath, err)
	}

	// Skip if content has not changed.
	if existing != nil && !existing.Deleted && existing.ContentHash == hash {
		return
	}

	infoHash, err := s.engine.CreateAndSeed(filePath)
	if err != nil {
		log.Printf("sync: create torrent for %s: %v", filePath, err)
		return
	}

	// Increment vector clock for this node.
	vc := make(map[string]int)
	if existing != nil {
		for k, v := range existing.VectorClock {
			vc[k] = v
		}
	}
	vc[s.cfg.NodeID]++

	info, err := os.Stat(filePath)
	if err != nil {
		log.Printf("sync: stat %s: %v", filePath, err)
		return
	}

	record := store.FileRecord{
		Path:        filePath,
		ContentHash: hash,
		InfoHash:    infoHash,
		VectorClock: vc,
		ModTime:     info.ModTime(),
		Deleted:     false,
		SeenAt:      time.Now(),
	}

	if err := s.store.PutFile(record); err != nil {
		log.Printf("sync: store file record %s: %v", filePath, err)
		return
	}

	// Add all known peers to the torrent so they can download immediately.
	peers, _ := s.store.GetPeers()
	var peerAddrs []string
	for _, p := range peers {
		if p.PublicAddr != "" {
			peerAddrs = append(peerAddrs, fmt.Sprintf("%s:%d", p.PublicAddr, s.cfg.TorrentPort))
		}
	}
	if len(peerAddrs) > 0 {
		_ = s.engine.AddPeers(infoHash, peerAddrs)
	}

	// Broadcast announce to gossip peers.
	announcePayload, err := json.Marshal(gossip.AnnouncePayload{
		Path:         filePath,
		ContentHash:  hash,
		InfoHash:     infoHash,
		Size:         info.Size(),
		ModTime:      info.ModTime(),
		VectorClock:  vc,
		OriginNodeID: s.cfg.NodeID,
	})
	if err == nil {
		_ = s.gossip.Broadcast(gossip.Message{
			Type:    "announce",
			Payload: json.RawMessage(announcePayload),
		})
	}
}

// handleFileDeleted marks a file as deleted and broadcasts a tombstone.
func (s *Syncer) handleFileDeleted(filePath string) {
	existing, _ := s.store.GetFile(filePath)

	vc := make(map[string]int)
	if existing != nil {
		for k, v := range existing.VectorClock {
			vc[k] = v
		}
	}
	vc[s.cfg.NodeID]++

	if err := s.store.DeleteFile(filePath); err != nil {
		log.Printf("sync: mark deleted %s: %v", filePath, err)
		return
	}

	tombPayload, err := json.Marshal(gossip.TombstonePayload{
		Path:         filePath,
		VectorClock:  vc,
		OriginNodeID: s.cfg.NodeID,
	})
	if err == nil {
		_ = s.gossip.Broadcast(gossip.Message{
			Type:    "tombstone",
			Payload: json.RawMessage(tombPayload),
		})
	}
}

// handleGossipMessage dispatches incoming gossip messages to the appropriate handler.
func (s *Syncer) handleGossipMessage(msg gossip.Message) {
	s.mu.RLock()
	paused := s.paused
	s.mu.RUnlock()

	if paused {
		return
	}

	switch msg.Type {
	case "announce":
		var payload gossip.AnnouncePayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			log.Printf("sync: unmarshal announce: %v", err)
			return
		}
		s.handleAnnounce(payload)

	case "tombstone":
		var payload gossip.TombstonePayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			log.Printf("sync: unmarshal tombstone: %v", err)
			return
		}
		s.handleTombstone(payload)
	}
}

// handleAnnounce processes an incoming file announcement from a peer.
func (s *Syncer) handleAnnounce(payload gossip.AnnouncePayload) {
	// Skip if this announcement originated from us.
	if payload.OriginNodeID == s.cfg.NodeID {
		return
	}

	existing, _ := s.store.GetFile(payload.Path)

	if existing != nil && !existing.Deleted {
		rel := compareVectorClocks(payload.VectorClock, existing.VectorClock)

		switch rel {
		case vcEqual, vcOlder:
			// We already have this version or a newer one.
			return

		case vcConcurrent:
			// Concurrent edit: fork the current file before overwriting.
			forkPath := forkFilePath(payload.Path)
			log.Printf("sync: concurrent edit conflict on %s, forking to %s", payload.Path, forkPath)
			if err := copyFile(payload.Path, forkPath); err != nil {
				log.Printf("sync: fork file: %v", err)
			}
			// Fall through to leech the incoming version.
		}
	}

	// Start leeching the incoming version.
	peers, _ := s.store.GetPeers()
	var peerAddrs []string
	for _, p := range peers {
		if p.PublicAddr != "" {
			peerAddrs = append(peerAddrs, fmt.Sprintf("%s:%d", p.PublicAddr, s.cfg.TorrentPort))
		}
	}

	if err := s.engine.StartLeech(payload.InfoHash, payload.Path, peerAddrs); err != nil {
		log.Printf("sync: start leech for %s: %v", payload.Path, err)
		return
	}

	// On completion, update the store record.
	s.engine.OnComplete(payload.InfoHash, func() {
		// Merge vector clocks: take max of each component.
		mergedVC := make(map[string]int)
		if existing != nil {
			for k, v := range existing.VectorClock {
				mergedVC[k] = v
			}
		}
		for k, v := range payload.VectorClock {
			if v > mergedVC[k] {
				mergedVC[k] = v
			}
		}

		record := store.FileRecord{
			Path:        payload.Path,
			ContentHash: payload.ContentHash,
			InfoHash:    payload.InfoHash,
			VectorClock: mergedVC,
			ModTime:     payload.ModTime,
			Deleted:     false,
			SeenAt:      time.Now(),
		}

		if err := s.store.PutFile(record); err != nil {
			log.Printf("sync: store file after leech %s: %v", payload.Path, err)
		}
	})
}

// handleTombstone processes an incoming file deletion from a peer.
func (s *Syncer) handleTombstone(payload gossip.TombstonePayload) {
	if payload.OriginNodeID == s.cfg.NodeID {
		return
	}

	existing, _ := s.store.GetFile(payload.Path)
	if existing != nil {
		rel := compareVectorClocks(payload.VectorClock, existing.VectorClock)
		if rel == vcOlder || rel == vcEqual {
			return
		}
	}

	// Remove the local file.
	if err := os.Remove(payload.Path); err != nil && !os.IsNotExist(err) {
		log.Printf("sync: remove file %s: %v", payload.Path, err)
	}

	if err := s.store.DeleteFile(payload.Path); err != nil {
		log.Printf("sync: mark tombstone %s: %v", payload.Path, err)
	}
}

// runHeartbeatLoop sends heartbeats to the MC and updates peers when topology changes.
func (s *Syncer) runHeartbeatLoop(ctx context.Context) {
	lastSuccess := func() time.Time {
		v, _ := s.store.GetMeta("last_mc_success")
		if v == "" {
			return time.Time{}
		}
		t, _ := time.Parse(time.RFC3339, v)
		return t
	}

	gracePeriod := 1800

	s.mc.StartHeartbeatLoop(
		ctx,
		30*time.Second,
		func() int {
			// Return current local peers version (count of known peers).
			peers, _ := s.store.GetPeers()
			return len(peers)
		},
		func() string {
			// Return public address from config or empty.
			return ""
		},
		lastSuccess,
		func(t time.Time) {
			_ = s.store.SetMeta("last_mc_success", t.Format(time.RFC3339))
		},
		func() int {
			return gracePeriod
		},
		func(peers []mcclient.PeerInfo) {
			s.updatePeers(peers)
		},
		func() {
			log.Println("sync: node or swarm disabled by MC, pausing")
			s.Pause()
		},
	)
}

// updatePeers refreshes the local peer store and gossip connections.
func (s *Syncer) updatePeers(peers []mcclient.PeerInfo) {
	for _, p := range peers {
		record := store.PeerRecord{
			NodeID:          p.NodeID,
			PublicAddr:      p.PublicAddr,
			CertFingerprint: p.CertFingerprint,
			LastSeen:        time.Now(),
		}
		if err := s.store.PutPeer(record); err != nil {
			log.Printf("sync: store peer %s: %v", p.NodeID, err)
		}

		// Attempt to connect to any new peers via gossip.
		if p.PublicAddr != "" {
			gossipAddr := fmt.Sprintf("%s:%d", p.PublicAddr, s.cfg.GossipPort)
			if err := s.gossip.Connect(gossipAddr, p.CertFingerprint); err != nil {
				log.Printf("sync: connect gossip peer %s at %s: %v", p.NodeID, gossipAddr, err)
			}
		}
	}

	// Add new peers to any active torrents being leeched.
	files, _ := s.store.ListFiles()
	for _, f := range files {
		if f.Deleted || f.InfoHash == "" {
			continue
		}
		var addrs []string
		for _, p := range peers {
			if p.PublicAddr != "" {
				addrs = append(addrs, fmt.Sprintf("%s:%d", p.PublicAddr, s.cfg.TorrentPort))
			}
		}
		if len(addrs) > 0 {
			_ = s.engine.AddPeers(f.InfoHash, addrs)
		}
	}
}

// Pause halts sync operations without stopping the process.
func (s *Syncer) Pause() {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
}

// Resume re-enables sync operations after a Pause.
func (s *Syncer) Resume() {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
}

// vcRelation describes the relationship between two vector clocks.
type vcRelation int

const (
	vcEqual      vcRelation = 0
	vcNewer      vcRelation = 1
	vcOlder      vcRelation = 2
	vcConcurrent vcRelation = 3
)

// compareVectorClocks determines how incomingVC relates to localVC.
func compareVectorClocks(incoming, local map[string]int) vcRelation {
	incomingDominates := false
	localDominates := false

	allKeys := make(map[string]struct{})
	for k := range incoming {
		allKeys[k] = struct{}{}
	}
	for k := range local {
		allKeys[k] = struct{}{}
	}

	for k := range allKeys {
		iv := incoming[k]
		lv := local[k]
		if iv > lv {
			incomingDominates = true
		} else if lv > iv {
			localDominates = true
		}
	}

	switch {
	case !incomingDominates && !localDominates:
		return vcEqual
	case incomingDominates && !localDominates:
		return vcNewer
	case !incomingDominates && localDominates:
		return vcOlder
	default:
		return vcConcurrent
	}
}

// hashFile computes the SHA256 hex hash of the file at the given path.
func hashFile(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("hash file open: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file read: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// forkFilePath returns a conflict-fork path for the given file.
func forkFilePath(original string) string {
	ext := filepath.Ext(original)
	base := original[:len(original)-len(ext)]
	return fmt.Sprintf("%s.conflict.%d%s", base, time.Now().UnixNano(), ext)
}

// copyFile copies the file at src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copy open src: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("copy create dst: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy data: %w", err)
	}
	return nil
}
