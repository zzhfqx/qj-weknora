package interfaces

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// MCPUserPermissionRepository 持久化 MCP 按用户授权表（一用户一行）。
// 该表由千机侧建库/共享流程写入，weknora 侧只读。
type MCPUserPermissionRepository interface {
	// GetByUser 取指定工作空间下某外部用户的授权行；
	// 无行时返回 (nil, nil)，与 gorm.ErrRecordNotFound 约定一致。
	GetByUser(ctx context.Context, tenantID uint64, userID string) (*types.MCPUserPermission, error)
}

// MCPUserPermissionService 是 MCP 裁决热路径读取用户授权知识库列表的入口。
type MCPUserPermissionService interface {
	// GetUserPermission 返回该用户被授权可读的知识库 id 列表。
	// 用户无行或列表为空时返回空切片（语义=看不到任何知识库工具）。
	// 结果带 30–60s 短 TTL 进程内缓存，但空结果不做负缓存
	// （"刚建库立刻首次提问"是正常路径）。
	GetUserPermission(ctx context.Context, tenantID uint64, userID string) ([]string, error)
}
