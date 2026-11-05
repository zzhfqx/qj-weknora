package types

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// MCPUserPermission 是 fqx 二开为内置 MCP 服务面新增的"按用户授权"表：
// 每个工作空间的每个外部用户有且仅有一行。KnowledgeBaseIDs 列出该用户经
// MCP 端点可读的知识库 id；语义为"在列表内=有权，不在=无权"，用户无行或
// 列表为空时看不到任何 MCP 知识库工具。
//
// user_id 为外部（千机）调用方标识，weknora 不为其建立影子用户，因此本表
// 与官方用户/租户体系无写依赖，跟随上游升级时不会产生结构冲突。该表由
// 千机侧建库/共享授权的业务流程写入，weknora 的 MCP 层只读。
type MCPUserPermission struct {
	ID       string `json:"id" gorm:"type:varchar(36);primaryKey"`
	TenantID uint64 `json:"tenant_id" gorm:"not null;uniqueIndex:idx_mcp_user_permission_user,priority:1"`
	UserID   string `json:"user_id" gorm:"type:varchar(128);not null;default:'';uniqueIndex:idx_mcp_user_permission_user,priority:2"`
	// KnowledgeBaseIDs 为该用户可读知识库 id 的 JSON 数组，
	// 与 mcp_endpoints.knowledge_base_ids 同款 StringArray 映射。
	KnowledgeBaseIDs StringArray `json:"knowledge_base_ids" gorm:"type:jsonb;not null;default:'[]'"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

// TableName 返回存储表名。
func (MCPUserPermission) TableName() string { return "mcp_user_permission" }

// BeforeCreate 在插入前补 id 与默认值。
func (p *MCPUserPermission) BeforeCreate(_ *gorm.DB) error {
	if p.ID == "" {
		p.ID = uuid.New().String()
	}
	if p.KnowledgeBaseIDs == nil {
		p.KnowledgeBaseIDs = StringArray{}
	}
	return nil
}

// MCPUserIDFromContext 取出 middleware.MCPUserIdentity 写入的已验签外部
// 用户 id。第二个返回值为 false 表示请求未携带已认证用户；MCP 服务面上的
// 调用方必须按"拒绝"处理（fail-closed），绝不能退化为匿名/机器用户全开。
func MCPUserIDFromContext(ctx context.Context) (string, bool) {
	userID, _ := ctx.Value(MCPUserIDContextKey).(string)
	userID = strings.TrimSpace(userID)
	return userID, userID != ""
}
