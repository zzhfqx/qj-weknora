package browserskill

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// BrowserConnection represents one active WebSocket connection from a
// paired browser extension. The Manager maintains a registry of these so
// agent tool calls can be routed to the right device.
type BrowserConnection interface {
	// DeviceID returns the ID of the connected device.
	DeviceID() string
	// TenantID returns the tenant this device belongs to.
	TenantID() uint64
	// UserID returns the user this device is paired to.
	UserID() string
	// Send sends a message to the browser extension.
	Send(ctx context.Context, msg *BrowserMessage) error
	// Receive reads the next message from the browser extension.
	Receive() (*BrowserMessage, error)
	// Close closes the underlying connection.
	Close() error
	// Done returns a channel that closes when the connection is closed.
	Done() <-chan struct{}
}

// connEntry wraps a BrowserConnection with reference counting and metadata.
type connEntry struct {
	conn      BrowserConnection
	taskCount int
	lastSeen  time.Time
}

// Daemon manages the registry of active browser connections.
// It is safe for concurrent use.
type Daemon struct {
	mu    sync.RWMutex
	conns map[string]*connEntry // keyed by device ID
}

// NewDaemon creates a new connection daemon.
func NewDaemon() *Daemon {
	return &Daemon{
		conns: make(map[string]*connEntry),
	}
}

// Register adds a new browser connection to the registry.
// If a connection with the same device ID already exists, it is replaced
// (and the old one closed).
func (d *Daemon) Register(conn BrowserConnection) error {
	if conn == nil {
		return errors.New("browserskill: nil connection")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if existing, ok := d.conns[conn.DeviceID()]; ok {
		// Close the stale connection
		_ = existing.conn.Close()
	}
	d.conns[conn.DeviceID()] = &connEntry{
		conn:     conn,
		lastSeen: time.Now(),
	}

	// Start a read loop in the background to detect disconnects
	go d.readLoop(conn)
	return nil
}

// Unregister removes a connection from the registry and closes it.
func (d *Daemon) Unregister(deviceID string) {
	d.mu.Lock()
	entry, ok := d.conns[deviceID]
	if ok {
		delete(d.conns, deviceID)
	}
	d.mu.Unlock()
	if ok && entry != nil {
		_ = entry.conn.Close()
	}
}

// GetConnection looks up a connection by device ID.
func (d *Daemon) GetConnection(deviceID string) (BrowserConnection, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	entry, ok := d.conns[deviceID]
	if !ok {
		return nil, false
	}
	return entry.conn, true
}

// FindUserConnection finds any active connection for a given user.
// If the user has multiple devices, it returns the most recently active one.
func (d *Daemon) FindUserConnection(tenantID uint64, userID string) (BrowserConnection, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var best BrowserConnection
	var bestSeen time.Time
	for _, entry := range d.conns {
		c := entry.conn
		if c.TenantID() == tenantID && c.UserID() == userID {
			if best == nil || entry.lastSeen.After(bestSeen) {
				best = c
				bestSeen = entry.lastSeen
			}
		}
	}
	return best, best != nil
}

// ConnectionCount returns the number of active connections.
func (d *Daemon) ConnectionCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.conns)
}

// UserHasActiveBrowser checks if a user has at least one active browser connection.
func (d *Daemon) UserHasActiveBrowser(tenantID uint64, userID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, entry := range d.conns {
		c := entry.conn
		if c.TenantID() == tenantID && c.UserID() == userID {
			return true
		}
	}
	return false
}

// ListDevices returns device IDs of all currently connected devices for a user.
func (d *Daemon) ListDevices(tenantID uint64, userID string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var ids []string
	for _, entry := range d.conns {
		c := entry.conn
		if c.TenantID() == tenantID && c.UserID() == userID {
			ids = append(ids, c.DeviceID())
		}
	}
	return ids
}

// --- internal helpers ---

func (d *Daemon) readLoop(conn BrowserConnection) {
	defer d.Unregister(conn.DeviceID())
	for {
		_, err := conn.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return
			}
			// Check if the connection is closed
			select {
			case <-conn.Done():
				return
			default:
			}
			// For other errors, keep going (could be transient)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Touch last seen
		d.mu.Lock()
		if entry, ok := d.conns[conn.DeviceID()]; ok {
			entry.lastSeen = time.Now()
		}
		d.mu.Unlock()
	}
}

// --- Wire-format helpers ---

// MarshalMessage serializes a BrowserMessage to JSON.
func MarshalMessage(msg *BrowserMessage) ([]byte, error) {
	return json.Marshal(msg)
}

// UnmarshalMessage parses a JSON message into a BrowserMessage.
func UnmarshalMessage(data []byte) (*BrowserMessage, error) {
	var msg BrowserMessage
	err := json.Unmarshal(data, &msg)
	if err != nil {
		return nil, err
	}
	return &msg, nil
}
