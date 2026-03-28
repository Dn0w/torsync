package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dn0w/torsync/internal/config"
	"github.com/dn0w/torsync/internal/gossip"
	"github.com/dn0w/torsync/internal/license"
	sscclient "github.com/dn0w/torsync/internal/mcclient"
	"github.com/dn0w/torsync/internal/store"
	syncer "github.com/dn0w/torsync/internal/sync"
	"github.com/dn0w/torsync/internal/svcmgr"
	"github.com/dn0w/torsync/internal/torrent"
	"github.com/dn0w/torsync/internal/watcher"
	"github.com/dn0w/torsync/internal/webui"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "import-key":
		cmdImportKey(os.Args[2:])
	case "start":
		cmdStart(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "peers":
		cmdPeers(os.Args[2:])
	case "service":
		cmdService(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`torSync - distributed file synchronisation agent

Usage:
  torsync import-key  <keyfile>  [--config <path>]   Import a license key from SSC
  torsync start       [--config <path>]               Start the sync agent
  torsync status      [--config <path>]               Show node status
  torsync peers       [--config <path>]               List known peers
  torsync service     <install|start|stop|uninstall|status>  Manage OS service`)
}

// ─── import-key ───────────────────────────────────────────────────────────────

// cmdImportKey validates a .key file from SSC, extracts the API key and limits,
// and writes them into the node's config.yaml.
func cmdImportKey(args []string) {
	fs := flag.NewFlagSet("import-key", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "Config file path")
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "import-key: key file path is required")
		os.Exit(1)
	}
	keyFile := fs.Arg(0)

	// Read and decode the key file.
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		log.Fatalf("import-key: read key file: %v", err)
	}

	var kf struct{ Token string `json:"token"` }
	if err := json.Unmarshal(raw, &kf); err != nil || kf.Token == "" {
		log.Fatalf("import-key: invalid key file format (expected JSON {\"token\":\"...\"})")
	}

	// Quick decode to validate and display info.
	lc, err := license.NewFromKeyFile(keyFile, "")
	if err != nil {
		log.Fatalf("import-key: %v", err)
	}
	_ = lc

	// Load or create config.
	cfg := config.Defaults()
	if _, statErr := os.Stat(*cfgPath); statErr == nil {
		if loaded, loadErr := config.Load(*cfgPath); loadErr == nil {
			cfg = loaded
		}
	}

	// Copy the .key file into the data dir.
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		log.Fatalf("import-key: create data dir: %v", err)
	}
	destKey := filepath.Join(cfg.DataDir, "license.key")
	if err := os.WriteFile(destKey, raw, 0600); err != nil {
		log.Fatalf("import-key: copy key file: %v", err)
	}

	// Parse claims from the token (decode without signature verification).
	var claims struct {
		Sub     string `json:"sub"`
		APIKey  string `json:"api_key"`
		SwarmID string `json:"swarm_id"`
		MaxNodes int   `json:"max_nodes"`
		Exp     int64  `json:"exp"`
		RegUntil int64 `json:"registration_until"`
	}
	parts := splitJWT(kf.Token)
	if err := json.Unmarshal(parts, &claims); err != nil {
		log.Fatalf("import-key: decode claims: %v", err)
	}

	// Update config.
	cfg.NodeID = claims.Sub
	cfg.APIKey = claims.APIKey
	cfg.LicenseKeyPath = destKey

	if cfg.NodeName == "" {
		cfg.NodeName = "node-" + claims.Sub[:8]
	}
	if cfg.SscURL == "" {
		fmt.Println("Note: ssc_url is not set in config. Edit config.yaml to add it.")
	}

	if err := cfg.Save(*cfgPath); err != nil {
		log.Fatalf("import-key: save config: %v", err)
	}

	exp := "—"
	if claims.Exp > 0 {
		exp = time.Unix(claims.Exp, 0).Format("2006-01-02")
	}
	regUntil := "—"
	if claims.RegUntil > 0 {
		regUntil = time.Unix(claims.RegUntil, 0).Format("2006-01-02")
	}

	fmt.Println("License key imported successfully!")
	fmt.Printf("  Instance ID:        %s\n", claims.Sub)
	fmt.Printf("  Max nodes:          %d\n", claims.MaxNodes)
	fmt.Printf("  Registration until: %s\n", regUntil)
	fmt.Printf("  Expires:            %s\n", exp)
	fmt.Printf("  Key stored at:      %s\n", destKey)
	fmt.Printf("  Config saved to:    %s\n", *cfgPath)
	fmt.Printf("\nRun 'torsync start --config %s' to begin syncing.\n", *cfgPath)
}

// ─── start ────────────────────────────────────────────────────────────────────

func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "Config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("start: load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── License ──────────────────────────────────────────────────────────────
	var lc *license.Client

	if cfg.APIKey != "" && cfg.LicenseKeyPath != "" {
		cachePath := filepath.Join(cfg.DataDir, "license.key")
		lc, err = license.NewFromKeyFile(cfg.LicenseKeyPath, cachePath)
		if err != nil {
			log.Fatalf("start: load license: %v", err)
		}
		lc.SetSSCURL(cfg.SscURL)

		if err := lc.Start(ctx); err != nil {
			log.Fatalf("start: license: %v", err)
		}
		if !lc.IsActive() {
			log.Fatalf("start: license is not active (status: %s)", lc.Status())
		}
		log.Printf("torsync: license status: %s, max_nodes: %d, expires: %s",
			lc.Status(), lc.MaxNodes(), lc.ExpiresAt().Format("2006-01-02"))
	} else {
		// Free tier.
		lc = license.NewFree()
		log.Println("torsync: running in free tier (max 2 nodes, 1 sync dir)")
	}

	// ── Determine effective sync directories ─────────────────────────────────
	syncDirs := cfg.SyncDirs
	if lc.IsFree() && len(syncDirs) > 1 {
		log.Printf("torsync: free tier — only syncing first directory (%s)", syncDirs[0].Path)
		syncDirs = syncDirs[:1]
	}
	if len(syncDirs) == 0 {
		log.Fatalf("start: no sync directories configured")
	}

	// ── Store ─────────────────────────────────────────────────────────────────
	dbPath := filepath.Join(cfg.DataDir, "torsync.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("start: open store: %v", err)
	}
	defer st.Close()

	// ── TLS cert ──────────────────────────────────────────────────────────────
	certPath := filepath.Join(cfg.DataDir, "node.crt")
	keyPath := filepath.Join(cfg.DataDir, "node.key")

	// Generate cert on first start if missing.
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		certPEM, keyPEM, _, certErr := gossip.GenerateCert(cfg.NodeName)
		if certErr != nil {
			log.Fatalf("start: generate cert: %v", certErr)
		}
		_ = os.WriteFile(certPath, certPEM, 0600)
		_ = os.WriteFile(keyPath, keyPEM, 0600)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		log.Fatalf("start: read cert: %v", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatalf("start: read key: %v", err)
	}

	tlsCfg, err := gossip.MakeTLSConfig(certPEM, keyPEM)
	if err != nil {
		log.Fatalf("start: build tls config: %v", err)
	}

	// ── Gossip ────────────────────────────────────────────────────────────────
	gossipAddr := fmt.Sprintf("0.0.0.0:%d", cfg.GossipPort)
	gs := gossip.New(cfg.NodeID, gossipAddr, tlsCfg)
	if err := gs.Start(); err != nil {
		log.Fatalf("start: gossip server: %v", err)
	}
	defer gs.Close()

	// Free tier: check node count via gossip.
	if lc.IsFree() {
		go func() {
			// Give gossip a few seconds to discover peers.
			time.Sleep(10 * time.Second)
			if gs.PeerCount() >= lc.MaxNodes() {
				log.Fatalf("start: free tier allows max %d nodes but swarm already has %d. Import a license key to add more.", lc.MaxNodes(), gs.PeerCount()+1)
			}
		}()
	}

	// Add SSC-discovered peers to gossip.
	if !lc.IsFree() {
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					for _, p := range lc.Peers() {
						if p.PublicAddress != "" {
							gs.AddPeer(p.PublicAddress)
						}
					}
				}
			}
		}()
	}

	// ── Torrent engine ────────────────────────────────────────────────────────
	eng, err := torrent.New(torrent.EngineConfig{
		DataDir:    cfg.DataDir,
		ListenPort: cfg.TorrentPort,
		EnableMSE:  true,
	})
	if err != nil {
		log.Fatalf("start: torrent engine: %v", err)
	}
	defer eng.Close()

	// ── SSC node heartbeat client (peer discovery) ────────────────────────────
	var mc *sscclient.Client
	if cfg.APIKey != "" && cfg.NodeID != "" && cfg.SscURL != "" {
		mc = sscclient.New(cfg.SscURL, cfg.APIKey, cfg.NodeID)
	}

	// ── Watchers + syncers for each directory ─────────────────────────────────
	for _, sd := range syncDirs {
		dir := sd.Path
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("start: create sync dir %s: %v", dir, err)
		}

		syn := syncer.New(cfg, st, nil, eng, gs, mc)
		w, err := watcher.New(dir, func(event watcher.Event) {
			syn.HandleFileEvent(event)
		})
		if err != nil {
			log.Fatalf("start: create watcher for %s: %v", dir, err)
		}
		if err := w.Start(); err != nil {
			log.Fatalf("start: start watcher for %s: %v", dir, err)
		}
		syn = syncer.New(cfg, st, w, eng, gs, mc)

		go func(syn *syncer.Syncer) {
			if err := syn.Start(ctx); err != nil && err != context.Canceled {
				log.Printf("syncer error for %s: %v", dir, err)
			}
		}(syn)

		defer w.Stop()
		log.Printf("torsync: watching %s", dir)
	}

	// ── Web UI ────────────────────────────────────────────────────────────────
	webuiSrv := webui.New(cfg, *cfgPath, lc, st, gs)
	webuiAddr := fmt.Sprintf("127.0.0.1:%d", cfg.WebPort)
	httpSrv := &http.Server{Addr: webuiAddr, Handler: webuiSrv.Handler()}
	go func() {
		log.Printf("torsync: web UI at http://127.0.0.1:%d (user: %s)", cfg.WebPort, cfg.WebUsername)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("web UI error: %v", err)
		}
	}()
	defer httpSrv.Shutdown(context.Background())

	// ── Shutdown ──────────────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("torsync: node %s started — %d sync dir(s)", cfg.NodeID, len(syncDirs))

	select {
	case <-quit:
		log.Println("torsync: shutting down...")
		cancel()
	case <-ctx.Done():
	}

	log.Println("torsync: stopped")
}

// ─── status ───────────────────────────────────────────────────────────────────

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "Config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("status: load config: %v", err)
	}

	dbPath := filepath.Join(cfg.DataDir, "torsync.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("status: open store: %v", err)
	}
	defer st.Close()

	files, _ := st.ListFiles()
	peers, _ := st.GetPeers()

	active, deleted := 0, 0
	for _, f := range files {
		if f.Deleted {
			deleted++
		} else {
			active++
		}
	}

	fmt.Printf("Node ID:       %s\n", cfg.NodeID)
	fmt.Printf("Node Name:     %s\n", cfg.NodeName)
	fmt.Printf("SSC URL:       %s\n", cfg.SscURL)
	fmt.Printf("Sync Dirs:     %d configured\n", len(cfg.SyncDirs))
	for _, d := range cfg.SyncDirs {
		fmt.Printf("               %s\n", d.Path)
	}
	fmt.Printf("Files tracked: %d (%d deleted)\n", active, deleted)
	fmt.Printf("Known peers:   %d\n", len(peers))
	fmt.Printf("Web UI:        http://127.0.0.1:%d\n", cfg.WebPort)
}

// ─── peers ────────────────────────────────────────────────────────────────────

func cmdPeers(args []string) {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "Config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("peers: load config: %v", err)
	}

	dbPath := filepath.Join(cfg.DataDir, "torsync.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("peers: open store: %v", err)
	}
	defer st.Close()

	peers, err := st.GetPeers()
	if err != nil {
		log.Fatalf("peers: %v", err)
	}

	if len(peers) == 0 {
		fmt.Println("No peers known.")
		return
	}

	fmt.Printf("%-36s  %-25s  %s\n", "NODE ID", "ADDRESS", "LAST SEEN")
	fmt.Println(strings.Repeat("-", 90))
	for _, p := range peers {
		fmt.Printf("%-36s  %-25s  %s\n", p.NodeID, p.PublicAddr, p.LastSeen.Format(time.RFC3339))
	}
}

// ─── service ──────────────────────────────────────────────────────────────────

func cmdService(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "service: subcommand required: install, start, stop, uninstall, status")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("service", flag.ExitOnError)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "Config file path")
	_ = fs.Parse(args[1:])

	switch args[0] {
	case "install":
		binary, err := os.Executable()
		if err != nil {
			log.Fatalf("service install: %v", err)
		}
		if err := svcmgr.Install(binary, *cfgPath); err != nil {
			log.Fatalf("service install: %v", err)
		}
	case "start":
		if err := svcmgr.Start(); err != nil {
			log.Fatalf("service start: %v", err)
		}
	case "stop":
		if err := svcmgr.Stop(); err != nil {
			log.Fatalf("service stop: %v", err)
		}
	case "uninstall":
		if err := svcmgr.Uninstall(); err != nil {
			log.Fatalf("service uninstall: %v", err)
		}
	case "status":
		if err := svcmgr.Status(); err != nil {
			log.Fatalf("service status: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "service: unknown subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// splitJWT decodes the payload segment of a JWT and returns the raw JSON bytes.
func splitJWT(token string) []byte {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	return b
}
