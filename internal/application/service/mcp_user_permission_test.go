package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// 单测用内存仓储：可控返回值并统计查询次数，验证缓存与"无负缓存"。
type fakeUserPermissionRepo struct {
	row        *types.MCPUserPermission
	err        error
	callCount  int
	lastUserID string
}

func (r *fakeUserPermissionRepo) GetByUser(
	_ context.Context, _ uint64, userID string,
) (*types.MCPUserPermission, error) {
	r.callCount++
	r.lastUserID = userID
	return r.row, r.err
}

var _ interfaces.MCPUserPermissionRepository = (*fakeUserPermissionRepo)(nil)

// 非空结果在 TTL 内只查一次库，且返回的是副本（调用方修改不污染缓存）。
func TestMCPUserPermissionCachesNonEmpty(t *testing.T) {
	repo := &fakeUserPermissionRepo{row: &types.MCPUserPermission{
		KnowledgeBaseIDs: types.StringArray{"kb-a", "kb-b"},
	}}
	svc := NewMCPUserPermissionService(repo)
	ctx := context.Background()

	first, err := svc.GetUserPermission(ctx, 7, "u1")
	require.NoError(t, err)
	require.Equal(t, []string{"kb-a", "kb-b"}, first)

	first[0] = "mutated"
	second, err := svc.GetUserPermission(ctx, 7, "u1")
	require.NoError(t, err)
	require.Equal(t, []string{"kb-a", "kb-b"}, second)
	require.Equal(t, 1, repo.callCount)
}

// 空结果不做负缓存：无行用户每次都穿透查库，保证"刚建库立刻首次提问"
// 不会在 TTL 内被错误拒绝。
func TestMCPUserPermissionNoNegativeCache(t *testing.T) {
	repo := &fakeUserPermissionRepo{row: nil}
	svc := NewMCPUserPermissionService(repo)
	ctx := context.Background()

	ids, err := svc.GetUserPermission(ctx, 7, "new-user")
	require.NoError(t, err)
	require.Empty(t, ids)
	ids, err = svc.GetUserPermission(ctx, 7, "new-user")
	require.NoError(t, err)
	require.Empty(t, ids)
	require.Equal(t, 2, repo.callCount)
}

// 列表中的空白项与重复 id 被归一化掉。
func TestMCPUserPermissionNormalizes(t *testing.T) {
	repo := &fakeUserPermissionRepo{row: &types.MCPUserPermission{
		KnowledgeBaseIDs: types.StringArray{" kb-a ", "kb-b", "", "kb-a", "kb-c"},
	}}
	svc := NewMCPUserPermissionService(repo)

	ids, err := svc.GetUserPermission(context.Background(), 1, "u1")
	require.NoError(t, err)
	require.Equal(t, []string{"kb-a", "kb-b", "kb-c"}, ids)
}

// DB 错误向上传播（调用方据此 fail-closed），且不得被缓存。
func TestMCPUserPermissionDBError(t *testing.T) {
	repo := &fakeUserPermissionRepo{err: errors.New("db down")}
	svc := NewMCPUserPermissionService(repo)

	_, err := svc.GetUserPermission(context.Background(), 1, "u1")
	require.Error(t, err)
	_, err = svc.GetUserPermission(context.Background(), 1, "u1")
	require.Error(t, err)
	require.Equal(t, 2, repo.callCount)
}

// 按用户隔离的内存仓储：不同用户返回各自的 kb 列表。
type perUserFakeRepo struct {
	mu   sync.Mutex
	rows map[string]*types.MCPUserPermission
}

func (r *perUserFakeRepo) GetByUser(
	_ context.Context, _ uint64, userID string,
) (*types.MCPUserPermission, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rows[userID], nil
}

var _ interfaces.MCPUserPermissionRepository = (*perUserFakeRepo)(nil)

// 并发：50 个不同 user_id 并发查询，各自只拿到自己的 kb id，零串扰
// （方案 6.1 并发用例；无任何进程级"当前用户"状态可串）。
func TestMCPUserPermissionConcurrentUsersIsolated(t *testing.T) {
	const n = 50
	repo := &perUserFakeRepo{rows: make(map[string]*types.MCPUserPermission, n)}
	for i := 0; i < n; i++ {
		userID := fmt.Sprintf("user-%02d", i)
		repo.rows[userID] = &types.MCPUserPermission{
			KnowledgeBaseIDs: types.StringArray{fmt.Sprintf("kb-%02d", i)},
		}
	}
	svc := NewMCPUserPermissionService(repo)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			userID := fmt.Sprintf("user-%02d", i)
			for j := 0; j < 10; j++ {
				ids, err := svc.GetUserPermission(context.Background(), 7, userID)
				if err != nil {
					errs <- err
					return
				}
				if len(ids) != 1 || ids[0] != fmt.Sprintf("kb-%02d", i) {
					errs <- fmt.Errorf("user %s got %v (cross-talk)", userID, ids)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
