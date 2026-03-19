package gossip

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"sync"
	"time"
)

// Message is the outer envelope for all gossip protocol messages.
type Message struct {
	Type    string          `json:"type"` // announce, tombstone, peer_list, heartbeat
	Payload json.RawMessage `json:"payload"`
}

// AnnouncePayload carries metadata about a new or updated file.
type AnnouncePayload struct {
	Path         string         `json:"path"`
	ContentHash  string         `json:"content_hash"`
	InfoHash     string         `json:"info_hash"`
	Size         int64          `json:"size"`
	ModTime      time.Time      `json:"mod_time"`
	VectorClock  map[string]int `json:"vector_clock"`
	OriginNodeID string         `json:"origin_node_id"`
}

// TombstonePayload signals that a file has been deleted.
type TombstonePayload struct {
	Path         string         `json:"path"`
	VectorClock  map[string]int `json:"vector_clock"`
	OriginNodeID string         `json:"origin_node_id"`
}

// peer represents an active mTLS connection to a peer node.
type peer struct {
	nodeID          string
	certFingerprint string
	conn            net.Conn
	enc             *json.Encoder
	mu              sync.Mutex
}

// GossipServer manages mTLS gossip connections to peer nodes.
type GossipServer struct {
	nodeID     string
	listenAddr string
	tlsConfig  *tls.Config

	mu       sync.RWMutex
	peers    map[string]*peer // nodeID → peer
	handlers []func(Message)

	listener net.Listener
}

// New creates a GossipServer.
func New(nodeID, listenAddr string, tlsConfig *tls.Config) *GossipServer {
	return &GossipServer{
		nodeID:     nodeID,
		listenAddr: listenAddr,
		tlsConfig:  tlsConfig,
		peers:      make(map[string]*peer),
	}
}

// Start binds the mTLS listener and begins accepting peer connections.
func (g *GossipServer) Start() error {
	ln, err := tls.Listen("tcp", g.listenAddr, g.tlsConfig)
	if err != nil {
		return fmt.Errorf("gossip: listen %s: %w", g.listenAddr, err)
	}
	g.listener = ln
	log.Printf("gossip: listening on %s", g.listenAddr)
	go g.acceptLoop()
	return nil
}

// Connect establishes an outbound mTLS connection to a peer.
// certFingerprint is used to verify the peer's self-signed certificate.
func (g *GossipServer) Connect(peerAddr, peerCertFingerprint string) error {
	cfg := g.tlsConfig.Clone()
	cfg.InsecureSkipVerify = true // We do manual fingerprint verification below.
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		return verifyFingerprint(cs, peerCertFingerprint)
	}

	conn, err := tls.Dial("tcp", peerAddr, cfg)
	if err != nil {
		return fmt.Errorf("gossip: connect to %s: %w", peerAddr, err)
	}

	p := &peer{
		certFingerprint: peerCertFingerprint,
		conn:            conn,
		enc:             json.NewEncoder(conn),
	}

	// Send our node ID as the first message so the peer can identify us.
	hello := Message{Type: "hello"}
	helloPayload, _ := json.Marshal(map[string]string{"node_id": g.nodeID})
	hello.Payload = helloPayload

	p.mu.Lock()
	err = p.enc.Encode(hello)
	p.mu.Unlock()
	if err != nil {
		conn.Close()
		return fmt.Errorf("gossip: send hello to %s: %w", peerAddr, err)
	}

	// Read peer's node ID from their hello response.
	dec := json.NewDecoder(conn)
	var helloResp Message
	if err := dec.Decode(&helloResp); err != nil {
		conn.Close()
		return fmt.Errorf("gossip: read hello from %s: %w", peerAddr, err)
	}

	var helloData map[string]string
	if err := json.Unmarshal(helloResp.Payload, &helloData); err != nil {
		conn.Close()
		return fmt.Errorf("gossip: parse hello from %s: %w", peerAddr, err)
	}
	p.nodeID = helloData["node_id"]

	g.mu.Lock()
	g.peers[p.nodeID] = p
	g.mu.Unlock()

	go g.readLoop(p, dec)
	return nil
}

// Broadcast sends a message to all currently connected peers.
func (g *GossipServer) Broadcast(msg Message) error {
	g.mu.RLock()
	peers := make([]*peer, 0, len(g.peers))
	for _, p := range g.peers {
		peers = append(peers, p)
	}
	g.mu.RUnlock()

	var lastErr error
	for _, p := range peers {
		p.mu.Lock()
		err := p.enc.Encode(msg)
		p.mu.Unlock()
		if err != nil {
			lastErr = err
			log.Printf("gossip: broadcast to %s failed: %v", p.nodeID, err)
		}
	}
	return lastErr
}

// OnMessage registers a handler that is called for every received message.
func (g *GossipServer) OnMessage(handler func(Message)) {
	g.mu.Lock()
	g.handlers = append(g.handlers, handler)
	g.mu.Unlock()
}

// Close shuts down the gossip listener and all peer connections.
func (g *GossipServer) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, p := range g.peers {
		p.conn.Close()
	}
	g.peers = make(map[string]*peer)

	if g.listener != nil {
		return g.listener.Close()
	}
	return nil
}

// acceptLoop accepts incoming mTLS connections from peers.
func (g *GossipServer) acceptLoop() {
	for {
		conn, err := g.listener.Accept()
		if err != nil {
			log.Printf("gossip: accept error: %v", err)
			return
		}
		go g.handleInbound(conn)
	}
}

// handleInbound processes a newly accepted peer connection.
func (g *GossipServer) handleInbound(conn net.Conn) {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	// Expect hello message first.
	var hello Message
	if err := dec.Decode(&hello); err != nil {
		log.Printf("gossip: read inbound hello: %v", err)
		conn.Close()
		return
	}

	var helloData map[string]string
	if err := json.Unmarshal(hello.Payload, &helloData); err != nil {
		conn.Close()
		return
	}
	peerNodeID := helloData["node_id"]

	// Reply with our own hello.
	replyPayload, _ := json.Marshal(map[string]string{"node_id": g.nodeID})
	reply := Message{Type: "hello", Payload: replyPayload}
	if err := enc.Encode(reply); err != nil {
		conn.Close()
		return
	}

	p := &peer{
		nodeID: peerNodeID,
		conn:   conn,
		enc:    enc,
	}

	g.mu.Lock()
	g.peers[peerNodeID] = p
	g.mu.Unlock()

	g.readLoop(p, dec)
}

// readLoop reads JSON messages from a peer until the connection closes.
func (g *GossipServer) readLoop(p *peer, dec *json.Decoder) {
	defer func() {
		p.conn.Close()
		g.mu.Lock()
		delete(g.peers, p.nodeID)
		g.mu.Unlock()
		log.Printf("gossip: peer %s disconnected", p.nodeID)
	}()

	for {
		var msg Message
		if err := dec.Decode(&msg); err != nil {
			return
		}
		g.dispatch(msg)
	}
}

// dispatch calls all registered message handlers.
func (g *GossipServer) dispatch(msg Message) {
	g.mu.RLock()
	handlers := make([]func(Message), len(g.handlers))
	copy(handlers, g.handlers)
	g.mu.RUnlock()

	for _, h := range handlers {
		h(msg)
	}
}

// GenerateCert creates a self-signed ECDSA certificate for the given nodeID.
// Returns certPEM, keyPEM, and the SHA256 hex fingerprint of the certificate.
func GenerateCert(nodeID string) (certPEM, keyPEM []byte, fingerprint string, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("gossip: generate key: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: nodeID,
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("gossip: create certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("gossip: marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Fingerprint is the SHA256 hash of the DER-encoded certificate.
	sum := sha256.Sum256(certDER)
	fingerprint = hex.EncodeToString(sum[:])

	return certPEM, keyPEM, fingerprint, nil
}

// verifyFingerprint checks that the peer's certificate matches the expected fingerprint.
func verifyFingerprint(cs tls.ConnectionState, expected string) error {
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("gossip: no peer certificates presented")
	}
	sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
	got := hex.EncodeToString(sum[:])
	if got != expected {
		return fmt.Errorf("gossip: cert fingerprint mismatch: got %s want %s", got, expected)
	}
	return nil
}

// MakeTLSConfig builds a tls.Config from PEM-encoded certificate and key.
func MakeTLSConfig(certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("gossip: load key pair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// PeerCount returns the number of currently connected gossip peers.
func (g *GossipServer) PeerCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.peers)
}

// ConnectedPeerIDs returns the node IDs of all connected peers.
func (g *GossipServer) ConnectedPeerIDs() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ids := make([]string, 0, len(g.peers))
	for id := range g.peers {
		ids = append(ids, id)
	}
	return ids
}

