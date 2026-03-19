package torrent

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// EngineConfig holds configuration for the BitTorrent engine.
type EngineConfig struct {
	DataDir    string
	ListenPort int
	EnableMSE  bool
}

// TorrentStats holds transfer statistics for a torrent.
type TorrentStats struct {
	InfoHash      string
	UploadBytes   int64
	DownloadBytes int64
	ProgressPct   float64
}

// Engine wraps an anacrolix/torrent client and manages seeding/leeching.
type Engine struct {
	client    *torrent.Client
	dataDir   string
	mu        sync.Mutex
	callbacks map[string][]func() // infoHash → completion callbacks
}

// New creates and starts a BitTorrent engine.
// MSE (Message Stream Encryption) and uTP are always enabled.
func New(cfg EngineConfig) (*Engine, error) {
	tcfg := torrent.NewDefaultClientConfig()
	tcfg.DataDir = cfg.DataDir
	tcfg.ListenPort = cfg.ListenPort

	// Always enable MSE for encrypted peer connections.
	tcfg.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{
		Preferred:        true,
		RequirePreferred: false,
	}

	// Enable uTP for NAT traversal.
	tcfg.DisableUTP = false

	// Use file-based storage so data survives restarts.
	tcfg.DefaultStorage = storage.NewFileByInfoHash(cfg.DataDir)

	client, err := torrent.NewClient(tcfg)
	if err != nil {
		return nil, fmt.Errorf("torrent: new client: %w", err)
	}

	return &Engine{
		client:    client,
		dataDir:   cfg.DataDir,
		callbacks: make(map[string][]func()),
	}, nil
}

// CreateAndSeed creates a single-file torrent for filePath and starts seeding.
// Returns the infohash hex string.
func (e *Engine) CreateAndSeed(filePath string) (string, error) {
	info := metainfo.Info{
		PieceLength: 256 * 1024, // 256 KiB pieces
	}

	if buildErr := info.BuildFromFilePath(filePath); buildErr != nil {
		return "", fmt.Errorf("torrent: build info from %s: %w", filePath, buildErr)
	}

	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		return "", fmt.Errorf("torrent: bencode info: %w", err)
	}

	mi := metainfo.MetaInfo{
		InfoBytes: infoBytes,
	}

	t, err := e.client.AddTorrent(&mi)
	if err != nil {
		return "", fmt.Errorf("torrent: add torrent: %w", err)
	}

	<-t.GotInfo()
	t.DownloadAll()

	infoHash := t.InfoHash().HexString()
	return infoHash, nil
}

// AddPeers adds peer addresses to an existing torrent.
func (e *Engine) AddPeers(infoHash string, peers []string) error {
	t, err := e.getTorrent(infoHash)
	if err != nil {
		return err
	}

	var pp []torrent.PeerInfo
	for _, addr := range peers {
		pp = append(pp, torrent.PeerInfo{Addr: torrent.StringAddr(addr)})
	}
	t.AddPeers(pp)
	return nil
}

// StartLeech starts downloading a torrent identified by infoHash.
// Known peer addresses are added immediately to bootstrap the download.
func (e *Engine) StartLeech(infoHash string, outputPath string, peers []string) error {
	var ih metainfo.Hash
	if err := ih.FromHexString(infoHash); err != nil {
		return fmt.Errorf("torrent: parse infohash %s: %w", infoHash, err)
	}

	spec := &torrent.TorrentSpec{
		InfoHash:    ih,
		DisplayName: filepath.Base(outputPath),
		Storage:     storage.NewFileByInfoHash(e.dataDir),
	}

	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return fmt.Errorf("torrent: add torrent spec: %w", err)
	}

	// Add known peers before waiting for info so download starts immediately.
	var pp []torrent.PeerInfo
	for _, addr := range peers {
		pp = append(pp, torrent.PeerInfo{Addr: torrent.StringAddr(addr)})
	}
	t.AddPeers(pp)

	<-t.GotInfo()
	t.DownloadAll()

	// Monitor completion and fire callbacks.
	go e.watchCompletion(t)

	return nil
}

// WaitForCompletion blocks until the torrent identified by infoHash is complete
// or the context is cancelled.
func (e *Engine) WaitForCompletion(ctx context.Context, infoHash string) error {
	t, err := e.getTorrent(infoHash)
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
			if t.BytesMissing() == 0 {
				return nil
			}
		}
	}
}

// OnComplete registers a callback that fires when the given torrent completes.
func (e *Engine) OnComplete(infoHash string, callback func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.callbacks[infoHash] = append(e.callbacks[infoHash], callback)
}

// RemoveTorrent stops seeding or leeching for the given infohash.
func (e *Engine) RemoveTorrent(infoHash string) error {
	t, err := e.getTorrent(infoHash)
	if err != nil {
		return err
	}
	t.Drop()
	return nil
}

// Stats returns transfer statistics for a torrent.
func (e *Engine) Stats(infoHash string) (*TorrentStats, error) {
	t, err := e.getTorrent(infoHash)
	if err != nil {
		return nil, err
	}

	ts := t.Stats()
	total := t.Length()
	missing := t.BytesMissing()

	var progress float64
	if total > 0 {
		progress = float64(total-missing) / float64(total) * 100
	}

	return &TorrentStats{
		InfoHash:      infoHash,
		UploadBytes:   ts.BytesWrittenData.Int64(),
		DownloadBytes: ts.BytesReadData.Int64(),
		ProgressPct:   progress,
	}, nil
}

// Close shuts down the torrent client.
func (e *Engine) Close() {
	e.client.Close()
}

// getTorrent retrieves an active torrent by its infohash hex string.
func (e *Engine) getTorrent(infoHash string) (*torrent.Torrent, error) {
	var ih metainfo.Hash
	if err := ih.FromHexString(infoHash); err != nil {
		return nil, fmt.Errorf("torrent: parse infohash: %w", err)
	}
	t, ok := e.client.Torrent(ih)
	if !ok {
		return nil, fmt.Errorf("torrent: not found: %s", infoHash)
	}
	return t, nil
}

// watchCompletion polls a torrent and fires registered callbacks on completion.
func (e *Engine) watchCompletion(t *torrent.Torrent) {
	for {
		time.Sleep(500 * time.Millisecond)
		if t.BytesMissing() == 0 {
			ih := t.InfoHash().HexString()
			e.mu.Lock()
			cbs := e.callbacks[ih]
			delete(e.callbacks, ih)
			e.mu.Unlock()
			for _, cb := range cbs {
				cb()
			}
			return
		}
	}
}
