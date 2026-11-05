package middleware

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

const (
	// mcpUserCredentialHeader 是网关注入的每请求用户身份票据头。
	mcpUserCredentialHeader = "X-User-Credential"
	// mcpRequireUserIdentityEnv 为灰度开关：默认 true（无头/非法即 401）；
	// 设为 false/0/no/off 时仅对无头调用告警放行，仅供发布过渡期排障。
	mcpRequireUserIdentityEnv = "MCP_REQUIRE_USER_IDENTITY"
)

// MCPUserIdentity 是挂在 MCPEndpointAuth 之后的"用户身份层"：从
// X-User-Credential 头取出 mcp_user_identity 票据，验签并与已认证端点做
// 交叉校验（tid=端点工作空间、aud=端点 id），通过后把外部用户 id 同时
// 写入 gin keys 与 request context 两个表面（纪律同 applyAuthSession）。
//
// 任何失败一律 401 中断（fail-closed），严禁退化为"无身份=机器用户全开"。
func MCPUserIdentity() gin.HandlerFunc {
	requireIdentity := envRequireUserIdentity()
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(mcpUserCredentialHeader))
		if raw == "" {
			if !requireIdentity {
				// 灰度模式：不拦截，但留下可按 endpoint 检索的无头调用告警。
				logger.Warnf(c.Request.Context(),
					"[mcp-user-identity] headerless MCP call allowed (MCP_REQUIRE_USER_IDENTITY=false)")
				c.Next()
				return
			}
			abortMCPIdentity(c, "unauthorized: missing user credential")
			return
		}

		claims, err := service.ParseMCPUserTicket(raw)
		if err != nil {
			logger.Warnf(c.Request.Context(),
				"[mcp-user-identity] invalid user credential: %v", err)
			abortMCPIdentity(c, "unauthorized: invalid user credential")
			return
		}

		// 交叉校验：端点已由 MCPEndpointAuth 写入 context；票据必须签给
		// 同一工作空间的同一个端点，防 A 工作空间/端点的签名单跨面重放。
		ep, ok := MCPEndpointFromContext(c.Request.Context())
		if !ok || ep == nil {
			// 正常路径不会发生（中间件链顺序保证），按服务不可用处理。
			c.JSON(http.StatusInternalServerError, gin.H{"error": "endpoint context unavailable"})
			c.Abort()
			return
		}
		if claims.TenantID != ep.TenantID || claims.EndpointID != ep.ID {
			logger.Warnf(c.Request.Context(),
				"[mcp-user-identity] credential scope mismatch: endpoint=%s tenant=%d",
				ep.ID, ep.TenantID)
			abortMCPIdentity(c, "unauthorized: user credential does not match this endpoint")
			return
		}

		// user_id 的唯一可信来源是这里的已验签票据；同时写两个表面，
		// 保证 gin 风格与 context 风格的下游读取方拿到同一值。
		c.Set(types.MCPUserIDContextKey.String(), claims.UserID)
		ctx := context.WithValue(c.Request.Context(), types.MCPUserIDContextKey, claims.UserID)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// abortMCPIdentity 输出统一形态的 401。
func abortMCPIdentity(c *gin.Context, message string) {
	c.Header("WWW-Authenticate", `Bearer realm="weknora-mcp-user"`)
	c.JSON(http.StatusUnauthorized, gin.H{"error": message})
	c.Abort()
}

// envRequireUserIdentity 读取灰度开关；未设置或无法识别为"关闭"时默认 true。
func envRequireUserIdentity() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(mcpRequireUserIdentityEnv))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}
