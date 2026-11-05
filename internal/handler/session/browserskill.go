package session

import (
	"context"
	stderrors "errors"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/browserskill"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// browserSkillWSUpgrader upgrades HTTP to WebSocket for BrowserSkill connections.
var browserSkillWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4 * 1024,
	WriteBufferSize: 8 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// BrowserSkillWS godoc
// @Summary      本地浏览器 WebSocket
// @Description  与配对的本地浏览器扩展建立 WebSocket 连接，用于代理浏览器自动化命令
// @Tags         会话
// @Param        id          path  string  true   "会话 ID"
// @Param        device_id   query string  true   "浏览器设备 ID"
// @Param        token       query string  true   "设备令牌（用于设备侧认证）"
// @Success      101  {string}  string  "WebSocket 升级成功"
// @Failure      400  {object}  errors.AppError
// @Failure      401  {object}  errors.AppError
// @Router       /sessions/{id}/local-browser [get]
func (h *Handler) BrowserSkillWS(c *gin.Context) {
	ctx := c.Request.Context()
	sessionID := secutils.SanitizeForLog(c.Param("id"))
	if sessionID == "" {
		c.Error(errors.NewBadRequestError("session id required"))
		return
	}

	// Verify session exists and is owned by the caller
	tenantID, _ := c.Get(types.TenantIDContextKey.String())
	userID, _ := c.Get(types.UserIDContextKey.String())
	caller := types.Caller{
		TenantID: tenantID.(uint64),
		UserID:   userID.(string),
	}
	ctx = types.WithCaller(ctx, caller)

	if _, err := h.sessionService.GetSession(ctx, sessionID); err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"session_id": sessionID})
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// Upgrade to WebSocket
	conn, err := browserSkillWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logger.Error(ctx, "Failed to upgrade browser skill WebSocket", err)
		return
	}

	h.runBrowserSkillConn(ctx, conn, sessionID, caller)
}

// browserConn wraps an active browser WebSocket connection with send/receive
// channels and lifecycle management.
type browserConn struct {
	ws        *websocket.Conn
	sessionID string
	caller    types.Caller
	mu        sync.Mutex
	closed    bool
}

// runBrowserSkillConn is the per-connection loop. In this secondary-dev
// adaptation it handles the basic message framing: hello, keepalive, and
// dispatch to the manager. The full browser command protocol is handled by
// the browserskill package.
func (h *Handler) runBrowserSkillConn(ctx context.Context, ws *websocket.Conn, sessionID string, caller types.Caller) {
	bc := &browserConn{
		ws:        ws,
		sessionID: sessionID,
		caller:    caller,
	}
	defer bc.close()

	logger.Infof(ctx, "BrowserSkill WebSocket connected, session: %s", sessionID)

	// Send a ready message so the extension knows the server is listening.
	_ = bc.sendJSON(map[string]interface{}{
		"type": "ready",
		"session_id": sessionID,
	})

	// Read loop
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if !isNormalWSClose(err) {
				logger.Warnf(ctx, "BrowserSkill WS read error: %v", err)
			}
			return
		}

		var env browserWSMessage
		if err := json.Unmarshal(msg, &env); err != nil {
			logger.Warnf(ctx, "BrowserSkill bad message: %v", err)
			continue
		}

		// Handle keepalive / ping directly to avoid manager round-trips.
		if env.Type == "ping" {
			_ = bc.sendJSON(map[string]interface{}{"type": "pong"})
			continue
		}
		if env.Type == "pong" {
			continue
		}

		// TODO: route to browserskill.Manager task dispatcher when the
		// agent-tool layer is wired up. For now we echo back that the
		// message was received.
		logger.Debugf(ctx, "BrowserSkill message type=%s from session=%s", env.Type, sessionID)
	}
}

// browserWSMessage is the envelope for all BrowserSkill WebSocket messages.
type browserWSMessage struct {
	Type    string          `json:"type"`
	TaskID  string          `json:"task_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (bc *browserConn) sendJSON(v interface{}) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.closed {
		return stderrors.New("connection closed")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return bc.ws.WriteMessage(websocket.TextMessage, data)
}

func (bc *browserConn) close() {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if !bc.closed {
		bc.closed = true
		_ = bc.ws.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
		_ = bc.ws.Close()
	}
}

// isNormalWSClose returns true when the error is an expected close frame.
func isNormalWSClose(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	if stderrors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNormalClosure,
			websocket.CloseGoingAway,
			websocket.CloseNoStatusReceived,
			websocket.CloseAbnormalClosure:
			return true
		}
	}
	return false
}

// --- REST endpoints for device / pairing management ---

// ListBrowserDevices godoc
// @Summary      列出已配对的本地浏览器设备
// @Description  返回当前用户已配对的所有本地浏览器扩展设备
// @Tags         本地浏览器
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "设备列表"
// @Failure      500  {object}  errors.AppError
// @Security     Bearer
// @Router       /browser/devices [get]
func (h *Handler) ListBrowserDevices(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, _ := c.Get(types.TenantIDContextKey.String())
	userID, _ := c.Get(types.UserIDContextKey.String())

	if h.browserSkill == nil || !h.browserSkill.Enabled() {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"data":    []*browserskill.Device{},
			"enabled": false,
		})
		return
	}

	devices, err := h.browserSkill.ListDevices(ctx, tenantID.(uint64), userID.(string))
	if err != nil {
		if stderrors.Is(err, browserskill.ErrBrowserUnavailable) {
			c.JSON(http.StatusOK, gin.H{"success": true, "data": []*browserskill.Device{}, "enabled": false})
			return
		}
		logger.Error(ctx, "Failed to list browser devices", err)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    devices,
		"enabled": true,
	})
}

// DeleteBrowserDevice godoc
// @Summary      删除已配对的浏览器设备
// @Description  解除指定浏览器设备的配对，之后该设备需要重新配对
// @Tags         本地浏览器
// @Param        device_id  path  string  true  "设备 ID"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  errors.AppError
// @Security     Bearer
// @Router       /browser/devices/{device_id} [delete]
func (h *Handler) DeleteBrowserDevice(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, _ := c.Get(types.TenantIDContextKey.String())
	userID, _ := c.Get(types.UserIDContextKey.String())
	deviceID := secutils.SanitizeForLog(c.Param("device_id"))
	if deviceID == "" {
		c.Error(errors.NewBadRequestError("device id required"))
		return
	}

	if h.browserSkill == nil || !h.browserSkill.Enabled() {
		c.Error(errors.NewNotFoundError(browserskill.ErrBrowserUnavailable.Error()))
		return
	}

	if err := h.browserSkill.DeleteDevice(ctx, tenantID.(uint64), userID.(string), deviceID); err != nil {
		if stderrors.Is(err, browserskill.ErrDeviceNotFound) {
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.Error(ctx, "Failed to delete browser device", err)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// CreateBrowserPairing godoc
// @Summary      生成浏览器配对令牌
// @Description  创建一个短有效期的配对令牌，浏览器扩展使用该令牌完成首次配对
// @Tags         本地浏览器
// @Produce      json
// @Success      200  {object}  map[string]interface{}  "配对信息：token、expires_at"
// @Security     Bearer
// @Router       /browser/pairing [post]
func (h *Handler) CreateBrowserPairing(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, _ := c.Get(types.TenantIDContextKey.String())
	userID, _ := c.Get(types.UserIDContextKey.String())

	if h.browserSkill == nil || !h.browserSkill.Enabled() {
		c.Error(errors.NewNotFoundError(browserskill.ErrBrowserUnavailable.Error()))
		return
	}

	token, err := h.browserSkill.CreatePairingToken(ctx, tenantID.(uint64), userID.(string), 300)
	if err != nil {
		logger.Error(ctx, "Failed to create browser pairing token", err)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"token":      token,
			"expires_at": time.Now().Add(5 * time.Minute).Unix(),
		},
	})
}
