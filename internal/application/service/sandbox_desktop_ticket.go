package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	sandboxDesktopTicketType = "sandbox_desktop"
	// DefaultSandboxDesktopTicketTTL is long enough for the WebSocket
	// handshake, short enough that a leaked query string is not a 24h
	// session. After upgrade the ticket itself is discarded; the open
	// desktop rechecks the bound access-token id instead.
	DefaultSandboxDesktopTicketTTL = 2 * time.Minute
)

// SandboxDesktopTicketClaims is the handshake identity bound to one session's
// desktop. TokenID is the auth_tokens.id of the access JWT that minted the
// ticket; the open desktop rechecks that row so logout / revocation tear
// the bridge down.
type SandboxDesktopTicketClaims struct {
	UserID    string
	TenantID  uint64
	SessionID string
	TokenID   string
}

// IssueSandboxDesktopTicket mints a short-lived JWT for the browser noVNC
// WebSocket handshake. It is not an access token: ValidateToken rejects it.
func IssueSandboxDesktopTicket(userID string, tenantID uint64, sessionID, tokenID string, ttl time.Duration) (string, error) {
	userID = strings.TrimSpace(userID)
	sessionID = strings.TrimSpace(sessionID)
	tokenID = strings.TrimSpace(tokenID)
	if userID == "" || sessionID == "" || tokenID == "" || tenantID == 0 {
		return "", errors.New("sandbox desktop ticket requires user, tenant, session, and access token")
	}
	if ttl <= 0 {
		ttl = DefaultSandboxDesktopTicketTTL
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":    userID,
		"tenant_id":  tenantID,
		"session_id": sessionID,
		"token_id":   tokenID,
		"type":       sandboxDesktopTicketType,
		"exp":        time.Now().Add(ttl).Unix(),
		"iat":        time.Now().Unix(),
	})
	return token.SignedString([]byte(getJwtSecret()))
}

// ParseSandboxDesktopTicket validates a desktop handshake ticket. Access
// and refresh tokens are rejected.
func ParseSandboxDesktopTicket(raw string) (*SandboxDesktopTicketClaims, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("missing desktop ticket")
	}
	token, err := jwt.Parse(raw, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(getJwtSecret()), nil
	})
	if err != nil || token == nil || !token.Valid {
		return nil, errors.New("invalid or expired desktop ticket")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid desktop ticket claims")
	}
	tokenType, _ := claims["type"].(string)
	if tokenType != sandboxDesktopTicketType {
		return nil, errors.New("not a desktop ticket")
	}
	userID, _ := claims["user_id"].(string)
	sessionID, _ := claims["session_id"].(string)
	tokenID, _ := claims["token_id"].(string)
	tenantID := uint64FromClaims(claims, "tenant_id")
	return &SandboxDesktopTicketClaims{
		UserID:    userID,
		TenantID:  tenantID,
		SessionID: sessionID,
		TokenID:   tokenID,
	}, nil
}

// uint64FromClaims reads a numeric claim as uint64, tolerating both JSON
// numbers (float64) and string forms.
func uint64FromClaims(claims jwt.MapClaims, key string) uint64 {
	switch v := claims[key].(type) {
	case float64:
		return uint64(v)
	case int64:
		return uint64(v)
	case uint64:
		return v
	case string:
		var n uint64
		fmt.Sscanf(v, "%d", &n)
		return n
	default:
		return 0
	}
}
