package service

import (
	"context"
	"math"
	"slices"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// classifyRetrievalResults 按检索类型（向量 / 关键词）分离检索结果。
// 每个 RetrieveResult 保持为独立的列表：它们来自不同的引擎、存储组和参数集（文档 vs FAQ），
// 按 goroutine 完成顺序追加，排名只在列表内部有意义。
func classifyRetrievalResults(ctx context.Context, retrieveResults []*types.RetrieveResult) (
	vectorLists, keywordLists [][]*types.IndexWithScore,
) {
	for _, retrieveResult := range retrieveResults {
		logger.Infof(ctx, "Retrieval results, engine: %v, retriever: %v, count: %v",
			retrieveResult.RetrieverEngineType,
			retrieveResult.RetrieverType,
			len(retrieveResult.Results),
		)
		if len(retrieveResult.Results) == 0 {
			continue
		}
		if retrieveResult.RetrieverType == types.VectorRetrieverType {
			vectorLists = append(vectorLists, retrieveResult.Results)
		} else {
			keywordLists = append(keywordLists, retrieveResult.Results)
		}
	}
	return
}

// flattenLists 拼接多个检索结果列表为一个扁平列表。
func flattenLists(lists [][]*types.IndexWithScore) []*types.IndexWithScore {
	var out []*types.IndexWithScore
	for _, list := range lists {
		out = append(out, list...)
	}
	return out
}

// fuseOrDeduplicate 要么通过 RRF 融合向量+关键词结果，要么对单检索器结果去重。
// 所有路径返回的分数都归一化到 [0, 1]，这样来自不同检索（按文档目标、
// 嵌入模型分组、FAQ vs 文档调用）的结果可以放在一起排名：
//   - 纯向量：保持引擎原始的余弦相似度（FAQ 阈值依赖它）；
//   - 纯关键词：BM25 除以自身列表的最高分；
//   - 混合：加权 RRF 除以理论最大值（双第一的 chunk 得分为 1）。
//
// retrievalCfg 可以为 nil，此时使用 RRF 默认参数。
func fuseOrDeduplicate(
	ctx context.Context, vectorLists, keywordLists [][]*types.IndexWithScore, retrievalCfg *types.RetrievalConfig,
) []*types.IndexWithScore {
	if len(keywordLists) == 0 {
		// Vector-only: keep original embedding scores (important for FAQ)
		result := deduplicateByScore(flattenLists(vectorLists))
		logger.Infof(ctx, "Result count after deduplication: %d", len(result))
		return result
	}
	if len(vectorLists) == 0 {
		// 纯关键词：保持 BM25 的相对顺序，但将无界分数归一化到 [0, 1]
		// 再传给 rerank/MMR。原始 BM25（常 > 10）会让 compositeScore
		// 的 0.3*base 项饱和到 1.0，导致所有候选打平，MMR 退化为纯多样性排序。
		// 不同引擎或索引的 BM25 不可比，所以每个列表按自身最高分缩放。
		for _, list := range keywordLists {
			rescaleUnboundedScores(list)
		}
		result := deduplicateByScore(flattenLists(keywordLists))
		logger.Infof(ctx, "Result count after deduplication: %d", len(result))
		return result
	}
	// Hybrid: use RRF fusion to merge vector + keyword results
	result := fuseWithRRF(ctx, vectorLists, keywordLists, retrievalCfg)
	logger.Infof(ctx, "Result count after RRF fusion: %d", len(result))
	return result
}

// sortByScoreDesc is a reusable sort comparator for IndexWithScore slices (descending by Score).
func sortByScoreDesc(a, b *types.IndexWithScore) int {
	if a.Score > b.Score {
		return -1
	} else if a.Score < b.Score {
		return 1
	}
	return 0
}

// deduplicateByScore 按 chunk ID 对检索结果去重，保留每个 chunk 的最高分。
// 返回按分数降序排列的结果。
// 用于只有单个检索器（例如 FAQ 的纯向量检索）的场景。
func deduplicateByScore(results []*types.IndexWithScore) []*types.IndexWithScore {
	chunkInfoMap := make(map[string]*types.IndexWithScore, len(results))
	for _, r := range results {
		if existing, exists := chunkInfoMap[r.ChunkID]; !exists || r.Score > existing.Score {
			chunkInfoMap[r.ChunkID] = r
		}
	}
	deduped := make([]*types.IndexWithScore, 0, len(chunkInfoMap))
	for _, info := range chunkInfoMap {
		deduped = append(deduped, info)
	}
	slices.SortFunc(deduped, sortByScoreDesc)
	return deduped
}

// rescaleUnboundedScores 将单检索器的候选集合映射到 [0, 1] 区间，
// 方法是除以最大的有限分数。关键词（BM25）分数是无界的；
// 如果保持原样，compositeScore（0.3*base，上限 1.0）会饱和，
// 导致所有候选都打平，MMR 退化为纯多样性排序。
//
// 已经在 [0, 1] 内的分数保持不变，因此那些将关键词命中标记为 1.0 的引擎
// （Qdrant、Doris）和已经归一化的向量分数保持原有量级。
// 无论哪种情况，相对顺序都保持不变。
// 检索/Langfuse 采样发生在融合之前，因此原始 BM25 在检索 span 中仍然可见。
func rescaleUnboundedScores(results []*types.IndexWithScore) {
	maxScore := 0.0
	for _, r := range results {
		if r == nil {
			continue
		}
		if math.IsNaN(r.Score) || math.IsInf(r.Score, 0) {
			continue
		}
		if r.Score > maxScore {
			maxScore = r.Score
		}
	}
	if maxScore <= 1 {
		return
	}
	for _, r := range results {
		if r == nil {
			continue
		}
		switch {
		case math.IsNaN(r.Score), math.IsInf(r.Score, -1), r.Score <= 0:
			r.Score = 0
		case math.IsInf(r.Score, 1):
			r.Score = 1
		default:
			r.Score = r.Score / maxScore
		}
	}
}

// fuseWithRRF 使用倒数排名融合（Reciprocal Rank Fusion）合并向量和关键词检索结果。
// RRF 分数 = vectorWeight/(k+vectorRank) + keywordWeight/(k+keywordRank)，
// 再除以理论最大值 (vectorWeight+keywordWeight)/(k+1)，使结果落在 [0, 1]，
// 与单检索器路径的量纲一致。
// 原始 RRF 的最大值接近 0.016，导致混合检索的结果总是输给纯向量或纯关键词结果，
// 也使 rerank 的 base-score 项和 MMR 的 relevance 项几乎不起作用。
//
// 每个 chunk 在某类检索器中的排名是其在该类所有列表中的最佳名次，
// 每个列表按自身分数排序。直接拼接多个列表的位置是没有意义的：
// 否则第二个列表的最佳结果会排在第一个列表的所有结果之后。
//
// k、vectorWeight 和 keywordWeight 来自 retrievalCfg（有默认值）。
// 合并结果按 RRF 分数降序排列。
func fuseWithRRF(
	ctx context.Context, vectorLists, keywordLists [][]*types.IndexWithScore, retrievalCfg *types.RetrievalConfig,
) []*types.IndexWithScore {
	rrfK := retrievalCfg.GetEffectiveRRFK()
	vectorWeight, keywordWeight := retrievalCfg.GetEffectiveRRFWeights()
	maxRRF := (vectorWeight + keywordWeight) / float64(rrfK+1)

	vectorRanks := bestRanks(vectorLists)
	keywordRanks := bestRanks(keywordLists)

	// Collect all unique chunks — prefer vector result's metadata for each chunk
	chunkInfoMap := make(map[string]*types.IndexWithScore)
	for _, r := range flattenLists(vectorLists) {
		if existing, exists := chunkInfoMap[r.ChunkID]; !exists || r.Score > existing.Score {
			chunkInfoMap[r.ChunkID] = r
		}
	}
	for _, r := range flattenLists(keywordLists) {
		if _, exists := chunkInfoMap[r.ChunkID]; !exists {
			chunkInfoMap[r.ChunkID] = r
		}
	}

	// Compute weighted RRF scores and assign to each chunk
	result := make([]*types.IndexWithScore, 0, len(chunkInfoMap))
	for chunkID, info := range chunkInfoMap {
		rrfScore := 0.0
		if rank, ok := vectorRanks[chunkID]; ok {
			rrfScore += vectorWeight / float64(rrfK+rank)
		}
		if rank, ok := keywordRanks[chunkID]; ok {
			rrfScore += keywordWeight / float64(rrfK+rank)
		}
		info.Score = rrfScore / maxRRF
		result = append(result, info)
	}
	slices.SortFunc(result, sortByScoreDesc)

	// Log top results for debugging
	for i, chunk := range result {
		if i >= 15 {
			break
		}
		vRank, vOk := vectorRanks[chunk.ChunkID]
		kRank, kOk := keywordRanks[chunk.ChunkID]
		logger.Debugf(ctx, "RRF rank %d: chunk_id=%s, rrf_score=%.6f, vector_rank=%v(%v), keyword_rank=%v(%v)",
			i, chunk.ChunkID, chunk.Score, vRank, vOk, kRank, kOk)
	}

	return result
}

// bestRanks 返回每个 chunk 在所有列表中的最佳 1-based 排名，
// 每个列表先按自身分数排序去重（同一列表中重复的 chunk 取最高分）。
func bestRanks(lists [][]*types.IndexWithScore) map[string]int {
	ranks := make(map[string]int)
	for _, list := range lists {
		for i, r := range deduplicateByScore(list) {
			if current, ok := ranks[r.ChunkID]; !ok || i+1 < current {
				ranks[r.ChunkID] = i + 1
			}
		}
	}
	return ranks
}
