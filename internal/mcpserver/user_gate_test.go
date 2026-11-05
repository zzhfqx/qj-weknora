package mcpserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/ratelimit"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

// fakeUserPermissionSvc 是按用户授权查询的可控实现。
type fakeUserPermissionSvc struct {
	ids []string
	err error
}

func (f *fakeUserPermissionSvc) GetUserPermission(
	_ context.Context, _ uint64, _ string,
) ([]string, error) {
	return f.ids, f.err
}

var _ interfaces.MCPUserPermissionService = (*fakeUserPermissionSvc)(nil)

// fakeKBService 只覆盖测试用到的 ListKnowledgeBases，其余方法靠内嵌
// nil 接口兜底（测试路径不会调用）。
type fakeKBService struct {
	interfaces.KnowledgeBaseService
	kbs []*types.KnowledgeBase
}

func (f *fakeKBService) ListKnowledgeBases(_ context.Context) ([]*types.KnowledgeBase, error) {
	return f.kbs, nil
}

func newGateTestServer(svc interfaces.MCPUserPermissionService) *Server {
	return &Server{
		userPermissionSvc: svc,
		limiter:           ratelimit.New(nil, "mcp-user-gate-test:", time.Minute, "test"),
	}
}

func mcpCtx(ep *types.MCPEndpoint, userID string) context.Context {
	ctx := context.Background()
	if ep != nil {
		ctx = context.WithValue(ctx, types.MCPEndpointContextKey, ep)
	}
	if userID != "" {
		ctx = context.WithValue(ctx, types.MCPUserIDContextKey, userID)
	}
	return ctx
}

func defaultEndpoint() *types.MCPEndpoint {
	return &types.MCPEndpoint{
		ID:                 "ep1",
		TenantID:           7,
		Tools:              types.StringArray(types.DefaultMCPEndpointTools()),
		KnowledgeBaseIDs:   nil,
		RateLimitPerMinute: 60,
	}
}

func catalogTools() []mcp.Tool {
	names := types.DefaultMCPEndpointTools()
	out := make([]mcp.Tool, 0, len(names)+3)
	for _, n := range names {
		out = append(out, mcp.NewTool(n))
	}
	out = append(out, mcp.NewTool(types.MCPEndpointToolAddDocument))
	return out
}

// ---- filterTools：tools/list 可见性二元裁决 ----

func TestFilterToolsHidesAllWithoutUserOrPermission(t *testing.T) {
	ep := defaultEndpoint()
	all := catalogTools()
	srv := newGateTestServer(&fakeUserPermissionSvc{})

	// 无端点上下文：全部隐藏。
	got := srv.filterTools(context.Background(), all)
	require.Empty(t, got)
	require.NotNil(t, got) // 必须是 [] 而非 null，避免客户端把 tools/list 当失败
	// 有端点但无用户身份：全部隐藏（fail-closed）。
	got = srv.filterTools(mcpCtx(ep, ""), all)
	require.Empty(t, got)
	require.NotNil(t, got)
	// 有身份但授权列表为空：全部隐藏。
	got = srv.filterTools(mcpCtx(ep, "u-empty"), all)
	require.Empty(t, got)
	require.NotNil(t, got)
	// 授权服务报错：全部隐藏（fail-closed，不放行）。
	srvErr := newGateTestServer(&fakeUserPermissionSvc{err: errors.New("db down")})
	got = srvErr.filterTools(mcpCtx(ep, "u1"), all)
	require.Empty(t, got)
	require.NotNil(t, got)
}

func TestFilterToolsShowsEndpointToolsForAuthorizedUser(t *testing.T) {
	ep := defaultEndpoint()
	srv := newGateTestServer(&fakeUserPermissionSvc{ids: []string{"kb-a"}})

	got := srv.filterTools(mcpCtx(ep, "u1"), catalogTools())
	require.Len(t, got, len(types.DefaultMCPEndpointTools()))
	for _, tool := range got {
		require.Contains(t, types.DefaultMCPEndpointTools(), tool.Name)
	}
}

// ---- guardTool：按名直调隐藏/越权工具一律拒绝，授权用户放行 ----

func callGuard(t *testing.T, srv *Server, ctx context.Context, name string, next server.ToolHandlerFunc) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = map[string]any{"knowledge_base_id": "kb-a"}
	wrapped := srv.guardTool(next)
	res, err := wrapped(ctx, req)
	require.NoError(t, err)
	return res
}

func TestGuardToolDeniesHiddenToolByName(t *testing.T) {
	ep := defaultEndpoint()
	srv := newGateTestServer(&fakeUserPermissionSvc{ids: []string{"kb-a"}})

	called := false
	next := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("ok"), nil
	}
	// add_document 不在端点白名单：即使该用户有读权限也拒绝，且不进 handler。
	res := callGuard(t, srv, mcpCtx(ep, "u1"), types.MCPEndpointToolAddDocument, next)
	require.True(t, res.IsError)
	require.False(t, called)
}

func TestGuardToolDeniesUserWithoutPermission(t *testing.T) {
	ep := defaultEndpoint()

	// 无身份（中间件被绕过时的纵深防御）。
	srv := newGateTestServer(&fakeUserPermissionSvc{ids: []string{"kb-a"}})
	called := false
	next := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		called = true
		return mcp.NewToolResultText("ok"), nil
	}
	res := callGuard(t, srv, mcpCtx(ep, ""), types.MCPEndpointToolAsk, next)
	require.True(t, res.IsError)
	require.False(t, called)

	// 有身份但授权列表为空。
	srvEmpty := newGateTestServer(&fakeUserPermissionSvc{})
	res = callGuard(t, srvEmpty, mcpCtx(ep, "u-empty"), types.MCPEndpointToolAsk, next)
	require.True(t, res.IsError)
	require.False(t, called)

	// 授权服务故障：fail-closed。
	srvErr := newGateTestServer(&fakeUserPermissionSvc{err: errors.New("db down")})
	res = callGuard(t, srvErr, mcpCtx(ep, "u1"), types.MCPEndpointToolAsk, next)
	require.True(t, res.IsError)
	require.False(t, called)
}

func TestGuardToolAllowsAuthorizedUser(t *testing.T) {
	ep := defaultEndpoint()
	srv := newGateTestServer(&fakeUserPermissionSvc{ids: []string{"kb-a"}})

	next := func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}
	res := callGuard(t, srv, mcpCtx(ep, "u1"), types.MCPEndpointToolAsk, next)
	require.False(t, res.IsError)
}

// ---- allowedKnowledgeBases：端点集 ∩ 用户授权集 ----

func TestAllowedKnowledgeBasesIntersectsForUnrestrictedEndpoint(t *testing.T) {
	ep := defaultEndpoint() // KnowledgeBaseIDs 为空 = 端点不限定库
	srv := newGateTestServer(&fakeUserPermissionSvc{ids: []string{"kb-a", "kb-c"}})
	srv.kbService = &fakeKBService{kbs: []*types.KnowledgeBase{
		{ID: "kb-a", TenantID: 7},
		{ID: "kb-b", TenantID: 7},
		{ID: "kb-c", TenantID: 7},
	}}

	got, err := srv.allowedKnowledgeBases(mcpCtx(ep, "u1"), ep)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"kb-a", "kb-c"}, knowledgeBaseIDs(got))
}

func TestAllowedKnowledgeBasesEmptyForUserWithNoGrants(t *testing.T) {
	ep := defaultEndpoint()
	srv := newGateTestServer(&fakeUserPermissionSvc{ids: nil})
	srv.kbService = &fakeKBService{kbs: []*types.KnowledgeBase{
		{ID: "kb-a", TenantID: 7},
		{ID: "kb-b", TenantID: 7},
	}}

	got, err := srv.allowedKnowledgeBases(mcpCtx(ep, "u-empty"), ep)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestIntersectKnowledgeBasesHelper(t *testing.T) {
	kbs := []*types.KnowledgeBase{{ID: "a"}, nil, {ID: "b"}, {ID: "c"}}
	set := map[string]struct{}{"b": {}, "c": {}}
	got := intersectKnowledgeBases(kbs, set)
	require.Equal(t, []string{"b", "c"}, knowledgeBaseIDs(got))
	require.Nil(t, intersectKnowledgeBases(kbs, nil))
}
