package sourceloc

import "github.com/Tencent/WeKnora/internal/types"

// RemapBlocks 把基于 oldText 记录的块重新对准到 newText 上。
// 塌缩为空的块会被丢弃。
func RemapBlocks(blocks []types.SourceBlock, oldText, newText string) []types.SourceBlock {
	if len(blocks) == 0 || oldText == newText {
		return blocks
	}
	// 标记或 URL 改写后按完整块文本重新对齐。按比例插值可能为被删除或
	// 被修改的内容编造出一个位置。
	oldRunes := []rune(oldText)
	units := make([]Unit, 0, len(blocks))
	for _, b := range blocks {
		if b.Start < 0 || b.End > len(oldRunes) || b.End <= b.Start {
			continue
		}
		text := normalizeMarkdown(string(oldRunes[b.Start:b.End])).text
		units = append(units, Unit{Text: text, Locator: b.Locator})
	}
	return Align(newText, units)
}
