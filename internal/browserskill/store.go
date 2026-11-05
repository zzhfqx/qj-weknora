package browserskill

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"gorm.io/gorm"
)

// DBStore is the GORM-backed implementation of the Store interface.
type DBStore struct {
	db *gorm.DB
}

// Compile-time check that DBStore implements Store.
var _ Store = (*DBStore)(nil)

// NewDBStore creates a new database-backed Store.
func NewDBStore(db *gorm.DB) *DBStore {
	return &DBStore{db: db}
}

// --- Store interface: devices ---

func (s *DBStore) ListDevices(ctx context.Context, tenantID uint64, userID string) ([]*Device, error) {
	var rows []browserDeviceRow
	err := s.db.WithContext(ctx).
		Table("browser_devices").
		Where("tenant_id = ? AND user_id = ? AND deleted_at IS NULL", tenantID, userID).
		Order("created_at DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make([]*Device, len(rows))
	for i, r := range rows {
		result[i] = rowToDevice(&r)
	}
	return result, nil
}

func (s *DBStore) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	var r browserDeviceRow
	err := s.db.WithContext(ctx).
		Table("browser_devices").
		Where("id = ? AND deleted_at IS NULL", deviceID).
		First(&r).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrDeviceNotFound
		}
		return nil, err
	}
	return rowToDevice(&r), nil
}

func (s *DBStore) RegisterDevice(ctx context.Context, device *Device) error {
	now := time.Now()
	return s.db.WithContext(ctx).
		Exec(`
			INSERT INTO browser_devices (id, tenant_id, user_id, device_name, browser_name, browser_version, os_name, last_seen_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				device_name = EXCLUDED.device_name,
				browser_name = EXCLUDED.browser_name,
				browser_version = EXCLUDED.browser_version,
				os_name = EXCLUDED.os_name,
				last_seen_at = EXCLUDED.last_seen_at,
				updated_at = EXCLUDED.updated_at,
				deleted_at = NULL
		`,
			device.ID, device.TenantID, device.UserID, device.DeviceName,
			device.BrowserName, device.BrowserVersion, device.OSName,
			now, now, now,
		).Error
}

func (s *DBStore) DeleteDevice(ctx context.Context, tenantID uint64, userID string, deviceID string) error {
	result := s.db.WithContext(ctx).
		Table("browser_devices").
		Where("id = ? AND tenant_id = ? AND user_id = ?", deviceID, tenantID, userID).
		Update("deleted_at", time.Now())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// --- Store interface: pairings ---

func (s *DBStore) CreatePairing(ctx context.Context, tenantID uint64, userID string, ttlSeconds int) (string, error) {
	token, err := generatePairingToken()
	if err != nil {
		return "", err
	}
	expiresAt := time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	err = s.db.WithContext(ctx).
		Table("browser_pairings").
		Create(map[string]interface{}{
			"token":      token,
			"tenant_id":  tenantID,
			"user_id":    userID,
			"expires_at": expiresAt,
			"created_at": time.Now(),
		}).Error
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *DBStore) ConsumePairing(ctx context.Context, token string) (tenantID uint64, userID string, err error) {
	var row struct {
		TenantID   uint64    `gorm:"column:tenant_id"`
		UserID     string    `gorm:"column:user_id"`
		ExpiresAt  time.Time `gorm:"column:expires_at"`
		ConsumedAt *time.Time `gorm:"column:consumed_at"`
	}
	err = s.db.WithContext(ctx).
		Table("browser_pairings").
		Where("token = ?", token).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, "", ErrPairingTokenInvalid
		}
		return 0, "", err
	}
	if row.ConsumedAt != nil || time.Now().After(row.ExpiresAt) {
		return 0, "", ErrPairingTokenInvalid
	}
	// Mark as consumed
	result := s.db.WithContext(ctx).
		Table("browser_pairings").
		Where("token = ? AND consumed_at IS NULL", token).
		Update("consumed_at", time.Now())
	if result.Error != nil {
		return 0, "", result.Error
	}
	if result.RowsAffected == 0 {
		return 0, "", ErrPairingTokenInvalid
	}
	return row.TenantID, row.UserID, nil
}

// --- Device activity tracking ---

// TouchDevice updates the last_seen_at timestamp for a device.
func (s *DBStore) TouchDevice(ctx context.Context, deviceID string) error {
	return s.db.WithContext(ctx).
		Table("browser_devices").
		Where("id = ?", deviceID).
		Update("last_seen_at", time.Now()).Error
}

// --- Task interruptions ---

// SaveTaskInterrupt creates or updates a task interruption record.
func (s *DBStore) SaveTaskInterrupt(ctx context.Context, tenantID uint64, sessionID, taskID, state string) error {
	now := time.Now()
	return s.db.WithContext(ctx).
		Exec(`
			INSERT INTO browser_task_interruptions (tenant_id, user_id, session_id, task_id, state, created_at, updated_at)
			SELECT ?, '', ?, ?, ?, ?, ?
			ON CONFLICT (tenant_id, session_id, task_id) DO UPDATE SET
				state = EXCLUDED.state,
				updated_at = EXCLUDED.updated_at
		`, tenantID, sessionID, taskID, state, now, now).Error
}

// GetTaskInterrupt fetches the interruption state for a task.
func (s *DBStore) GetTaskInterrupt(ctx context.Context, tenantID uint64, sessionID, taskID string) (string, error) {
	var state string
	err := s.db.WithContext(ctx).
		Table("browser_task_interruptions").
		Select("state").
		Where("tenant_id = ? AND session_id = ? AND task_id = ?", tenantID, sessionID, taskID).
		Scan(&state).Error
	if err != nil {
		return "", err
	}
	return state, nil
}

// --- internal helpers ---

// browserDeviceRow is the raw DB row shape.
type browserDeviceRow struct {
	ID             string     `gorm:"column:id"`
	TenantID       uint64     `gorm:"column:tenant_id"`
	UserID         string     `gorm:"column:user_id"`
	DeviceName     string     `gorm:"column:device_name"`
	BrowserName    string     `gorm:"column:browser_name"`
	BrowserVersion string     `gorm:"column:browser_version"`
	OSName         string     `gorm:"column:os_name"`
	LastSeenAt     *time.Time `gorm:"column:last_seen_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

func rowToDevice(r *browserDeviceRow) *Device {
	d := &Device{
		ID:             r.ID,
		TenantID:       r.TenantID,
		UserID:         r.UserID,
		DeviceName:     r.DeviceName,
		BrowserName:    r.BrowserName,
		BrowserVersion: r.BrowserVersion,
		OSName:         r.OSName,
		CreatedAt:      r.CreatedAt.Unix(),
	}
	if r.LastSeenAt != nil {
		d.LastSeenAt = r.LastSeenAt.Unix()
	}
	return d
}

func generatePairingToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
