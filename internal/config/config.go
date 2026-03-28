package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// SyncDir is a single watched directory entry.
type SyncDir struct {
	Path string `yaml:"path"`
}

// Config holds the runtime configuration for a torSync agent.
type Config struct {
	// Node identity — populated after import-key
	NodeName string `yaml:"node_name"`
	NodeID   string `yaml:"node_id"`
	APIKey   string `yaml:"api_key"` // raw SSC API key for license heartbeats

	// SSC connection
	SscURL string `yaml:"ssc_url"`

	// Sync directories.
	// Free tier (no license): only the first entry is honoured.
	// Licensed tier: all entries are watched and synced.
	SyncDirs []SyncDir `yaml:"sync_dirs"`

	// Network ports
	TorrentPort int `yaml:"torrent_port"`
	GossipPort  int `yaml:"gossip_port"`

	// Storage
	DataDir string `yaml:"data_dir"`

	// Performance
	SmallFileBatchMS int `yaml:"small_file_batch_ms"`

	// Web management UI
	WebPort         int    `yaml:"web_port"`
	WebUsername     string `yaml:"web_username"`
	WebPasswordHash string `yaml:"web_password_hash"` // bcrypt hash

	// License key file — path to the .key file downloaded from SSC.
	// Populated by "torsync import-key".
	LicenseKeyPath string `yaml:"license_key_path"`
}

// DefaultDataDir returns the platform-appropriate default data directory.
func DefaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "torSync")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "torSync")
	default: // linux and others
		return "/var/lib/torsync"
	}
}

// DefaultConfigPath returns the platform-appropriate default config file path.
func DefaultConfigPath() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "torSync", "config.yaml")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "torsync", "config.yaml")
	default:
		return "/etc/torsync/config.yaml"
	}
}

// Defaults returns a Config pre-filled with platform-appropriate defaults.
func Defaults() *Config {
	dataDir := DefaultDataDir()
	return &Config{
		TorrentPort:      6881,
		GossipPort:       6883,
		DataDir:          dataDir,
		SmallFileBatchMS: 500,
		WebPort:          8080,
		WebUsername:      "admin",
		LicenseKeyPath:   filepath.Join(dataDir, "license.key"),
	}
}

// Load reads and parses a YAML config file from the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file %s: %w", path, err)
	}

	cfg := Defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}

	if cfg.NodeName == "" {
		return nil, fmt.Errorf("config: node_name is required")
	}
	if len(cfg.SyncDirs) == 0 && cfg.SscURL == "" {
		return nil, fmt.Errorf("config: at least one sync_dir is required")
	}

	return cfg, nil
}

// Save writes the current configuration back to the given path.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("config: create config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal yaml: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("config: write file %s: %w", path, err)
	}
	return nil
}

// SyncDir returns the first configured sync directory path, or empty string.
func (c *Config) FirstSyncDir() string {
	if len(c.SyncDirs) == 0 {
		return ""
	}
	return c.SyncDirs[0].Path
}
