package handler

import (
	"context"
	stderrors "errors"
	"net/http"

	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// MCPMetadataResponse 在既有快照响应形状上增加 synced 与 tool_count：
// 前端可据此区分“从未同步过”（synced=false）与“已同步但目录为空”
// （synced=true 且 tool_count=0）两种状态，data 仍为原来的完整快照。
type MCPMetadataResponse struct {
	Success   bool               `json:"success"`
	Synced    bool               `json:"synced"`
	ToolCount int                `json:"tool_count"`
	Data      *types.MCPMetadata `json:"data"`
}

// GetMCPMetadata godoc
// @Summary      读取已保存的 MCP 工具目录
// @Description  只读数据库，不连接上游。未同步时 data 为 null；连接配置变更后 stale 为 true。OAuth 目录按当前授权主体隔离。
// @Tags         MCP服务
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "MCP服务ID"
// @Success      200  {object}  MCPMetadataResponse  "目录快照"
// @Failure      400  {object}  errors.AppError         "请求参数错误"
// @Failure      401  {object}  errors.AppError         "OAuth 目录缺少授权主体"
// @Failure      404  {object}  errors.AppError         "服务不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /mcp-services/{id}/metadata [get]
func (h *MCPServiceHandler) GetMCPMetadata(c *gin.Context) { h.mcpMetadata(c, false) }

// RefreshMCPMetadata godoc
// @Summary      同步 MCP 工具目录
// @Description  显式连接上游并原子替换完整目录。OAuth 服务写入当前用户的快照，Viewer 及以上可调用；静态认证写入租户共享快照，需要 Admin。
// @Tags         MCP服务
// @Accept       json
// @Produce      json
// @Param        id   path      string  true  "MCP服务ID"
// @Success      200  {object}  MCPMetadataResponse  "同步后的目录快照"
// @Failure      400  {object}  errors.AppError         "目录不完整或校验失败"
// @Failure      401  {object}  errors.AppError         "OAuth 目录缺少授权主体"
// @Failure      403  {object}  errors.AppError         "静态认证目录需要管理员刷新"
// @Failure      404  {object}  errors.AppError         "服务不存在"
// @Failure      409  {object}  errors.AppError         "刷新期间连接配置已变更"
// @Failure      503  {object}  errors.AppError         "元数据存储不可用"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /mcp-services/{id}/metadata/refresh [post]
func (h *MCPServiceHandler) RefreshMCPMetadata(c *gin.Context) { h.mcpMetadata(c, true) }

func (h *MCPServiceHandler) mcpMetadata(c *gin.Context, refresh bool) {
	ctx := c.Request.Context()
	tenant := c.GetUint64(types.TenantIDContextKey.String())
	if tenant == 0 {
		_ = c.Error(errors.NewBadRequestError("Workspace ID cannot be empty"))
		return
	}
	svc, ok := h.mcpServiceService.(interfaces.MCPMetadataService)
	if !ok {
		_ = c.Error(errors.NewServiceUnavailableError("MCP metadata storage is unavailable"))
		return
	}
	id := c.Param("id")
	var snapshot *types.MCPMetadata
	var err error
	if refresh {
		service, getErr := h.mcpServiceService.GetMCPServiceByID(ctx, tenant, id)
		if getErr != nil || service == nil {
			logger.ErrorWithFields(ctx, getErr, map[string]interface{}{
				"service_id": secutils.SanitizeForLog(id),
				"refresh":    true,
			})
			_ = c.Error(mcpMetadataAppError(types.ErrMCPServiceNotFound, true))
			return
		}
		if !service.AuthConfig.IsOAuth() && !mayWriteSharedMCPMetadata(ctx) {
			_ = c.Error(errors.NewForbiddenError("Refreshing a shared MCP directory requires an administrator"))
			return
		}
		snapshot, err = svc.RefreshMCPMetadata(ctx, tenant, id)
	} else {
		snapshot, err = svc.GetMCPMetadata(ctx, tenant, id)
	}
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"service_id": secutils.SanitizeForLog(id),
			"refresh":    refresh,
		})
		_ = c.Error(mcpMetadataAppError(err, refresh))
		return
	}
	c.JSON(http.StatusOK, newMCPMetadataResponse(snapshot))
}

// newMCPMetadataResponse 组装目录快照响应：snapshot 为 nil（从未同步）时
// synced=false、tool_count=0；非 nil 时按实际工具数计数，空目录呈现为
// synced=true 且 tool_count=0，以便前端区分两种“无工具”状态。
func newMCPMetadataResponse(snapshot *types.MCPMetadata) MCPMetadataResponse {
	response := MCPMetadataResponse{Success: true, Synced: snapshot != nil, Data: snapshot}
	if snapshot != nil {
		response.ToolCount = len(snapshot.Tools)
	}
	return response
}

// mayWriteSharedMCPMetadata is the extra gate for static-auth catalogs. The
// route stays Viewer+ so OAuth users can persist their own snapshot after
// authorizing in chat. API keys already passed manage-MCP; JWT callers need Admin.
func mayWriteSharedMCPMetadata(ctx context.Context) bool {
	if _, ok := types.TenantAPIKeyScopeFromContext(ctx); ok {
		return true
	}
	if types.IsSystemAdminFromContext(ctx) {
		return true
	}
	return types.CallerFromContext(ctx).Role.HasPermission(types.TenantRoleAdmin)
}

func mcpMetadataAppError(err error, refresh bool) *errors.AppError {
	switch {
	case stderrors.Is(err, types.ErrMCPServiceNotFound):
		return errors.NewNotFoundError("MCP service not found")
	case stderrors.Is(err, types.ErrMCPOAuthPrincipalRequired):
		return errors.NewUnauthorizedError("OAuth metadata requires an authenticated user")
	case stderrors.Is(err, types.ErrMCPMetadataStorage):
		return errors.NewServiceUnavailableError("MCP metadata storage is unavailable")
	case stderrors.Is(err, types.ErrMCPMetadataConnectionChanged):
		return errors.NewConflictError("MCP connection changed during refresh; save the configuration and sync again")
	case stderrors.Is(err, types.ErrMCPMetadataTooLarge), stderrors.Is(err, types.ErrMCPMetadataInvalidTools):
		return errors.NewBadRequestError("MCP directory is invalid or too large")
	default:
		if refresh {
			return errors.NewBadRequestError("Failed to refresh MCP tools. Check the connection and try again.")
		}
		return errors.NewInternalServerError("Failed to read MCP metadata")
	}
}
