package store

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketFiles = []byte("files")
	bucketPeers = []byte("peers")
	bucketMeta  = []byte("meta")
)

// FileRecord tracks a synced file's state including its vector clock.
type FileRecord struct {
	Path        string         `json:"path"`
	ContentHash string         `json:"content_hash"`
	InfoHash    string         `json:"info_hash"`
	VectorClock map[string]int `json:"vector_clock"`
	ModTime     time.Time      `json:"mod_time"`
	Deleted     bool           `json:"deleted"`
	SeenAt      time.Time      `json:"seen_at"`
}

// PeerRecord holds contact information for a known peer node.
type PeerRecord struct {
	NodeID          string    `json:"node_id"`
	PublicAddr      string    `json:"public_addr"`
	CertFingerprint string    `json:"cert_fingerprint"`
	LastSeen        time.Time `json:"last_seen"`
}

// Store wraps a BoltDB database providing typed access to all buckets.
type Store struct {
	db *bolt.DB
}

// Open opens (or creates) a BoltDB database at the given path.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// Ensure all buckets exist.
	err = db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{bucketFiles, bucketPeers, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("create bucket %s: %w", bucket, err)
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init buckets: %w", err)
	}

	return &Store{db: db}, nil
}

// Close shuts down the BoltDB database.
func (s *Store) Close() error {
	return s.db.Close()
}

// PutFile inserts or updates a FileRecord keyed by its path.
func (s *Store) PutFile(record FileRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal file record: %w", err)
		}
		return b.Put([]byte(record.Path), data)
	})
}

// GetFile retrieves a FileRecord by its path. Returns nil, nil if not found.
func (s *Store) GetFile(path string) (*FileRecord, error) {
	var record *FileRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		data := b.Get([]byte(path))
		if data == nil {
			return nil
		}
		record = &FileRecord{}
		return json.Unmarshal(data, record)
	})
	if err != nil {
		return nil, fmt.Errorf("store: get file %s: %w", path, err)
	}
	return record, nil
}

// ListFiles returns all file records including tombstones.
func (s *Store) ListFiles() ([]FileRecord, error) {
	var records []FileRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		return b.ForEach(func(k, v []byte) error {
			var r FileRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return fmt.Errorf("unmarshal record for %s: %w", k, err)
			}
			records = append(records, r)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list files: %w", err)
	}
	return records, nil
}

// DeleteFile marks a file as deleted (tombstone) rather than removing the record.
// The record is kept to allow tombstone propagation to peers.
func (s *Store) DeleteFile(path string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFiles)
		data := b.Get([]byte(path))

		var record FileRecord
		if data != nil {
			if err := json.Unmarshal(data, &record); err != nil {
				return fmt.Errorf("unmarshal existing record: %w", err)
			}
		}
		record.Path = path
		record.Deleted = true
		record.SeenAt = time.Now()

		updated, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal tombstone: %w", err)
		}
		return b.Put([]byte(path), updated)
	})
}

// PutPeer inserts or updates a PeerRecord keyed by node ID.
func (s *Store) PutPeer(record PeerRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPeers)
		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal peer record: %w", err)
		}
		return b.Put([]byte(record.NodeID), data)
	})
}

// GetPeers returns all known peer records.
func (s *Store) GetPeers() ([]PeerRecord, error) {
	var peers []PeerRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPeers)
		return b.ForEach(func(k, v []byte) error {
			var p PeerRecord
			if err := json.Unmarshal(v, &p); err != nil {
				return fmt.Errorf("unmarshal peer %s: %w", k, err)
			}
			peers = append(peers, p)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: get peers: %w", err)
	}
	return peers, nil
}

// GetMeta retrieves a metadata value by key. Returns "", nil if not found.
func (s *Store) GetMeta(key string) (string, error) {
	var value string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		data := b.Get([]byte(key))
		if data != nil {
			value = string(data)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("store: get meta %s: %w", key, err)
	}
	return value, nil
}

// SetMeta stores a metadata key-value pair.
func (s *Store) SetMeta(key, value string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.Put([]byte(key), []byte(value))
	})
}
