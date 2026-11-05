package browserskill

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"time"
)

// DeviceCredential is the durable credential a browser extension uses
// to authenticate itself after pairing. It's an ed25519-based: the public
// key is stored on the server, and the private key lives in the
// extension's secure storage. On connect, the extension signs a challenge
// to prove possession of the private key.
type DeviceCredential struct {
	DeviceID   string
	PublicKey  ed25519.PublicKey
	DeviceInfo DeviceInfo
}

// DeviceInfo contains metadata about a browser device.
type DeviceInfo struct {
	DeviceName     string
	BrowserName    string
	BrowserVersion string
	OSName         string
}

// PairingService handles the pairing flow:
//  1. Server generates a pairing token (CreatePairingToken)
//  2. User enters/shows QR / QR code in the settings UI
//  3. Browser extension presents token + device info + public key to /pair endpoint
//  4. Server validates token, stores device, returns device ID
type PairingService struct {
	store *DBStore
}

// NewPairingService creates a new pairing service.
func NewPairingService(store *DBStore) *PairingService {
	return &PairingService{store: store}
}

// CreatePairingToken generates a new short-lived pairing token for a user.
func (s *PairingService) CreatePairingToken(ctx context.Context, tenantID uint64, userID string, ttlSeconds int) (string, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 300 // 5 minutes default
	}
	return s.store.CreatePairing(ctx, tenantID, userID, ttlSeconds)
}

// CompletePairing consumes a pairing token and registers a new device.
// It returns the device ID on success.
func (s *PairingService) CompletePairing(ctx context.Context, token string, deviceInfo DeviceInfo, publicKeyHex string) (string, uint64, string, error) {
	tenantID, userID, err := s.store.ConsumePairing(ctx, token)
	if err != nil {
		return "", 0, "", err
	}

	// Validate and decode the public key
	pubKeyBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil {
		return "", 0, "", ErrPairingTokenInvalid
	}
	if len(pubKeyBytes) != ed25519.PublicKeySize {
		return "", 0, "", ErrPairingTokenInvalid
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	// Generate device ID from the public key (hex-encoded truncated hash-like format)
	deviceID := "dev_" + hex.EncodeToString(pubKey[:16])

	device := &Device{
		ID:             deviceID,
		TenantID:       tenantID,
		UserID:         userID,
		DeviceName:     deviceInfo.DeviceName,
		BrowserName:    deviceInfo.BrowserName,
		BrowserVersion: deviceInfo.BrowserVersion,
		OSName:         deviceInfo.OSName,
		CreatedAt:      time.Now().Unix(),
	}

	if err := s.store.RegisterDevice(ctx, device); err != nil {
		return "", 0, "", err
	}

	return deviceID, tenantID, userID, nil
}

// VerifyDevice checks that a device ID is valid and returns its metadata.
func (s *PairingService) VerifyDevice(ctx context.Context, deviceID string) (*Device, error) {
	return s.store.GetDevice(ctx, deviceID)
}

// ListDevices lists all paired devices for a user.
func (s *PairingService) ListDevices(ctx context.Context, tenantID uint64, userID string) ([]*Device, error) {
	return s.store.ListDevices(ctx, tenantID, userID)
}

// DeleteDevice removes a paired device.
func (s *PairingService) DeleteDevice(ctx context.Context, tenantID uint64, userID string, deviceID string) error {
	return s.store.DeleteDevice(ctx, tenantID, userID, deviceID)
}
