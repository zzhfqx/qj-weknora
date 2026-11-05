// Package mcpserver hosts WeKnora's own MCP server surface: the Streamable
// HTTP endpoint that external MCP clients (Claude Desktop, Cursor, VS Code
// Copilot, ...) connect to. Every request is authenticated by
// middleware.MCPEndpointAuth against a workspace MCPEndpoint row, and the
// endpoint's tool allowlist and knowledge-base scope decide what the client
// can see and call.
//
// A single mcp-go server instance is registered with the full tool catalog;
// per-endpoint visibility is applied through a tool filter (tools/list) and a
// handler middleware (tools/call), both keyed off the endpoint on the request
// context. The transport runs stateless so any replica can answer any call.
package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/ratelimit"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	serverName    = "weknora"
	serverVersion = "1.0.0"

	rateLimitKeyPrefix = "mcp:endpoint:ratelimit:"
	lastUsedTouchEvery = time.Minute
)

var (
	errNoEndpoint = errors.New("mcp endpoint missing from request context")
	// errMCPUserIdentityRequired 表示请求未经过 MCPUserIdentity 写入用户
	// 身份；MCP 服务面上必须 fail-closed。
	errMCPUserIdentityRequired = errors.New("mcp user identity missing from request context")
	// errMCPUserPermissionUnavailable 表示按用户授权服务未接线，属部署/
	// 装配错误，同样按拒绝处理而不是放开。
	errMCPUserPermissionUnavailable = errors.New("mcp user permission service unavailable")
)

// Server wires the tool catalog onto an mcp-go server and exposes it as an
// http.Handler.
type Server struct {
	kbService        interfaces.KnowledgeBaseService
	knowledgeService interfaces.KnowledgeService
	chunkService     interfaces.ChunkService
	wikiService      interfaces.WikiPageService
	sessionService   interfaces.SessionService
	messageService   interfaces.MessageService
	agentService     interfaces.CustomAgentService
	kbShareService   interfaces.KBShareService
	tenantService    interfaces.TenantService
	resourceCatalog  interfaces.ResourceCatalog
	fileService      interfaces.FileService
	storageResolver  interfaces.StorageBackendResolver
	endpointRepo     interfaces.MCPEndpointRepository
	// userPermissionSvc 为 fqx 二开的按用户授权查询（用户 → 可读 kb 列表）。
	userPermissionSvc interfaces.MCPUserPermissionService
	db                *gorm.DB
	cfg               *config.Config

	limiter   *ratelimit.Limiter
	lastTouch sync.Map // endpoint id -> time.Time of the last last_used_at write

	mcp     *server.MCPServer
	handler http.Handler
}

// NewServer builds the MCP server and its Streamable HTTP transport.
func NewServer(
	kbService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	chunkService interfaces.ChunkService,
	wikiService interfaces.WikiPageService,
	sessionService interfaces.SessionService,
	messageService interfaces.MessageService,
	agentService interfaces.CustomAgentService,
	kbShareService interfaces.KBShareService,
	tenantService interfaces.TenantService,
	endpointRepo interfaces.MCPEndpointRepository,
	userPermissionSvc interfaces.MCPUserPermissionService,
	db *gorm.DB,
	cfg *config.Config,
	redisClient *redis.Client,
	resourceCatalog interfaces.ResourceCatalog,
	fileService interfaces.FileService,
	storageResolver interfaces.StorageBackendResolver,
) *Server {
	s := &Server{
		kbService:         kbService,
		knowledgeService:  knowledgeService,
		chunkService:      chunkService,
		wikiService:       wikiService,
		sessionService:    sessionService,
		messageService:    messageService,
		agentService:      agentService,
		kbShareService:    kbShareService,
		tenantService:     tenantService,
		endpointRepo:      endpointRepo,
		userPermissionSvc: userPermissionSvc,
		db:                db,
		cfg:               cfg,
		resourceCatalog:   resourceCatalog,
		fileService:       fileService,
		storageResolver:   storageResolver,
		limiter:           ratelimit.New(redisClient, rateLimitKeyPrefix, time.Minute, ""),
	}
	// The local fallback map only grows without periodic eviction; the server
	// lives for the whole process so the cleanup goroutine never stops.
	go s.limiter.StartCleanup(make(chan struct{}))
	s.mcp = server.NewMCPServer(
		serverName,
		serverVersion,
		server.WithToolCapabilities(false),
		server.WithRecovery(),
		server.WithToolFilter(s.filterTools),
		server.WithToolHandlerMiddleware(s.guardTool),
		server.WithInstructions(serverInstructions),
	)
	s.mcp.AddTools(s.toolCatalog()...)
	s.handler = server.NewStreamableHTTPServer(
		s.mcp,
		server.WithStateLess(true),
	)
	return s
}

// Handler returns the http.Handler to mount under /mcp/:endpoint_id. The
// caller must run MCPEndpointAuth first; the handler trusts the context.
func (s *Server) Handler() http.Handler {
	return s.handler
}

const serverInstructions = "WeKnora knowledge workspace. Start with list_knowledge_bases to see what is in scope, " +
	"then use search_knowledge for semantic questions, grep_chunks for exact keywords, read_document to read " +
	"a whole document, and ask to get a synthesized answer with citations. Wiki tools browse the generated " +
	"wiki when a knowledge base has one. Write tools (add/update/delete_document) exist only on endpoints " +
	"that enabled them."

// endpointFromContext returns the endpoint authenticated for this call.
func endpointFromContext(ctx context.Context) (*types.MCPEndpoint, error) {
	ep, ok := ctx.Value(types.MCPEndpointContextKey).(*types.MCPEndpoint)
	if !ok || ep == nil {
		return nil, errNoEndpoint
	}
	return ep, nil
}

// filterTools hides catalog entries the endpoint did not enable. fqx 二开：
// 在端点白名单之外再按用户身份收窄——身份缺失、授权查询失败或授权知识库
// 列表为空的用户看不到任何知识库工具（fail-closed，对应方案 4.2 二元表）。
func (s *Server) filterTools(ctx context.Context, all []mcp.Tool) []mcp.Tool {
	// 任何隐藏分支都返回非 nil 空切片：序列化为合法的 "tools": []。
	// 返回 nil 会编码成 "tools": null，部分 MCP 客户端（如 OpenCode）
	// 会把它当成 tools/list 失败，导致无权限用户的服务状态报错而非干净的零工具。
	noTools := []mcp.Tool{}
	ep, err := endpointFromContext(ctx)
	if err != nil {
		return noTools
	}
	userKBs, err := s.userKnowledgeBaseIDs(ctx, ep)
	if err != nil {
		logger.Warnf(ctx, "[mcp-audit] tools/list denied: endpoint=%s err=%v", ep.ID, err)
		return noTools
	}
	if len(userKBs) == 0 {
		return noTools
	}
	out := make([]mcp.Tool, 0, len(all))
	for _, t := range all {
		if ep.HasTool(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// guardTool enforces the allowlist and rate limit on every tools/call, so a
// client cannot invoke a hidden tool by name.
// fqx 二开：端点白名单之后、限流之前增加按用户身份的同等校验，隐藏工具
// 按名直调同样被拒；每次调用输出一条结构化审计日志（见 auditToolCall）。
func (s *Server) guardTool(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ep, err := endpointFromContext(ctx)
		if err != nil {
			return mcp.NewToolResultError("unauthorized"), nil
		}
		if !ep.HasTool(req.Params.Name) {
			s.auditToolCall(ctx, ep, req, false, "tool_disabled")
			return mcp.NewToolResultErrorf("tool %q is not enabled on this endpoint", req.Params.Name), nil
		}
		userKBs, err := s.userKnowledgeBaseIDs(ctx, ep)
		if err != nil {
			s.auditToolCall(ctx, ep, req, false, "identity_or_permission_unavailable")
			// 错误文案不泄露授权明细。
			return mcp.NewToolResultError("tool not permitted for this user"), nil
		}
		if len(userKBs) == 0 {
			s.auditToolCall(ctx, ep, req, false, "no_user_permission")
			return mcp.NewToolResultError("tool not permitted for this user"), nil
		}
		if !s.limiter.Allow(ctx, ep.ID, ep.RateLimitPerMinute) {
			s.auditToolCall(ctx, ep, req, false, "rate_limited")
			return mcp.NewToolResultError("rate limit exceeded for this endpoint, retry shortly"), nil
		}
		s.touchLastUsed(ctx, ep.ID)
		s.auditToolCall(ctx, ep, req, true, "")
		result, err := next(ctx, req)
		if err != nil {
			return result, err
		}
		return s.rewriteResourceURLs(ctx, ep, result), nil
	}
}

// auditToolCall 为每次 tools/call 输出一条结构化审计记录，字段固定为
// user_id / endpoint_id / tool / 参数中的知识库与文档 id / 裁决结果 /
// request id，保证 MCP 面的操作可按千机外部用户 id 追溯（方案 4.5）。
// user_id 只可能来自已验签票据写入的 context。
func (s *Server) auditToolCall(
	ctx context.Context, ep *types.MCPEndpoint, req mcp.CallToolRequest, allowed bool, reason string,
) {
	userID, _ := types.MCPUserIDFromContext(ctx)
	fields := logger.Fields{
		"user_id":     userID,
		"endpoint_id": ep.ID,
		"tool":        req.Params.Name,
		"verdict":     "deny",
	}
	if allowed {
		fields["verdict"] = "allow"
	}
	args := req.GetArguments()
	if kbID, _ := args["knowledge_base_id"].(string); strings.TrimSpace(kbID) != "" {
		fields["knowledge_base_id"] = kbID
	}
	if knowledgeID, _ := args["knowledge_id"].(string); strings.TrimSpace(knowledgeID) != "" {
		fields["knowledge_id"] = knowledgeID
	}
	if requestID, ok := types.RequestIDFromContext(ctx); ok {
		fields["request_id"] = requestID
	}
	if allowed {
		logger.GetLogger(ctx).WithFields(fields).Info("[mcp-audit] tool call allowed")
		return
	}
	if reason != "" {
		fields["deny_reason"] = reason
	}
	logger.WarnWithFields(ctx, fields, "[mcp-audit] tool call denied")
}

// touchLastUsed records activity at most once a minute per endpoint.
func (s *Server) touchLastUsed(ctx context.Context, endpointID string) {
	if s.endpointRepo == nil {
		return
	}
	now := time.Now()
	if prev, ok := s.lastTouch.Load(endpointID); ok {
		if last, ok := prev.(time.Time); ok && now.Sub(last) < lastUsedTouchEvery {
			return
		}
	}
	s.lastTouch.Store(endpointID, now)
	go func() {
		bg, cancel := context.WithTimeout(logger.CloneContext(ctx), 5*time.Second)
		defer cancel()
		if err := s.endpointRepo.TouchLastUsed(bg, endpointID); err != nil {
			logger.Warnf(bg, "[mcpserver] touch last_used failed for %s: %v", endpointID, err)
		}
	}()
}

// toolCatalog lists every tool with its handler. Order matches the settings
// catalog so tools/list is stable.
func (s *Server) toolCatalog() []server.ServerTool {
	return []server.ServerTool{
		{Tool: listKnowledgeBasesTool(), Handler: s.handleListKnowledgeBases},
		{Tool: searchKnowledgeTool(), Handler: s.handleSearchKnowledge},
		{Tool: grepChunksTool(), Handler: s.handleGrepChunks},
		{Tool: listDocumentsTool(), Handler: s.handleListDocuments},
		{Tool: readDocumentTool(), Handler: s.handleReadDocument},
		{Tool: askTool(), Handler: s.handleAsk},
		{Tool: wikiSearchTool(), Handler: s.handleWikiSearch},
		{Tool: wikiReadPageTool(), Handler: s.handleWikiReadPage},
		{Tool: wikiIndexTool(), Handler: s.handleWikiIndex},
		{Tool: addDocumentTool(), Handler: s.handleAddDocument},
		{Tool: updateDocumentTool(), Handler: s.handleUpdateDocument},
		{Tool: deleteDocumentTool(), Handler: s.handleDeleteDocument},
	}
}

// toolResultFromAgentTool converts an agent tool result into an MCP result.
// Agent tools already format their Output for a model reader, so the text
// is passed through; Data is attached as structured content when present.
func toolResultFromAgentTool(res *types.ToolResult, err error) *mcp.CallToolResult {
	if err != nil && (res == nil || res.Error == "") {
		return mcp.NewToolResultError(err.Error())
	}
	if res == nil {
		return mcp.NewToolResultError("tool returned no result")
	}
	if !res.Success {
		msg := res.Error
		if msg == "" {
			msg = res.Output
		}
		if msg == "" {
			msg = "tool failed"
		}
		return mcp.NewToolResultError(msg)
	}
	if len(res.Data) > 0 {
		return mcp.NewToolResultStructured(res.Data, res.Output)
	}
	return mcp.NewToolResultText(res.Output)
}
