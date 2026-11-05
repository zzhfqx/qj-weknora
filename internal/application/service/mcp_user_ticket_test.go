package service

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// 用例 1：合法票据签发后可解析，全部 claims 正确，默认 TTL 为 5 分钟。
func TestMCPUserTicketRoundTrip(t *testing.T) {
	raw, err := IssueMCPUserTicket("qianji-user-7", 42, "ep-abc", 0)
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	claims, err := ParseMCPUserTicket(raw)
	require.NoError(t, err)
	require.Equal(t, "qianji-user-7", claims.UserID)
	require.Equal(t, uint64(42), claims.TenantID)
	require.Equal(t, "ep-abc", claims.EndpointID)
}

// 用例 2：过期票据被拒。
func TestMCPUserTicketRejectsExpired(t *testing.T) {
	// Issue 对 ttl<=0 会回退默认值，故过期票用手工 claims 构造。
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  "u1",
		"tid":  1,
		"aud":  "ep1",
		"type": mcpUserTicketType,
		"iat":  time.Now().Add(-2 * time.Hour).Unix(),
		"exp":  time.Now().Add(-time.Hour).Unix(),
	})
	raw, err := tok.SignedString([]byte(getJwtSecret()))
	require.NoError(t, err)
	_, err = ParseMCPUserTicket(raw)
	require.Error(t, err)
}

// 用例 3：type 不符（登录 access token / sandbox 票据形态）被拒。
func TestMCPUserTicketRejectsWrongType(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  "u1",
		"tid":  1,
		"aud":  "ep1",
		"type": "sandbox_terminal",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString([]byte(getJwtSecret()))
	require.NoError(t, err)
	_, err = ParseMCPUserTicket(raw)
	require.Error(t, err)

	// 完全没有 type 字段（普通 access token 形态）同样拒绝。
	tok = jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u1",
		"tid": 1,
		"aud": "ep1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	raw, err = tok.SignedString([]byte(getJwtSecret()))
	require.NoError(t, err)
	_, err = ParseMCPUserTicket(raw)
	require.Error(t, err)
}

// 用例 4：用错误密钥签名的票据被拒。
func TestMCPUserTicketRejectsWrongSecret(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":  "u1",
		"tid":  1,
		"aud":  "ep1",
		"type": mcpUserTicketType,
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString([]byte("a-totally-different-secret"))
	require.NoError(t, err)
	_, err = ParseMCPUserTicket(raw)
	require.Error(t, err)
}

// 用例 5：必填字段缺失/为空被拒（签发与解析两侧）。
func TestMCPUserTicketRejectsMissingFields(t *testing.T) {
	_, err := IssueMCPUserTicket("", 1, "ep1", time.Minute)
	require.Error(t, err)
	_, err = IssueMCPUserTicket("u1", 0, "ep1", time.Minute)
	require.Error(t, err)
	_, err = IssueMCPUserTicket("u1", 1, "", time.Minute)
	require.Error(t, err)

	sign := func(claims jwt.MapClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		raw, signErr := tok.SignedString([]byte(getJwtSecret()))
		require.NoError(t, signErr)
		return raw
	}
	base := jwt.MapClaims{
		"sub":  "u1",
		"tid":  1,
		"aud":  "ep1",
		"type": mcpUserTicketType,
		"exp":  time.Now().Add(time.Hour).Unix(),
	}
	without := func(key string) jwt.MapClaims {
		out := jwt.MapClaims{}
		for k, v := range base {
			out[k] = v
		}
		delete(out, key)
		return out
	}
	_, err = ParseMCPUserTicket(sign(without("sub")))
	require.Error(t, err)
	_, err = ParseMCPUserTicket(sign(without("tid")))
	require.Error(t, err)
	_, err = ParseMCPUserTicket(sign(without("aud")))
	require.Error(t, err)
}

// 用例 6：票据被篡改（改动 payload 字符）后签名失效，被拒。
func TestMCPUserTicketRejectsTampered(t *testing.T) {
	raw, err := IssueMCPUserTicket("u1", 1, "ep1", time.Hour)
	require.NoError(t, err)

	parts := strings.Split(raw, ".")
	require.Len(t, parts, 3)
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	_, err = ParseMCPUserTicket(tampered)
	require.Error(t, err)

	// 非 JWT 垃圾串同样拒绝。
	_, err = ParseMCPUserTicket("not-a-jwt")
	require.Error(t, err)
	_, err = ParseMCPUserTicket("   ")
	require.Error(t, err)
}
