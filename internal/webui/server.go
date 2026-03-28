// Package webui provides a local HTTP management interface for torSync nodes.
// It is protected by HTTP Basic Auth (single admin account, bcrypt-hashed password).
// All templates are inlined as Go string constants — no external files needed.
package webui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dn0w/torsync/internal/config"
	"github.com/dn0w/torsync/internal/license"
	"github.com/dn0w/torsync/internal/store"
)

// StatusProvider is implemented by the gossip + syncer combination to provide
// live node status for the dashboard.
type StatusProvider interface {
	PeerCount() int
}

// Server is the web management HTTP server.
type Server struct {
	cfg        *config.Config
	cfgPath    string
	lc         *license.Client
	store      *store.Store
	status     StatusProvider
	startTime  time.Time
}

// New creates a Server. statusProvider may be nil (shows N/A until set).
func New(cfg *config.Config, cfgPath string, lc *license.Client, st *store.Store, sp StatusProvider) *Server {
	return &Server{
		cfg:       cfg,
		cfgPath:   cfgPath,
		lc:        lc,
		store:     st,
		status:    sp,
		startTime: time.Now(),
	}
}

// Handler returns the HTTP handler with auth middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleDashboard)
	mux.HandleFunc("GET /files", s.handleFiles)
	mux.HandleFunc("GET /peers", s.handlePeers)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings", s.handleSaveSettings)
	mux.HandleFunc("POST /settings/sync-dir", s.handleAddSyncDir)
	mux.HandleFunc("POST /settings/sync-dir/remove", s.handleRemoveSyncDir)
	mux.HandleFunc("GET /api/status", s.handleAPIStatus)
	return s.authMiddleware(mux)
}

// ─── Auth ─────────────────────────────────────────────────────────────────────

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			// Allow unauthenticated health checks from localhost.
			if strings.HasPrefix(r.RemoteAddr, "127.") || strings.HasPrefix(r.RemoteAddr, "[::1]") {
				next.ServeHTTP(w, r)
				return
			}
		}

		user, pass, ok := r.BasicAuth()
		if !ok || user != s.cfg.WebUsername || !s.checkPassword(pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="torSync"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) checkPassword(plain string) bool {
	if s.cfg.WebPasswordHash == "" {
		// No hash set — accept any password until one is configured.
		return true
	}
	err := bcrypt.CompareHashAndPassword([]byte(s.cfg.WebPasswordHash), []byte(plain))
	return err == nil
}

// HashPassword returns a bcrypt hash suitable for storing in config.web_password_hash.
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := map[string]any{
		"Title":       "Dashboard",
		"NodeName":    s.cfg.NodeName,
		"NodeID":      s.cfg.NodeID,
		"Uptime":      time.Since(s.startTime).Round(time.Second).String(),
		"LicStatus":   s.lc.Status(),
		"IsFree":      s.lc.IsFree(),
		"MaxNodes":    s.lc.MaxNodes(),
		"ExpiresAt":   formatTime(s.lc.ExpiresAt()),
		"PeerCount":   s.peerCount(),
		"SyncDirs":    s.cfg.SyncDirs,
	}
	s.render(w, pageDashboard, data)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	files, _ := s.store.ListFiles()
	data := map[string]any{
		"Title": "Files",
		"Files": files,
	}
	s.render(w, pageFiles, data)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peers, _ := s.store.GetPeers()
	sscPeers := s.lc.Peers()
	data := map[string]any{
		"Title":    "Peers",
		"Peers":    peers,
		"SSCPeers": sscPeers,
	}
	s.render(w, pagePeers, data)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, pageSettings, map[string]any{
		"Title":   "Settings",
		"Config":  s.cfg,
		"IsFree":  s.lc.IsFree(),
	})
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	if sscURL := r.FormValue("ssc_url"); sscURL != "" {
		s.cfg.SscURL = sscURL
	}
	if webPort := r.FormValue("web_port"); webPort != "" {
		fmt.Sscan(webPort, &s.cfg.WebPort)
	}

	// Password change
	newPass := r.FormValue("new_password")
	confirmPass := r.FormValue("confirm_password")
	if newPass != "" {
		if newPass != confirmPass {
			s.render(w, pageSettings, map[string]any{
				"Title":  "Settings",
				"Config": s.cfg,
				"IsFree": s.lc.IsFree(),
				"Error":  "Passwords do not match",
			})
			return
		}
		hash, err := HashPassword(newPass)
		if err != nil {
			s.render(w, pageSettings, map[string]any{
				"Title":  "Settings",
				"Config": s.cfg,
				"IsFree": s.lc.IsFree(),
				"Error":  "Failed to hash password: " + err.Error(),
			})
			return
		}
		s.cfg.WebPasswordHash = hash
	}

	if err := s.cfg.Save(s.cfgPath); err != nil {
		s.render(w, pageSettings, map[string]any{
			"Title":  "Settings",
			"Config": s.cfg,
			"IsFree": s.lc.IsFree(),
			"Error":  "Failed to save: " + err.Error(),
		})
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) handleAddSyncDir(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	path := strings.TrimSpace(r.FormValue("path"))
	if path == "" {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	// Free tier: only 1 sync dir allowed.
	if s.lc.IsFree() && len(s.cfg.SyncDirs) >= 1 {
		s.render(w, pageSettings, map[string]any{
			"Title":  "Settings",
			"Config": s.cfg,
			"IsFree": s.lc.IsFree(),
			"Error":  "Free tier supports only one sync directory. Import a license key to add more.",
		})
		return
	}

	// Check for duplicates.
	for _, d := range s.cfg.SyncDirs {
		if filepath.Clean(d.Path) == filepath.Clean(path) {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
	}

	if err := os.MkdirAll(path, 0755); err != nil {
		s.render(w, pageSettings, map[string]any{
			"Title":  "Settings",
			"Config": s.cfg,
			"IsFree": s.lc.IsFree(),
			"Error":  "Cannot create directory: " + err.Error(),
		})
		return
	}

	s.cfg.SyncDirs = append(s.cfg.SyncDirs, config.SyncDir{Path: path})
	_ = s.cfg.Save(s.cfgPath)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handleRemoveSyncDir(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	path := r.FormValue("path")
	cleaned := filepath.Clean(path)

	var kept []config.SyncDir
	for _, d := range s.cfg.SyncDirs {
		if filepath.Clean(d.Path) != cleaned {
			kept = append(kept, d)
		}
	}
	s.cfg.SyncDirs = kept
	_ = s.cfg.Save(s.cfgPath)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"node_id":    s.cfg.NodeID,
		"node_name":  s.cfg.NodeName,
		"license":    s.lc.Status(),
		"is_free":    s.lc.IsFree(),
		"max_nodes":  s.lc.MaxNodes(),
		"peer_count": s.peerCount(),
		"uptime_s":   int(time.Since(s.startTime).Seconds()),
	})
}

// ─── template helpers ─────────────────────────────────────────────────────────

func (s *Server) render(w http.ResponseWriter, body string, data map[string]any) {
	full := pageShell(body)
	t, err := template.New("").Funcs(template.FuncMap{
		"timeAgo": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			d := time.Since(t)
			if d < time.Minute {
				return "just now"
			}
			if d < time.Hour {
				return fmt.Sprintf("%dm ago", int(d.Minutes()))
			}
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		},
	}).Parse(full)
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		// Headers already sent — nothing useful we can do.
		return
	}
}

func (s *Server) peerCount() int {
	if s.status != nil {
		return s.status.PeerCount()
	}
	return 0
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
