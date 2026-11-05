package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const (
	wikiFollowUpPrefix = "wiki:followup:"
	// wikiFollowUpReconcileAfter 是对账间隔，不是队列租约。一个任务可以
	// 任意长时间等待 worker 而不会失去预留容量。丢失的任务会在这个间隔
	// 内保留，以允许正在进行的入队操作完成（包括调用方超时的情况）。
	wikiFollowUpReconcileAfter = 2 * time.Minute
)

// wikiFollowUpReserveScript 用 Redis 原子操作控制 follow-up 任务的并发上限。
// 预留覆盖整个 asynq 任务生命周期（包括延迟任务和重试）。
// 预留数量 = max(0, maxInflight+1 - 当前已预留数)，额外 +1 是为了让最后一个
// 完成的批次可以在释放自己的槽位之前先预定好下一个接班人。
const wikiFollowUpReserveScript = `
local available = math.max(0, tonumber(ARGV[2]) - redis.call('ZCARD', KEYS[1]))
local n = math.min(tonumber(ARGV[1]), available)
local ids = {}
for i = 1, n do
  local id = ARGV[3] .. ':' .. i
  redis.call('ZADD', KEYS[1], ARGV[4], id)
  ids[#ids + 1] = id
end
return ids
`

// wikiFollowUpReconcileScript 用于对账：检查 asynq 中任务的实际状态，
// 更新或移除对应的预留记录。ZADD XX 避免复活已完成的任务。
const wikiFollowUpReconcileScript = `
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if score and tonumber(score) == tonumber(ARGV[2]) then
  if ARGV[3] == 'remove' then
    redis.call('ZREM', KEYS[1], ARGV[1])
  else
    redis.call('ZADD', KEYS[1], 'XX', ARGV[3], ARGV[1])
  end
end
return 1
`

// reserveFollowUpTasks 为 KB 预留 n 个 follow-up 任务槽位，返回实际预留的任务 ID 列表。
// 预留数量受 maxInflight 限制，超过上限的部分会被拒绝。
func (s *wikiIngestService) reserveFollowUpTasks(
	ctx context.Context, kbID string, requested, maxInflight int,
) ([]string, error) {
	if requested <= 0 {
		return nil, nil
	}
	if err := s.reconcileFollowUpTasks(ctx, kbID); err != nil {
		return nil, err
	}
	return s.redisClient.Eval(ctx, wikiFollowUpReserveScript,
		[]string{wikiFollowUpPrefix + kbID}, requested, max(1, maxInflight)+1,
		"wiki-followup-"+uuid.NewString(), time.Now().Add(wikiFollowUpReconcileAfter).UnixMilli(),
	).StringSlice()
}

// reconcileFollowUpTasks 对账：检查过期的预留记录，与 asynq 中的实际任务状态对比，
// 更新或移除已失效的预留。防止因任务入队失败或 worker 崩溃导致预留泄漏。
func (s *wikiIngestService) reconcileFollowUpTasks(ctx context.Context, kbID string) error {
	key := wikiFollowUpPrefix + kbID
	now := time.Now()
	entries, err := s.redisClient.ZRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10),
	}).Result()
	if err != nil || len(entries) == 0 {
		return err
	}
	// 共享 service 的 Redis 连接；Inspector.Close 不能关闭它
	inspector := asynq.NewInspectorFromRedisClient(s.redisClient)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, ok := entry.Member.(string)
		if !ok {
			continue
		}
		info, err := inspector.GetTaskInfo(types.QueueWiki, id)
		missing := errors.Is(err, asynq.ErrTaskNotFound) || errors.Is(err, asynq.ErrQueueNotFound)
		if err != nil && !missing {
			return err
		}
		action := strconv.FormatInt(now.Add(wikiFollowUpReconcileAfter).UnixMilli(), 10)
		if missing || info.State == asynq.TaskStateArchived || info.State == asynq.TaskStateCompleted {
			action = "remove"
		}
		if err := s.redisClient.Eval(ctx, wikiFollowUpReconcileScript,
			[]string{key}, id, entry.Score, action).Err(); err != nil {
			return err
		}
	}
	return nil
}

// releaseFollowUpTask 释放当前任务的 follow-up 预留。
// 在批次处理完成时调用，确保容量能被后续批次使用。
func (s *wikiIngestService) releaseFollowUpTask(ctx context.Context, kbID string) {
	if s.redisClient == nil {
		return
	}
	id, ok := asynq.GetTaskID(ctx)
	if !ok {
		return
	}
	cleanupCtx, cancel := wikiIngestCleanupContext(ctx)
	defer cancel()
	if err := s.redisClient.ZRem(cleanupCtx, wikiFollowUpPrefix+kbID, id).Err(); err != nil {
		logger.Warnf(ctx, "wiki ingest: follow-up reservation release failed: %v", err)
	}
}

// scheduleFollowUpRecheck 安排一个合并的探针任务，用于在所有容量都被预留
// 但入队失败或 worker 崩溃时仍能推进进度。每个 KB 最多有两个探针在等待。
// 它不受预留预算限制，是兜底机制。
func (s *wikiIngestService) scheduleFollowUpRecheck(ctx context.Context, payload WikiIngestPayload) bool {
	langfuse.InjectTracing(ctx, &payload)
	b, _ := json.Marshal(payload)
	id := "wiki-followup-recheck-" + payload.KnowledgeBaseID
	if current, _ := asynq.GetTaskID(ctx); current == id {
		// 当前任务 ID 在 asynq 确认前仍被占用，交替使用两个 ID 让探针可以自我续期
		id += "-next"
	}
	t := asynq.NewTask(types.TypeWikiIngest, b,
		asynq.Queue(types.QueueWiki),
		asynq.MaxRetry(wikiIngestMaxRetry),
		asynq.Timeout(60*time.Minute),
		asynq.ProcessIn(wikiFollowUpReconcileAfter+wikiFollowUpDelay),
		asynq.TaskID(id),
	)
	if _, err := s.task.Enqueue(t); err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
			return true
		}
		logger.Warnf(ctx, "wiki ingest: follow-up recheck enqueue failed: %v", err)
		return false
	}
	return true
}

// followUpTriggerCount 根据待处理文档数、批次大小、当前活跃槽位和最大并发数，
// 计算应该启动多少个 follow-up 任务。
// 原理：先算需要多少个批次才能处理完，再算还有多少空闲槽位，取两者的较小值。
// activeSlots 为 -1 或 maxInflight <= 1 时保守返回 1。
func followUpTriggerCount(pending, batchSize, activeSlots, maxInflight int) int {
	if pending <= 0 || batchSize <= 0 {
		return 0
	}
	batchesNeeded := (pending + batchSize - 1) / batchSize
	if activeSlots < 0 || maxInflight <= 1 {
		return 1
	}
	// +1 允许正在完成的批次在释放自己的槽位之前先预定接班人
	freeSlots := maxInflight - activeSlots + 1
	if freeSlots < 1 {
		freeSlots = 1
	}
	return min(batchesNeeded, freeSlots)
}
