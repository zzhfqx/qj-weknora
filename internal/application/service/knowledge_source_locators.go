package service

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/asr"
	"github.com/Tencent/WeKnora/internal/sourceloc"
	"github.com/Tencent/WeKnora/internal/types"
)

// attachStructureBlocks 在解析器没有上报源块时，为解析结果补充原文定位：
// Office 文件按自身结构对齐，解析器原样透传的纯文本直接映射回自身。
// 失败只会损失定位信息，不会影响解析结果。
func attachStructureBlocks(ctx context.Context, fileType string, data []byte, result *types.ReadResult) {
	if result == nil || len(result.SourceBlocks) > 0 || result.MarkdownContent == "" || len(data) == 0 {
		return
	}
	ft := strings.ToLower(strings.TrimPrefix(fileType, "."))
	switch {
	case sourceloc.SupportsStructure(ft):
		started := time.Now()
		blocks, err := sourceloc.AlignStructure(ft, data, result.MarkdownContent)
		if err != nil {
			logger.Warnf(ctx, "[SourceLocator] 读取 %s 结构失败: %v", ft, err)
			return
		}
		result.SourceBlocks = blocks
		logger.Infof(ctx, "[SourceLocator] 已对齐 %d 个 %s 结构块，耗时 %s", len(blocks), ft, time.Since(started))
	case ft == "md" || ft == "markdown" || ft == "txt" || ft == "text":
		if result.MarkdownContent == string(data) {
			result.SourceBlocks = passThroughTextBlocks(result.MarkdownContent)
		}
	}
}

// passThroughTextBlocks 把原样透传的文本原文映射回自身。文本定位器的偏移从
// UTF-8 BOM 之后开始计算，因为查看器解码文件时不含 BOM；而块本身仍然覆盖
// 包含 BOM 的 Markdown。
func passThroughTextBlocks(content string) []types.SourceBlock {
	body, hasBOM := strings.CutPrefix(content, "\ufeff")
	blocks := sourceloc.TextBlocks(body)
	if hasBOM {
		for i := range blocks {
			blocks[i].Start++
			blocks[i].End++
		}
	}
	return blocks
}

// transcriptWithSegments 把 ASR 结果渲染为每个带时间戳片段一行的文本，
// 并生成把每一行指回音频的时间块。没有片段时返回纯转写文本且没有块。
func transcriptWithSegments(result *asr.TranscriptionResult) (string, []types.SourceBlock) {
	if result == nil {
		return "", nil
	}
	if len(result.Segments) == 0 {
		return result.Text, nil
	}
	var (
		sb     strings.Builder
		blocks []types.SourceBlock
		pos    int
	)
	for _, seg := range result.Segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
			pos++
		}
		n := len([]rune(text))
		sb.WriteString(text)
		blocks = append(blocks, types.SourceBlock{
			Start: pos, End: pos + n,
			Locator: types.SourceLocator{
				Type:    types.SourceLocatorTime,
				StartMs: int64(seg.Start * 1000),
				EndMs:   int64(seg.End * 1000),
			},
		})
		pos += n
	}
	if len(blocks) == 0 {
		return result.Text, nil
	}
	return sb.String(), blocks
}

// buildSourceIndex 把解析器上报的源块重新对准到分块器将要切分的 Markdown，
// 并把每一张已存储的图片放到原始文件中的位置。
// parsed 是源块记录时所针对的 Markdown。
func buildSourceIndex(
	parsed string, blocks []types.SourceBlock, final string, images []docparser.StoredImage,
) *sourceloc.Index {
	if len(blocks) == 0 {
		return nil
	}
	idx := sourceloc.NewIndex(final, sourceloc.RemapBlocks(blocks, parsed, final))
	if idx == nil {
		return nil
	}
	// 按文档顺序访问图片，这样只需遍历一遍 Markdown 即可累计字符偏移，
	// 而不是每张图片都遍历一次。
	type placed struct{ image, at int }
	var found []placed
	for i := range images {
		if images[i].ServingURL == "" {
			continue
		}
		if at := strings.Index(final, images[i].ServingURL); at >= 0 {
			found = append(found, placed{image: i, at: at})
		}
	}
	sort.Slice(found, func(a, b int) bool { return found[a].at < found[b].at })
	bytePos, runePos := 0, 0
	for _, f := range found {
		runePos += utf8.RuneCountInString(final[bytePos:f.at])
		bytePos = f.at
		images[f.image].SourceLocators = idx.LocatorsAt(runePos)
	}
	return idx
}
