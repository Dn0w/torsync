package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the runtime configuration for a torsync node agent.
type Config struct {
	NodeName        string `yaml:"node_name"`
	SyncDir         string `yaml:"sync_dir"`
	MCURL           string `yaml:"mc_url"`
	APIKey          string `yaml:"api_key"`
	NodeID          string `yaml:"node_id"`
	TorrentPort     int    `yaml:"torrent_port"`
	GossipPort      int    `yaml:"gossip_port"`
	DataDir         string `yaml:"data_dir"`
	SmallFileBatchMS int   `yaml:"small_file_batch_ms"`
}

// Load reads and parses a YAML config file from the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read file %s: %w", path, err)
	}

	cfg := &Config{
		TorrentPort:      6881,
		GossipPort:       6883,
		DataDir:          "/var/lib/torsync",
		SmallFileBatchMS: 500,
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse yaml: %w", err)
	}

	if cfg.NodeName == "" {
		return nil, fmt.Errorf("config: node_name is required")
	}
	if cfg.SyncDir == "" {
		return nil, fmt.Errorf("config: sync_dir is required")
	}
	if cfg.MCURL == "" {
		return nil, fmt.Errorf("config: mc_url is required")
	}

	return cfg, nil
}

// Save writes the current configuration back to the given path.
// Used after registration to persist the node_id and api_key.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal yaml: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("config: write file %s: %w", path, err)
	}

	return nil
}
