package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// fakeEndpointPrelude 模拟 MCPEndpointAuth：把端点写进两个 context 表面。
func fakeEndpointPrelude(ep *types.MCPEndpoint) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(types.MCPEndpointContextKey.String(), ep)
		ctx := context.WithValue(c.Request.Context(), types.MCPEndpointContextKey, ep)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func newIdentityRouter(t *testing.T, ep *types.MCPEndpoint) (*gin.Engine, *string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var seenUser string
	r.POST("/mcp/:endpoint_id",
		fakeEndpointPrelude(ep),
		MCPUserIdentity(),
		func(c *gin.Context) {
			seenUser, _ = types.MCPUserIDFromContext(c.Request.Context())
			c.String(http.StatusOK, "ok")
		},
	)
	return r, &seenUser
}

func identityRequest(t *testing.T, r *gin.Engine, epID, credential string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp/"+epID, nil)
	if credential != "" {
		req.Header.Set("X-User-Credential", credential)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMCPUserIdentityRequiresHeader(t *testing.T) {
	t.Setenv("JWT_SECRET", "mcp-user-identity-test-secret")
	t.Setenv("MCP_REQUIRE_USER_IDENTITY", "true")

	ep := &types.MCPEndpoint{ID: "ep1", TenantID: 7}
	r, _ := newIdentityRouter(t, ep)

	w := identityRequest(t, r, "ep1", "")
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMCPUserIdentityRejectsBadSignatureAndExpired(t *testing.T) {
	t.Setenv("JWT_SECRET", "mcp-user-identity-test-secret")
	t.Setenv("MCP_REQUIRE_USER_IDENTITY", "true")

	ep := &types.MCPEndpoint{ID: "ep1", TenantID: 7}
	r, _ := newIdentityRouter(t, ep)

	// 错密钥签名。
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"type": "mcp_user_identity",
		"sub":  "u1",
		"tid":  7,
		"aud":  "ep1",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString([]byte("wrong-secret"))
	require.NoError(t, err)
	w := identityRequest(t, r, "ep1", raw)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// 过期票据：Issue 对负 ttl 会回退默认值，故手工造 exp 在过去的票据。
	expiredTok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"type": "mcp_user_identity",
		"sub":  "u1",
		"tid":  7,
		"aud":  "ep1",
		"iat":  time.Now().Add(-2 * time.Hour).Unix(),
		"exp":  time.Now().Add(-time.Hour).Unix(),
	})
	rawExpired, err := expiredTok.SignedString([]byte("mcp-user-identity-test-secret"))
	require.NoError(t, err)
	w = identityRequest(t, r, "ep1", rawExpired)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMCPUserIdentityRejectsScopeMismatch(t *testing.T) {
	t.Setenv("JWT_SECRET", "mcp-user-identity-test-secret")
	t.Setenv("MCP_REQUIRE_USER_IDENTITY", "true")

	ep := &types.MCPEndpoint{ID: "ep1", TenantID: 7}
	r, _ := newIdentityRouter(t, ep)

	// tid 与端点工作空间不一致。
	raw, err := service.IssueMCPUserTicket("u1", 8, "ep1", time.Minute)
	require.NoError(t, err)
	w := identityRequest(t, r, "ep1", raw)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	// aud 与端点 id 不一致。
	raw, err = service.IssueMCPUserTicket("u1", 7, "ep-other", time.Minute)
	require.NoError(t, err)
	w = identityRequest(t, r, "ep1", raw)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMCPUserIdentityAcceptsValidTicket(t *testing.T) {
	t.Setenv("JWT_SECRET", "mcp-user-identity-test-secret")
	t.Setenv("MCP_REQUIRE_USER_IDENTITY", "true")

	ep := &types.MCPEndpoint{ID: "ep1", TenantID: 7}
	r, seenUser := newIdentityRouter(t, ep)

	raw, err := service.IssueMCPUserTicket("qianji-user-9", 7, "ep1", time.Minute)
	require.NoError(t, err)
	w := identityRequest(t, r, "ep1", raw)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "qianji-user-9", *seenUser)
}

// 其它类型票据（如登录 access token 形态）即使签名正确也必须拒绝。
func TestMCPUserIdentityRejectsWrongTicketType(t *testing.T) {
	t.Setenv("JWT_SECRET", "mcp-user-identity-test-secret")
	t.Setenv("MCP_REQUIRE_USER_IDENTITY", "true")

	ep := &types.MCPEndpoint{ID: "ep1", TenantID: 7}
	r, _ := newIdentityRouter(t, ep)

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"type": "access",
		"sub":  "u1",
		"tid":  7,
		"aud":  "ep1",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	raw, err := tok.SignedString([]byte("mcp-user-identity-test-secret"))
	require.NoError(t, err)
	w := identityRequest(t, r, "ep1", raw)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// 灰度开关关闭时，无头调用仅告警放行，且 ctx 中没有用户身份。
func TestMCPUserIdentityWarnOnlyWhenDisabled(t *testing.T) {
	t.Setenv("JWT_SECRET", "mcp-user-identity-test-secret")
	t.Setenv("MCP_REQUIRE_USER_IDENTITY", "false")

	ep := &types.MCPEndpoint{ID: "ep1", TenantID: 7}
	r, seenUser := newIdentityRouter(t, ep)

	w := identityRequest(t, r, "ep1", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "", *seenUser)
}
