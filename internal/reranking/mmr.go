package reranking

import (
	"context"
	"math"

	"github.com/Tencent/WeKnora/internal/searchutil"
	"github.com/Tencent/WeKnora/internal/types"
)

// DefaultMMRLambda 控制 SelectMMR 中相关性与冗余度的权重平衡。
const DefaultMMRLambda = 0.7

// SelectMMR 基于最大边际相关性（Maximal Marginal Relevance）从结果中选出最多 k 个，
// 综合考虑结果的 Score 和富文本段落的 token 重叠度，返回选中结果的索引（按选择顺序）。
// 分数相同时优先选择排在前面的结果。
func SelectMMR(ctx context.Context, results []*types.SearchResult, k int, lambda float64) []int {
	if k <= 0 || len(results) == 0 {
		return nil
	}
	// 第一个选择没有冗余项。当调用方只需要一个结果时，
	// 跳过段落清洗和 token 化，直接选最高分。
	if k == 1 {
		best, bestScore := 0, math.Inf(-1)
		for i, r := range results {
			if score := lambda*r.Score - (1-lambda)*0; score > bestScore {
				best, bestScore = i, score
			}
		}
		return []int{best}
	}

	remaining := make([]int, len(results))
	for i := range results {
		remaining[i] = i
	}
	tokens, offsets, postings := buildMMRPostings(ctx, results)

	// 缓存每个候选与已选段落的最大 Jaccard 相似度。
	// Postings 只对共享 token 的候选计算交集；不相交的段落冗余度为 0，无需更新。
	maxRedundancy := make([]float64, len(results))
	intersections := make([]int, len(results))
	taken := make([]bool, len(results))
	touched := make([]int, 0, len(results))
	selected := make([]int, 0, min(k, len(results)))
	for len(selected) < k && len(remaining) > 0 {
		bestPos := 0
		bestScore := math.Inf(-1)
		for pos, i := range remaining {
			mmr := lambda*results[i].Score - (1-lambda)*maxRedundancy[i]
			if mmr > bestScore {
				bestScore = mmr
				bestPos = pos
			}
		}
		chosen := remaining[bestPos]
		selected = append(selected, chosen)
		taken[chosen] = true
		remaining = append(remaining[:bestPos], remaining[bestPos+1:]...)
		if len(selected) == k || len(remaining) == 0 {
			break
		}
		touched = touched[:0]
		for _, token := range tokens[chosen] {
			for _, i := range postings[offsets[token]:offsets[token+1]] {
				if taken[i] {
					continue
				}
				if intersections[i] == 0 {
					touched = append(touched, i)
				}
				intersections[i]++
			}
		}
		for _, i := range touched {
			intersection := intersections[i]
			union := len(tokens[i]) + len(tokens[chosen]) - intersection
			maxRedundancy[i] = math.Max(maxRedundancy[i], float64(intersection)/float64(union))
			intersections[i] = 0
		}
	}
	return selected
}

// buildMMRPostings 将每个唯一 token 内部编号，并将候选 ID 存储在一个扁平数组中。
// offsets[t]:offsets[t+1] 是 token t 的倒排列表（posting list）。
// 每行来自一个 token 集合，因此一个共享 token 对一对候选的交集只贡献一次，
// 与其在任一段落中的出现频率无关。
func buildMMRPostings(
	ctx context.Context, results []*types.SearchResult,
) (tokens [][]int, offsets, postings []int) {
	termIDs := make(map[string]int)
	var counts []int
	tokens = make([][]int, len(results))
	for i, r := range results {
		set := searchutil.TokenizeSimple(EnrichedPassage(ctx, r))
		tokens[i] = make([]int, 0, len(set))
		for term := range set {
			id, exists := termIDs[term]
			if !exists {
				id = len(counts)
				termIDs[term] = id
				counts = append(counts, 0)
			}
			counts[id]++
			tokens[i] = append(tokens[i], id)
		}
	}
	offsets = make([]int, len(counts)+1)
	for id, count := range counts {
		offsets[id+1] = offsets[id] + count
	}
	postings = make([]int, offsets[len(counts)])
	// 复用 counts 作为插入游标；offsets 保持不变，用于查找。
	copy(counts, offsets)
	for i, row := range tokens {
		for _, id := range row {
			postings[counts[id]] = i
			counts[id]++
		}
	}
	return tokens, offsets, postings
}
