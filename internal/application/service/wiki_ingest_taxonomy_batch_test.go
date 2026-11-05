package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/panjf2000/ants/v2"

	"github.com/Tencent/WeKnora/internal/models/embedding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// recordingEmbedder 是一个记录每次请求条数的 embedder。它内嵌的是**真实的
// 池化实现**：走 BatchEmbedWithPool 的调用会与生产环境一致地按
// BATCH_EMBED_SIZE 切分；若有人直接调 BatchEmbed，这里会收到一个超大请求，
// 测试随即失败。
type recordingEmbedder struct {
	embedding.EmbedderPooler
	mu    sync.Mutex
	sizes []int
}

func newRecordingEmbedder(t *testing.T) *recordingEmbedder {
	t.Helper()
	pool, err := ants.NewPool(4)
	if err != nil {
		t.Fatalf("ants.NewPool 失败: %v", err)
	}
	t.Cleanup(pool.Release)
	return &recordingEmbedder{EmbedderPooler: embedding.NewBatchEmbedder(pool)}
}

func (e *recordingEmbedder) BatchEmbed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.sizes = append(e.sizes, len(texts))
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, text := range texts {
		// 每条文本只给一个维度，且维度值由文本本身派生（取文本长度），
		// 这样拼回结果一旦乱序，相似度选择结果就会暴露问题。
		out[i] = []float32{float32(len(text))}
	}
	return out, nil
}

func (e *recordingEmbedder) requestSizes() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.sizes...)
}

func (e *recordingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return []float32{float32(len(text))}, nil
}
func (e *recordingEmbedder) GetModelName() string { return "recording" }
func (e *recordingEmbedder) GetDimensions() int   { return 1 }
func (e *recordingEmbedder) GetModelID() string   { return "recording" }

type recordingModelService struct {
	interfaces.ModelService
	embedder embedding.Embedder
}

func (s recordingModelService) GetEmbeddingModel(context.Context, string) (embedding.Embedder, error) {
	return s.embedder, nil
}

// TestSelectRelevantFoldersHonoursBatchEmbedSize 回归用例：目录预选曾把全部
// 文件夹、全部条目各打成一个请求发给厂商，对单请求条数有限制的 embedding
// 服务而言，无论 BATCH_EMBED_SIZE 设多低都会被拒（官方 issue #3390）。
func TestSelectRelevantFoldersHonoursBatchEmbedSize(t *testing.T) {
	t.Setenv("BATCH_EMBED_SIZE", "3")
	embedder := newRecordingEmbedder(t)
	svc := &wikiIngestService{modelService: recordingModelService{embedder: embedder}}

	// 目录数超过「全量喂给」阈值，强制走向量相似度预选分支。
	pool := make([][]string, 0, wikiTaxonomyFeedAllMaxFolders+20)
	for i := 0; i < wikiTaxonomyFeedAllMaxFolders+20; i++ {
		pool = append(pool, []string{fmt.Sprintf("L1-%d", i%4), fmt.Sprintf("deep-%d", i)})
	}
	items := make([]wikiTaxonomyItem, 0, 7)
	for i := 0; i < 7; i++ {
		items = append(items, wikiTaxonomyItem{
			slug:  fmt.Sprintf("entity/item-%d", i),
			title: fmt.Sprintf("item %d", i),
			about: "about",
		})
	}

	kb := &types.KnowledgeBase{EmbeddingModelID: "m"}
	selected := svc.selectRelevantFolders(context.Background(), kb, items, pool)

	sizes := embedder.requestSizes()
	if len(sizes) == 0 {
		t.Fatal("embedder 从未被调用，本用例已不再覆盖相似度预选分支")
	}
	for _, size := range sizes {
		if size > 3 {
			t.Fatalf("单次请求携带 %d 条输入，超过 BATCH_EMBED_SIZE=3（所有请求条数: %v）", size, sizes)
		}
	}
	// 文件夹与条目两侧都必须走池：80 个文件夹 + 7 个条目，每批 3 条。
	if total := sumInts(sizes); total != len(pool)+len(items) {
		t.Fatalf("embedder 共看到 %d 条输入，期望 %d 条（文件夹数+条目数）", total, len(pool)+len(items))
	}
	if len(selected) == 0 {
		t.Fatal("相似度预选结果为空")
	}
}

func sumInts(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
