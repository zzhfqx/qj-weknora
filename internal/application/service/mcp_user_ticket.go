package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// mcpUserTicketType 是身份票据的独立用途标识，Parse 时拒绝登录
	// access token / sandbox 票据等其它类型票据混用（仿 sandbox_terminal）。
	mcpUserTicketType = "mcp_user_identity"
	// DefaultMCPUserTicketTTL 为票据默认有效期。网关在转发每个 MCP 请求时
	// 按本机时钟即时签发，鉴权发生在 HTTP 请求建立时刻，短 TTL 不影响
	// SSE 长连，同时把泄露窗口压到最小。
	DefaultMCPUserTicketTTL = 5 * time.Minute
)

// MCPUserTicketClaims 是 X-User-Credential 票据的验签结果：
//   - UserID：千机外部用户 id（claims.sub）；
//   - TenantID：weknora 工作空间 id（claims.tid），中间件须校验与端点
//     所属工作空间一致；
//   - EndpointID：绑定的 MCP 端点 id（claims.aud），防跨端点重放。
type MCPUserTicketClaims struct {
	UserID     string
	TenantID   uint64
	EndpointID string
}

// IssueMCPUserTicket 签发一枚短期用户身份票据（HS256，密钥取 JWT_SECRET）。
// ttl <= 0 时使用 DefaultMCPUserTicketTTL。
func IssueMCPUserTicket(userID string, tenantID uint64, endpointID string, ttl time.Duration) (string, error) {
	userID = strings.TrimSpace(userID)
	endpointID = strings.TrimSpace(endpointID)
	if userID == "" || endpointID == "" || tenantID == 0 {
		return "", errors.New("mcp user ticket requires user, tenant and endpoint")
	}
	if ttl <= 0 {
		ttl = DefaultMCPUserTicketTTL
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"type": mcpUserTicketType,
		"sub":  userID,
		"tid":  tenantID,
		"aud":  endpointID,
		"jti":  uuid.New().String(),
		"iat":  now.Unix(),
		"exp":  now.Add(ttl).Unix(),
	})
	return token.SignedString([]byte(getJwtSecret()))
}

// ParseMCPUserTicket 校验用户身份票据：签名、HMAC 算法、过期时间、type 用途
// 隔离以及必填字段，任一不满足即返回错误（调用方 fail-closed 返回 401）。
func ParseMCPUserTicket(raw string) (*MCPUserTicketClaims, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("missing mcp user ticket")
	}
	token, err := jwt.Parse(raw, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(getJwtSecret()), nil
	})
	if err != nil || token == nil || !token.Valid {
		return nil, errors.New("invalid or expired mcp user ticket")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, errors.New("invalid mcp user ticket claims")
	}
	if tokenType, _ := claims["type"].(string); tokenType != mcpUserTicketType {
		return nil, errors.New("not an mcp user identity ticket")
	}
	userID, _ := claims["sub"].(string)
	endpointID, _ := claims["aud"].(string)
	tenantID := tenantIDFromClaimsTID(claims)
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(endpointID) == "" || tenantID == 0 {
		return nil, errors.New("invalid mcp user ticket claims")
	}
	return &MCPUserTicketClaims{
		UserID:     strings.TrimSpace(userID),
		TenantID:   tenantID,
		EndpointID: strings.TrimSpace(endpointID),
	}, nil
}

// tenantIDFromClaimsTID 从 "tid" 字段取租户 id。通用 helper tenantIDFromClaims
// 固定读 "tenant_id" 键，而身份票据按方案使用短键 "tid"，故单列一个极小
// 转换（JSON 数字在 MapClaims 中为 float64）。
func tenantIDFromClaimsTID(claims jwt.MapClaims) uint64 {
	switch v := claims["tid"].(type) {
	case float64:
		if v > 0 {
			return uint64(v)
		}
	case int64:
		if v > 0 {
			return uint64(v)
		}
	case uint64:
		if v > 0 {
			return v
		}
	}
	return 0
}
