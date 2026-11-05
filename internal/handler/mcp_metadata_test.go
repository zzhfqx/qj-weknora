package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestMCPMetadataAppErrorDoesNotLeakUpstreamDetails(t *testing.T) {
	notFound := mcpMetadataAppError(types.ErrMCPServiceNotFound, false)
	require.Equal(t, http.StatusNotFound, notFound.HTTPCode)
	require.Equal(t, "MCP service not found", notFound.Message)

	unauthorized := mcpMetadataAppError(types.ErrMCPOAuthPrincipalRequired, true)
	require.Equal(t, http.StatusUnauthorized, unauthorized.HTTPCode)

	conflict := mcpMetadataAppError(types.ErrMCPMetadataConnectionChanged, true)
	require.Equal(t, http.StatusConflict, conflict.HTTPCode)

	unavailable := mcpMetadataAppError(types.ErrMCPMetadataStorage, false)
	require.Equal(t, http.StatusServiceUnavailable, unavailable.HTTPCode)

	refresh := mcpMetadataAppError(errors.New("dial tcp 10.1.2.3:443 https://secret.internal/mcp"), true)
	require.Equal(t, http.StatusBadRequest, refresh.HTTPCode)
	require.Equal(t, "Failed to refresh MCP tools. Check the connection and try again.", refresh.Message)
	require.NotContains(t, refresh.Error(), "secret.internal")
	require.NotContains(t, refresh.Error(), "10.1.2.3")
}

func TestMayWriteSharedMCPMetadata(t *testing.T) {
	require.False(t, mayWriteSharedMCPMetadata(context.Background()))
	require.False(t, mayWriteSharedMCPMetadata(types.WithCaller(
		context.Background(),
		types.Caller{Role: types.TenantRoleViewer},
	)))
	require.True(t, mayWriteSharedMCPMetadata(types.WithCaller(
		context.Background(),
		types.Caller{Role: types.TenantRoleAdmin},
	)))
	require.True(t, mayWriteSharedMCPMetadata(types.WithTenantAPIKeyScope(
		context.Background(),
		types.TenantAPIKeyScope{KeyID: 1, FullAccess: true},
	)))
	require.True(t, mayWriteSharedMCPMetadata(
		context.WithValue(context.Background(), types.SystemAdminContextKey, true),
	))
}

func TestNewMCPMetadataResponse(t *testing.T) {
	// 从未同步：synced=false、tool_count=0，data 为 null。
	unsynced := newMCPMetadataResponse(nil)
	require.True(t, unsynced.Success)
	require.False(t, unsynced.Synced)
	require.Equal(t, 0, unsynced.ToolCount)
	require.Nil(t, unsynced.Data)

	// 已同步但目录为空：synced=true、tool_count=0，与“从未同步”可区分。
	empty := newMCPMetadataResponse(&types.MCPMetadata{Tools: []*types.MCPTool{}})
	require.True(t, empty.Synced)
	require.Equal(t, 0, empty.ToolCount)
	require.NotNil(t, empty.Data)

	// 已同步且有工具：按实际数量计数。
	snapshot := &types.MCPMetadata{Tools: make([]*types.MCPTool, 3)}
	populated := newMCPMetadataResponse(snapshot)
	require.True(t, populated.Synced)
	require.Equal(t, 3, populated.ToolCount)
	require.Same(t, snapshot, populated.Data)
}
