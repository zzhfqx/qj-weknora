package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// citedChunkRepo 仅实现 resolveCitedChunks 路径会用到的两个仓储查询，
// 按 ID / 父 ID 列表返回预置块。内嵌接口的其余方法保持 nil：
// 该路径不应触达它们，一旦触达测试会立刻 panic。
type citedChunkRepo struct {
	interfaces.ChunkRepository
	byID     map[string][]*types.Chunk
	byParent map[string][]*types.Chunk
}

func (r *citedChunkRepo) ListChunksByID(
	_ context.Context, _ uint64, ids []string,
) ([]*types.Chunk, error) {
	var out []*types.Chunk
	for _, id := range ids {
		out = append(out, r.byID[id]...)
	}
	return out, nil
}

func (r *citedChunkRepo) ListChunksByParentIDs(
	_ context.Context, _ uint64, parentIDs []string,
) ([]*types.Chunk, error) {
	var out []*types.Chunk
	for _, id := range parentIDs {
		out = append(out, r.byParent[id]...)
	}
	return out, nil
}

func citationChunk(id, content string) *types.Chunk {
	return &types.Chunk{
		ID:          id,
		TenantID:    1,
		KnowledgeID: "kn-1",
		Content:     content,
		ChunkType:   types.ChunkTypeText,
		IsEnabled:   true,
	}
}

func imageChildChunk(id, parentID, chunkType types.ChunkType, imageInfoJSON string, enabled bool) *types.Chunk {
	return &types.Chunk{
		ID:            id,
		TenantID:      1,
		KnowledgeID:   "kn-1",
		ChunkType:     chunkType,
		IsEnabled:     enabled,
		ParentChunkID: parentID,
		ImageInfo:     imageInfoJSON,
	}
}

// TestResolveCitedChunksRetainsScannedOCR 验证 Reduce 阶段在 image_info 缺失、
// 只有 caption、已有 OCR 三种情况下，扫描页 OCR 正文都恰好到达一次（不丢失、
// 不与 image_info 内联重复）。
func TestResolveCitedChunksRetainsScannedOCR(t *testing.T) {
	const text = "The contractual amount is 123456 dollars."
	for _, tc := range []struct {
		name string
		info string
	}{
		{name: "missing image info"},
		{name: "caption without OCR", info: `[{"url":"resource://page","caption":"Contract page"}]`},
		{name: "existing OCR", info: `[{"url":"resource://page","ocr_text":"` + text + `"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := citationChunk("page", "![page](resource://page)")
			ocr := imageChildChunk("ocr", parent.ID, types.ChunkTypeImageOCR, tc.info, true)
			ocr.Content = text
			cited := wikiCitationChunks([]*types.Chunk{parent, ocr})
			if len(cited) != 1 || !strings.Contains(cited[0].Content, text) {
				t.Fatal("引用分类未收到扫描页 OCR")
			}
			repo := &citedChunkRepo{
				byID:     map[string][]*types.Chunk{parent.ID: {parent}},
				byParent: map[string][]*types.Chunk{parent.ID: {ocr}},
			}
			svc := &wikiIngestService{chunkRepo: repo}
			out := svc.resolveCitedChunks(context.Background(), 1, []SlugUpdate{
				{KnowledgeID: "kn-1", SourceChunks: []string{cited[0].ID}},
			})
			if strings.Count(out[parent.ID], text) != 1 {
				t.Fatalf("Reduce 收到的 OCR 次数不为 1: %q", out[parent.ID])
			}
		})
	}
}

// TestResolveCitedChunksExcludesDisabledScannedEvidence 验证停用证据不进 Reduce：
// 停用的 OCR 子块不追加；父块本身停用时整块不出现。
func TestResolveCitedChunksExcludesDisabledScannedEvidence(t *testing.T) {
	for _, disabledParent := range []bool{false, true} {
		parent := citationChunk("page", "![page](resource://page)")
		parent.IsEnabled = !disabledParent
		ocr := imageChildChunk("ocr", parent.ID, types.ChunkTypeImageOCR, "", disabledParent)
		ocr.Content = "DISABLED_TEXT"
		repo := &citedChunkRepo{
			byID:     map[string][]*types.Chunk{parent.ID: {parent}},
			byParent: map[string][]*types.Chunk{parent.ID: {ocr}},
		}
		svc := &wikiIngestService{chunkRepo: repo}
		out := svc.resolveCitedChunks(context.Background(), 1, []SlugUpdate{
			{KnowledgeID: "kn-1", SourceChunks: []string{parent.ID}},
		})
		if strings.Contains(out[parent.ID], "DISABLED_TEXT") {
			t.Fatalf("停用证据进入了 Reduce: %q", out[parent.ID])
		}
		if disabledParent && out[parent.ID] != "" {
			t.Fatalf("停用父块进入了 Reduce: %q", out[parent.ID])
		}
	}
}
