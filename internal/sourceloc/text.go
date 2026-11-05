package sourceloc

import "github.com/Tencent/WeKnora/internal/types"

// TextBlocks 把解析器原样透传的纯文本原文（Markdown、TXT）拆成段落块，
// 其 text 定位器指向原文中对应的字符区间。
func TextBlocks(content string) []types.SourceBlock {
	var blocks []types.SourceBlock
	start := -1 // 当前段落起始的字符偏移
	pos := 0
	blankRun := 0
	emit := func(end int) {
		if start >= 0 && end > start {
			blocks = append(blocks, types.SourceBlock{
				Start: start, End: end,
				Locator: types.SourceLocator{Type: types.SourceLocatorText, Mapping: "exact", Start: start, End: end},
			})
		}
		start = -1
	}
	lineHasText := false
	lineStart := 0
	for _, r := range content {
		switch r {
		case '\n':
			if !lineHasText {
				blankRun++
				if blankRun == 1 {
					emit(lineStart)
				}
			} else {
				blankRun = 0
			}
			lineHasText = false
			lineStart = pos + 1
		case ' ', '\t', '\r':
		default:
			if !lineHasText && start < 0 {
				start = lineStart
			}
			lineHasText = true
		}
		pos++
	}
	emit(pos)
	return blocks
}
