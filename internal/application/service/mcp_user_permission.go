package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	// MCPUserPermissionCacheTTL 为授权列表进程内缓存有效期，落在方案建议
	// 的 30–60s 区间：千机侧建库/共享/收回权限最迟 45s 后在 MCP 面生效。
	MCPUserPermissionCacheTTL = 45 * time.Second
)

// mcpUserPermissionService 以"用户 → 可读知识库 id 列表"单向查询服务裁决
// 热路径。进程内短 TTL 缓存让落在同一窗口内的并发请求只查一次库；
// 空结果不缓存（无负缓存）。
type mcpUserPermissionService struct {
	repo interfaces.MCPUserPermissionRepository

	mu    sync.Mutex
	cache map[mcpUserPermissionCacheKey]mcpUserPermissionCacheEntry
}

// mcpUserPermissionCacheKey 为 (工作空间, 外部用户) 复合键。
type mcpUserPermissionCacheKey struct {
	tenantID uint64
	userID   string
}

// mcpUserPermissionCacheEntry 存归一化后的列表快照与过期时刻。
type mcpUserPermissionCacheEntry struct {
	ids []string
	exp time.Time
}

// NewMCPUserPermissionService 创建按用户授权查询服务，并启动缓存过期项清理
// 协程（进程生命周期内常驻，与 mcpserver limiter 的处理方式一致）。
func NewMCPUserPermissionService(repo interfaces.MCPUserPermissionRepository) interfaces.MCPUserPermissionService {
	s := &mcpUserPermissionService{
		repo:  repo,
		cache: make(map[mcpUserPermissionCacheKey]mcpUserPermissionCacheEntry),
	}
	go s.sweep(make(chan struct{}))
	return s
}

// GetUserPermission 返回用户可读知识库 id 列表（已去空白、去重）。
// 查不到行或列表为空 → 返回空切片、且不写缓存（无负缓存）；
// DB 错误向上返回，由调用方按 fail-closed 处理。
func (s *mcpUserPermissionService) GetUserPermission(
	ctx context.Context, tenantID uint64, userID string,
) ([]string, error) {
	userID = strings.TrimSpace(userID)
	key := mcpUserPermissionCacheKey{tenantID: tenantID, userID: userID}

	// 快路径：命中且未过期。返回副本，防止调用方修改污染缓存。
	s.mu.Lock()
	if entry, ok := s.cache[key]; ok && time.Now().Before(entry.exp) {
		s.mu.Unlock()
		return append([]string(nil), entry.ids...), nil
	}
	s.mu.Unlock()

	row, err := s.repo.GetByUser(ctx, tenantID, userID)
	if err != nil {
		return nil, err
	}
	var ids []string
	if row != nil {
		ids = normalizeMCPUserKBIDs(row.KnowledgeBaseIDs)
	}
	if len(ids) > 0 {
		s.mu.Lock()
		s.cache[key] = mcpUserPermissionCacheEntry{
			ids: ids,
			exp: time.Now().Add(MCPUserPermissionCacheTTL),
		}
		s.mu.Unlock()
	} else {
		// 空结果不缓存；同时清掉可能存在的旧授权快照：撤销授权/删行后
		// 本次读取即让旧条目立即失效，而不必等 TTL 到期。
		s.mu.Lock()
		delete(s.cache, key)
		s.mu.Unlock()
	}
	return append([]string(nil), ids...), nil
}

// normalizeMCPUserKBIDs 去掉空白项与重复 id，保持首次出现顺序。
func normalizeMCPUserKBIDs(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// sweep 定时清理过期缓存项，避免外部用户无限增长撑爆 map。
// stop 关闭后协程退出（当前进程生命周期内不会关闭，与其他后台协程对齐）。
func (s *mcpUserPermissionService) sweep(stop <-chan struct{}) {
	ticker := time.NewTicker(MCPUserPermissionCacheTTL)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			for k, v := range s.cache {
				if !now.Before(v.exp) {
					delete(s.cache, k)
				}
			}
			s.mu.Unlock()
		}
	}
}
