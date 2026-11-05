// Package browserskill implements the local browser automation skill.
//
// BrowserSkill lets an agent drive a real browser (Chrome, Edge, etc.) on
// the user's local machine via a paired browser extension. The agent issues
// commands (navigate, click, type, etc.) and receives screenshots + DOM
// state back, all tunneled over a WebSocket connection authenticated by a
// per-device secret.
//
// This package contains:
//   - Manager: top-level coordinator (pairing, device registry, task dispatch)
//   - Store: database-backed device/paing storage
//   - Errors: public error definitions
//   - Result: result parsing and formatting helpers
package browserskill

import (
	"context"
	"sync"
)

// Manager is the top-level BrowserSkill coordinator. It tracks paired
// browser devices, routes agent tool calls to the right device, and
// handles task lifecycle (start, interrupt, resume).
//
// A nil *Manager is valid: all methods degrade to "not available" so
// callers don't have to check before calling.
type Manager struct {
	mu      sync.RWMutex
	enabled bool
	store   Store
}

// Config configures a Manager.
type Config struct {
	// Enabled controls whether BrowserSkill is active at all. When false,
	// the manager is inert and Enabled() returns false.
	Enabled bool
}

// Store is the persistence surface Manager needs. In production it is
// backed by the database; tests use an in-memory implementation.
type Store interface {
	// ListDevices returns all non-deleted devices for a tenant/user.
	ListDevices(ctx context.Context, tenantID uint64, userID string) ([]*Device, error)
	// GetDevice looks up a device by id.
	GetDevice(ctx context.Context, deviceID string) (*Device, error)
	// RegisterDevice registers a new paired device.
	RegisterDevice(ctx context.Context, device *Device) error
	// DeleteDevice soft-deletes a device.
	DeleteDevice(ctx context.Context, tenantID uint64, userID string, deviceID string) error
	// CreatePairing creates a short-lived pairing token.
	CreatePairing(ctx context.Context, tenantID uint64, userID string, ttlSeconds int) (string, error)
	// ConsumePairing validates and consumes a pairing token.
	ConsumePairing(ctx context.Context, token string) (tenantID uint64, userID string, err error)
}

// Device represents one paired browser extension instance.
type Device struct {
	ID             string `json:"id"`
	TenantID       uint64 `json:"tenant_id"`
	UserID         string `json:"user_id"`
	DeviceName     string `json:"device_name"`
	BrowserName    string `json:"browser_name"`
	BrowserVersion string `json:"browser_version"`
	OSName         string `json:"os_name"`
	LastSeenAt     int64  `json:"last_seen_at,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

// NewManager creates a new BrowserSkill manager. When config.Enabled is
// false or store is nil, the manager is inert.
func NewManager(cfg Config, store Store) *Manager {
	if !cfg.Enabled || store == nil {
		return &Manager{enabled: false}
	}
	return &Manager{
		enabled: true,
		store:   store,
	}
}

// Enabled reports whether BrowserSkill is available in this deployment.
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled
}

// ListDevices returns all paired devices for the caller.
func (m *Manager) ListDevices(ctx context.Context, tenantID uint64, userID string) ([]*Device, error) {
	if !m.Enabled() || m.store == nil {
		return nil, ErrBrowserUnavailable
	}
	return m.store.ListDevices(ctx, tenantID, userID)
}

// DeleteDevice removes a paired device.
func (m *Manager) DeleteDevice(ctx context.Context, tenantID uint64, userID string, deviceID string) error {
	if !m.Enabled() || m.store == nil {
		return ErrBrowserUnavailable
	}
	return m.store.DeleteDevice(ctx, tenantID, userID, deviceID)
}

// CreatePairingToken generates a new pairing token for the caller.
func (m *Manager) CreatePairingToken(ctx context.Context, tenantID uint64, userID string, ttlSeconds int) (string, error) {
	if !m.Enabled() || m.store == nil {
		return "", ErrBrowserUnavailable
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 300 // 5 minutes default
	}
	return m.store.CreatePairing(ctx, tenantID, userID, ttlSeconds)
}
