package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/application/access"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/mark3labs/mcp-go/mcp"
)

// optionalKnowledgeBaseSelectors 解析调用方显式传入的 knowledge_base_ids
// 参数，并对畸形输入 fail-closed：参数未传时返回 nil（后续走端点默认范围，
// 含本分支的“端点集 ∩ 用户授权集”交集）；一旦显式传了，类型不对或含空白
// 元素都直接报错，避免旧写法 GetStringSlice 静默吞掉空名单而把请求放大成
// 全量范围。该校验只看参数格式，发生在用户授权交集之前，与二开正交。
func optionalKnowledgeBaseSelectors(req mcp.CallToolRequest) ([]string, error) {
	if _, supplied := req.GetArguments()["knowledge_base_ids"]; !supplied {
		return nil, nil
	}
	selectors, err := req.RequireStringSlice("knowledge_base_ids")
	if err != nil {
		return nil, err
	}
	for _, selector := range selectors {
		if strings.TrimSpace(selector) == "" {
			return nil, fmt.Errorf("knowledge_base_ids must not contain empty selectors")
		}
	}
	return selectors, nil
}

// allowedKnowledgeBases returns every knowledge base the endpoint may touch,
// each one re-authorized for the calling principal through access.ResolveKB
// so a stale or foreign ID in the allowlist is dropped rather than trusted.
// An unrestricted endpoint sees the workspace's own knowledge bases.
//
// fqx 二开（方案 4.3）：在端点允许集之上再与 mcp_user_permission 中该用户
// 的授权 id 列表取交集。端点未显式限定知识库时同样交集（此时端点集=
// 工作空间自有+共享给工作空间的全部库）。用户列表中的失效/跨工作空间 id
// 会被下方既有的逐个重授权循环 warn-and-continue 静默丢弃，无需同步清理。
func (s *Server) allowedKnowledgeBases(ctx context.Context, ep *types.MCPEndpoint) ([]*types.KnowledgeBase, error) {
	userKBs, err := s.userKnowledgeBaseIDs(ctx, ep)
	if err != nil {
		return nil, err
	}
	userSet := make(map[string]struct{}, len(userKBs))
	for _, id := range userKBs {
		userSet[strings.TrimSpace(id)] = struct{}{}
	}

	if !ep.RestrictsKnowledgeBases() {
		all, err := s.kbService.ListKnowledgeBases(ctx)
		if err != nil {
			return nil, err
		}
		return intersectKnowledgeBases(all, userSet), nil
	}

	// 端点限定模式：只对"端点集 ∩ 用户集"中的 id 逐个重授权；
	// 不在用户列表里的端点 id 直接跳过（不算异常，不打 warn）。
	out := make([]*types.KnowledgeBase, 0, len(ep.KnowledgeBaseIDs))
	for _, id := range ep.KnowledgeBaseIDs {
		id = strings.TrimSpace(id)
		if _, ok := userSet[id]; !ok {
			continue
		}
		kb, err := s.authorizedKnowledgeBase(ctx, id, types.OrgRoleViewer)
		if err != nil {
			logger.Warnf(ctx, "[mcpserver] endpoint %s references unavailable knowledge base %s: %v",
				ep.ID, id, err)
			continue
		}
		out = append(out, kb)
	}
	return out, nil
}

// intersectKnowledgeBases 保留 id 落在用户授权集合内的知识库（顺序以端点
// 列表为准）。
func intersectKnowledgeBases(kbs []*types.KnowledgeBase, userSet map[string]struct{}) []*types.KnowledgeBase {
	if len(userSet) == 0 {
		return nil
	}
	out := make([]*types.KnowledgeBase, 0, len(kbs))
	for _, kb := range kbs {
		if kb == nil {
			continue
		}
		if _, ok := userSet[kb.ID]; ok {
			out = append(out, kb)
		}
	}
	return out
}

// userKnowledgeBaseIDs 返回当前请求已验签外部用户被授权可读的知识库 id
// 列表。user_id 只能取自 MCPUserIdentity 写入 ctx 的票据结果；取不到身份
// 或授权服务不可用均返回错误，由调用方 fail-closed。
func (s *Server) userKnowledgeBaseIDs(ctx context.Context, ep *types.MCPEndpoint) ([]string, error) {
	userID, ok := types.MCPUserIDFromContext(ctx)
	if !ok {
		return nil, errMCPUserIdentityRequired
	}
	if s.userPermissionSvc == nil {
		return nil, errMCPUserPermissionUnavailable
	}
	ids, err := s.userPermissionSvc.GetUserPermission(ctx, ep.TenantID, userID)
	if err != nil {
		logger.Warnf(ctx, "[mcpserver] load user permission failed: endpoint=%s user=%s err=%v",
			ep.ID, userID, err)
		return nil, err
	}
	return ids, nil
}

// authorizedKnowledgeBase loads a knowledge base by ID and resolves the
// caller's permission on it (ownership or organization share). The returned
// error is access.ErrForbidden / ErrNotFound for authorization failures.
func (s *Server) authorizedKnowledgeBase(
	ctx context.Context, kbID string, required types.OrgMemberRole,
) (*types.KnowledgeBase, error) {
	kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, strings.TrimSpace(kbID))
	if err != nil || kb == nil {
		return nil, access.ErrNotFound
	}
	if _, err := s.resolveKB(ctx, kb, required); err != nil {
		return nil, err
	}
	return kb, nil
}

func (s *Server) resolveKB(
	ctx context.Context, kb *types.KnowledgeBase, required types.OrgMemberRole,
) (*access.KBAccess, error) {
	request := access.KBRequest{Caller: types.CallerFromContext(ctx)}
	return access.ResolveKB(ctx, request, kb, required, s.kbShareService, nil)
}

// scopedKBContext authorizes one knowledge base at the required permission
// and returns the context every document-level call must run under: it
// carries the resolved grant (so access.RequireKBWrite sees an explicit
// authorization instead of an implicit "same tenant") and switches the
// execution tenant to the knowledge base owner, which is what makes shared
// knowledge bases work. Their documents are stored under the owner tenant,
// so listing, reading and writing them through the caller's tenant would
// either find nothing or create rows under the wrong tenant.
//
// TenantInfo is swapped to the owner as well so model and storage settings
// resolve against the workspace that holds the data, mirroring the service
// layer's withKBWriteTenantInfo.
func (s *Server) scopedKBContext(
	ctx context.Context, kb *types.KnowledgeBase, required types.OrgMemberRole,
) (context.Context, error) {
	_, scoped, err := s.authorizeKB(ctx, kb, required)
	if err != nil {
		if errors.Is(err, access.ErrForbidden) || errors.Is(err, access.ErrUnauthorized) {
			if required == types.OrgRoleViewer {
				return ctx, fmt.Errorf("this endpoint is not allowed to read knowledge base %q", kb.ID)
			}
			return ctx, fmt.Errorf("this endpoint is not allowed to write to knowledge base %q", kb.ID)
		}
		return ctx, err
	}
	return scoped, nil
}

// authorizeKB resolves the caller's grant on kb and returns it together with
// the owner-scoped context described on scopedKBContext.
func (s *Server) authorizeKB(
	ctx context.Context, kb *types.KnowledgeBase, required types.OrgMemberRole,
) (*access.KBAccess, context.Context, error) {
	grant, err := s.resolveKB(ctx, kb, required)
	if err != nil {
		return nil, ctx, err
	}
	scoped := grant.Context(ctx)
	caller := types.CallerFromContext(ctx)
	if kb.TenantID != caller.TenantID && s.tenantService != nil {
		owner, err := s.tenantService.GetTenantByID(ctx, kb.TenantID)
		if err != nil || owner == nil {
			return nil, ctx, fmt.Errorf("the workspace owning knowledge base %q is unavailable", kb.ID)
		}
		scoped = context.WithValue(scoped, types.TenantInfoContextKey, owner)
	}
	return grant, scoped, nil
}

// selectKnowledgeBases narrows the allowed set to the ones a caller named,
// accepting either IDs or exact (case-insensitive) names. With no selector
// every allowed knowledge base is returned.
func (s *Server) selectKnowledgeBases(
	ctx context.Context, ep *types.MCPEndpoint, requested []string,
) ([]*types.KnowledgeBase, error) {
	allowed, err := s.allowedKnowledgeBases(ctx, ep)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("no knowledge base is available on this endpoint")
	}
	cleaned := make([]string, 0, len(requested))
	for _, r := range requested {
		if r = strings.TrimSpace(r); r != "" {
			cleaned = append(cleaned, r)
		}
	}
	if len(cleaned) == 0 {
		return allowed, nil
	}
	out := make([]*types.KnowledgeBase, 0, len(cleaned))
	seen := map[string]struct{}{}
	for _, sel := range cleaned {
		kb := matchKnowledgeBase(allowed, sel)
		if kb == nil {
			return nil, fmt.Errorf("knowledge base %q was not found or is outside this endpoint's scope; "+
				"call list_knowledge_bases to see what is available", sel)
		}
		if _, dup := seen[kb.ID]; dup {
			continue
		}
		seen[kb.ID] = struct{}{}
		out = append(out, kb)
	}
	return out, nil
}

func matchKnowledgeBase(kbs []*types.KnowledgeBase, selector string) *types.KnowledgeBase {
	for _, kb := range kbs {
		if kb.ID == selector {
			return kb
		}
	}
	for _, kb := range kbs {
		if strings.EqualFold(strings.TrimSpace(kb.Name), selector) {
			return kb
		}
	}
	return nil
}

// searchTargetsFor builds whole-knowledge-base search targets. TenantID is
// carried per target because shared knowledge bases index under their owner.
func searchTargetsFor(kbs []*types.KnowledgeBase) types.SearchTargets {
	targets := make(types.SearchTargets, 0, len(kbs))
	for _, kb := range kbs {
		targets = append(targets, &types.SearchTarget{
			Type:            types.SearchTargetTypeKnowledgeBase,
			KnowledgeBaseID: kb.ID,
			TenantID:        kb.TenantID,
		})
	}
	return targets
}

func knowledgeBaseIDs(kbs []*types.KnowledgeBase) []string {
	ids := make([]string, 0, len(kbs))
	for _, kb := range kbs {
		ids = append(ids, kb.ID)
	}
	return ids
}

// retrievableKnowledgeBases keeps knowledge bases that have a vector or
// keyword index, i.e. the ones the search tools can actually query.
func retrievableKnowledgeBases(kbs []*types.KnowledgeBase) []*types.KnowledgeBase {
	out := make([]*types.KnowledgeBase, 0, len(kbs))
	for _, kb := range kbs {
		if kb.IsVectorEnabled() || kb.IsKeywordEnabled() {
			out = append(out, kb)
		}
	}
	return out
}

// wikiKnowledgeBases keeps knowledge bases with a generated wiki.
func wikiKnowledgeBases(kbs []*types.KnowledgeBase) []*types.KnowledgeBase {
	out := make([]*types.KnowledgeBase, 0, len(kbs))
	for _, kb := range kbs {
		if kb.IsWikiEnabled() {
			out = append(out, kb)
		}
	}
	return out
}

func wikiScopesFor(kbs []*types.KnowledgeBase) []tools.WikiScope {
	return tools.NewWikiScopesFromKBIDs(knowledgeBaseIDs(kbs))
}

// knowledgeInScope loads a document and confirms it belongs to a knowledge
// base the endpoint may touch. The lookup is tenant-agnostic because a shared
// knowledge base's documents live under the owner tenant; the document must
// then match an allowed knowledge base and that knowledge base's tenant.
func (s *Server) knowledgeInScope(
	ctx context.Context, ep *types.MCPEndpoint, knowledgeID string,
) (*types.Knowledge, *types.KnowledgeBase, error) {
	allowed, err := s.allowedKnowledgeBases(ctx, ep)
	if err != nil {
		return nil, nil, err
	}
	return s.knowledgeInKnowledgeBases(ctx, allowed, knowledgeID)
}

// knowledgeInKnowledgeBases applies the same document/owner checks to either
// the full endpoint scope or a narrower, already authorized KB selection.
// 文档按属主租户跨租户存放（共享库场景），所以除了知识库必须在已授权名单内，
// 还要求文档租户与该知识库租户一致，防止用伪造属主的文档 id 试探越权。
func (s *Server) knowledgeInKnowledgeBases(
	ctx context.Context, kbs []*types.KnowledgeBase, knowledgeID string,
) (*types.Knowledge, *types.KnowledgeBase, error) {
	knowledgeID = strings.TrimSpace(knowledgeID)
	if knowledgeID == "" {
		return nil, nil, fmt.Errorf("knowledge_id is required")
	}
	k, err := s.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
	if err != nil || k == nil {
		return nil, nil, fmt.Errorf("document %q was not found", knowledgeID)
	}
	kb := matchKnowledgeBase(kbs, k.KnowledgeBaseID)
	if kb == nil || kb.TenantID != k.TenantID {
		return nil, nil, fmt.Errorf("document %q is outside this endpoint's scope or the selected knowledge bases",
			knowledgeID)
	}
	return k, kb, nil
}

// maxSearchKnowledgeDocuments bounds validation lookups and backend filter size.
const maxSearchKnowledgeDocuments = 100

// documentSearchTargets narrows an already authorized KB selection to explicit
// documents. Never leave whole-KB targets for bases with no selected documents.
// 把 knowledge_ids 按所属知识库聚合成文档级检索目标：
//   - 先经 knowledgeInKnowledgeBases 逐个验明文档存在、且在已授权+已选知识库内；
//   - 同一知识库的多个文档合并成一个 target（去重），范围在检索前收窄；
//   - DisableRecallThresholds=true：范围已被显式圈定，放宽相关度阈值，
//     避免"指定了文档却因为阈值差一点召回不到"。
func (s *Server) documentSearchTargets(
	ctx context.Context, kbs []*types.KnowledgeBase, knowledgeIDs []string,
) (types.SearchTargets, error) {
	if len(knowledgeIDs) == 0 {
		return nil, fmt.Errorf("knowledge_ids must contain at least one document id; " +
			"omit it to search whole knowledge bases")
	}
	if len(knowledgeIDs) > maxSearchKnowledgeDocuments {
		return nil, fmt.Errorf("knowledge_ids must contain at most %d document ids", maxSearchKnowledgeDocuments)
	}
	targets := make(types.SearchTargets, 0)
	byKB := make(map[string]*types.SearchTarget)
	seen := make(map[string]bool, len(knowledgeIDs))
	for _, id := range knowledgeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("knowledge_ids must not contain blank document ids")
		}
		if seen[id] {
			continue
		}
		k, kb, err := s.knowledgeInKnowledgeBases(ctx, kbs, id)
		if err != nil {
			return nil, err
		}
		target := byKB[kb.ID]
		if target == nil {
			target = &types.SearchTarget{
				Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: kb.ID, TenantID: kb.TenantID,
				DisableRecallThresholds: true,
			}
			byKB[kb.ID] = target
			targets = append(targets, target)
		}
		target.KnowledgeIDs = append(target.KnowledgeIDs, k.ID)
		seen[id] = true
	}
	return targets, nil
}
