package repository

import (
	"context"
	"errors"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

type mcpUserPermissionRepository struct {
	db *gorm.DB
}

// NewMCPUserPermissionRepository 创建 GORM 实现的按用户授权仓储。
func NewMCPUserPermissionRepository(db *gorm.DB) interfaces.MCPUserPermissionRepository {
	return &mcpUserPermissionRepository{db: db}
}

// GetByUser 按 (tenant_id, user_id) 唯一键取授权行；无行返回 (nil, nil)。
func (r *mcpUserPermissionRepository) GetByUser(
	ctx context.Context, tenantID uint64, userID string,
) (*types.MCPUserPermission, error) {
	var row types.MCPUserPermission
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ?", tenantID, userID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}
