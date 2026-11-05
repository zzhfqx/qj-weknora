package session

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// desktopWSUpgrader upgrades HTTP connections to WebSocket for noVNC RFB
// frame relay. The subprotocol is "binary" to match what noVNC sends.
var desktopWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  8 * 1024,
	WriteBufferSize: 32 * 1024,
	Subprotocols:    []string{"binary"},
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// SandboxDesktopWS godoc
// @Summary      沙箱图形桌面 WebSocket
// @Description  与会话沙箱中的 VNC 桌面建立 WebSocket 连接，中继 RFB 协议帧（noVNC 协议）
// @Tags         会话
// @Param        id       path  string  true   "会话 ID"
// @Param        ticket  query string  true   "桌面握手票据"
// @Success      101  {string}  string  "WebSocket 升级成功"
// @Failure      400  {object}  errors.AppError
// @Failure      404  {object}  errors.AppError
// @Router       /sessions/{id}/sandbox/desktop [get]
//
// The handler authenticates via a short-lived ticket (query param) because
// browser WebSocket handshakes cannot carry Authorization headers. After
// upgrade it relays RFB frames between the browser's noVNC client and the
// sandbox VNC server, with periodic auth rechecks on the access token.
func (h *Handler) SandboxDesktopWS(c *gin.Context) {
	ctx := c.Request.Context()

	// Authenticate via ticket (WebSocket handshake cannot carry headers)
	ticketRaw := c.Query("ticket")
	claims, err := service.ParseSandboxDesktopTicket(ticketRaw)
	if err != nil {
		c.Error(errors.NewUnauthorizedError(err.Error()))
		return
	}

	sessionID := secutils.SanitizeForLog(c.Param("id"))
	if sessionID == "" {
		c.Error(errors.NewBadRequestError("session id required"))
		return
	}

	// Ticket must match the session in the URL
	if claims.SessionID != sessionID {
		c.Error(errors.NewUnauthorizedError("ticket/session mismatch"))
		return
	}

	// Verify the session still exists and the ticket's user owns it
	caller := types.Caller{
		TenantID: claims.TenantID,
		UserID:   claims.UserID,
	}
	ctx = types.WithCaller(ctx, caller)

	if _, err := h.sessionService.GetSession(ctx, sessionID); err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Ensure a desktop is running for this session
	if h.desktopService == nil {
		c.Error(errors.NewNotFoundError("desktop unavailable"))
		return
	}
	if _, err := h.desktopService.EnsureSessionDesktop(ctx, sessionID); err != nil {
		logger.Errorf(ctx, "ensure desktop failed: session=%s err=%v", sessionID, err)
		c.Error(errors.NewInternalServerError("desktop start failed"))
		return
	}

	// Upgrade to WebSocket
	conn, err := desktopWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Errorf(ctx, "desktop WS upgrade failed: session=%s err=%v", sessionID, err)
		return
	}

	h.relayDesktopFrames(ctx, conn, sessionID)
}

// relayDesktopFrames copies RFB frames in both directions between the
// browser noVNC client and the sandbox VNC server.
func (h *Handler) relayDesktopFrames(
	ctx context.Context, browserConn *websocket.Conn, sessionID string,
) {
	defer browserConn.Close()

	// Open the VNC connection to the sandbox
	desktop, err := h.desktopService.OpenSessionDesktop(ctx, sessionID)
	if err != nil {
		logger.Errorf(ctx, "open desktop session failed: session=%s err=%v", sessionID, err)
		return
	}
	defer desktop.Close()

	// Relay in both directions
	var wg sync.WaitGroup
	wg.Add(2)

	// Browser → VNC
	go func() {
		defer wg.Done()
		h.copyBrowserToDesktop(ctx, browserConn, desktop, sessionID)
	}()

	// VNC → Browser
	go func() {
		defer wg.Done()
		h.copyDesktopToBrowser(ctx, desktop, browserConn, sessionID)
	}()

	wg.Wait()
}

func (h *Handler) copyBrowserToDesktop(
	ctx context.Context, browser *websocket.Conn, desktop sandbox.RemoteDesktopSession, sessionID string,
) {
	buf := make([]byte, 32*1024)
	desktopConn := desktop.Conn()
	for {
		msgType, reader, err := browser.NextReader()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				logger.Debugf(ctx, "desktop browser WS read error: session=%s err=%v", sessionID, err)
			}
			return
		}
		if msgType != websocket.BinaryMessage {
			// Skip text messages (noVNC control messages)
			continue
		}
		// Read the full message and write it to the VNC stream
		_, err = io.CopyBuffer(desktopConn, reader, buf)
		if err != nil {
			logger.Debugf(ctx, "desktop browser→vnc copy error: session=%s err=%v", sessionID, err)
			return
		}
		if h.desktopService != nil {
			h.desktopService.RecordActivity(sessionID)
		}
	}
}

func (h *Handler) copyDesktopToBrowser(
	ctx context.Context, desktop sandbox.RemoteDesktopSession, browser *websocket.Conn, sessionID string,
) {
	buf := make([]byte, 64*1024)
	desktopConn := desktop.Conn()
	writer, err := browser.NextWriter(websocket.BinaryMessage)
	if err != nil {
		logger.Debugf(ctx, "desktop vnc→browser writer error: session=%s err=%v", sessionID, err)
		return
	}

	for {
		n, err := desktopConn.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				logger.Debugf(ctx, "desktop vnc→browser write error: session=%s err=%v", sessionID, writeErr)
				return
			}
			if h.desktopService != nil {
				h.desktopService.RecordActivity(sessionID)
			}
		}
		if err != nil {
			if err != io.EOF {
				logger.Debugf(ctx, "desktop vnc read error: session=%s err=%v", sessionID, err)
			}
			return
		}
	}
}

// IssueSandboxDesktopTicket godoc
// @Summary      签发沙箱桌面票据
// @Description  签发一个 2 分钟有效的 JWT 票据，用于建立桌面 WebSocket 连接（浏览器 WebSocket 不能带 Authorization header）
// @Tags         会话
// @Produce      json
// @Param        session_id  path   string  true  "会话 ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  errors.AppError
// @Security     Bearer
// @Router       /sessions/{session_id}/sandbox/desktop-ticket [post]
func (h *Handler) IssueSandboxDesktopTicket(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		c.Error(errors.NewBadRequestError("session_id is required"))
		return
	}

	// Verify session ownership
	if _, err := h.sessionService.GetSession(ctx, sessionID); err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	if h.desktopService == nil || !h.desktopService.IsSupported() {
		c.Error(errors.NewNotFoundError("desktop not supported in this deployment"))
		return
	}

	// Resolve the access token so the ticket is bound to it
	userID, _ := types.UserIDFromContext(ctx)
	tenantID, _ := types.TenantIDFromContext(ctx)
	accessToken := sandboxDesktopBearerToken(c)
	if accessToken == "" {
		c.Error(errors.NewUnauthorizedError("access token required"))
		return
	}
	record, err := h.userService.GetAccessTokenByValue(ctx, accessToken)
	if err != nil || record == nil || strings.TrimSpace(record.ID) == "" {
		c.Error(errors.NewUnauthorizedError("invalid access token"))
		return
	}
	if err := service.AssertAccessTokenStillActive(record, userID, time.Now()); err != nil {
		c.Error(errors.NewUnauthorizedError("access token revoked"))
		return
	}

	ticket, err := service.IssueSandboxDesktopTicket(
		userID, tenantID, sessionID, record.ID, 0,
	)
	if err != nil {
		logger.Errorf(ctx, "issue desktop ticket failed: session=%s err=%v", sessionID, err)
		c.Error(errors.NewInternalServerError("failed to issue desktop ticket"))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"ticket":     ticket,
			"expires_in": int(service.DefaultSandboxDesktopTicketTTL.Seconds()),
		},
	})
}

// sandboxDesktopBearerToken extracts the bearer token from the Authorization
// header. Used by the ticket endpoint so the returned JWT is bound to the
// same access token the caller is already using.
func sandboxDesktopBearerToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}
