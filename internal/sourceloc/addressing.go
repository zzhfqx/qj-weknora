// Package sourceloc 把解析后的 Markdown 映射回原始文件中的位置：
// 在摄入流程改写文本后重新瞄准解析器上报的源块、把解析结果与 Office 文件的
// 结构对齐，并推导出每个 chunk 的定位器，供引用导航使用。
package sourceloc

import (
	"html"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
)

// Index 回答分块器的问题：“最终 Markdown 的这一段范围覆盖了原始文件的哪些
// 位置？”
type Index struct {
	runes  []rune
	blocks []types.SourceBlock // 按 Start 排序
	maxEnd []int               // maxEnd[k] = blocks[:k+1] 中的最大 End
}

// NewIndex 基于记录在 markdown 上的块构建索引。
func NewIndex(markdown string, blocks []types.SourceBlock) *Index {
	if len(blocks) == 0 {
		return nil
	}
	sorted := append([]types.SourceBlock(nil), blocks...)
	SortBlocks(sorted)
	maxEnd := make([]int, len(sorted))
	for k, b := range sorted {
		maxEnd[k] = b.End
		if k > 0 && maxEnd[k-1] > b.End {
			maxEnd[k] = maxEnd[k-1]
		}
	}
	return &Index{runes: []rune(markdown), blocks: sorted, maxEnd: maxEnd}
}

// Locators 返回与字符范围 [start, end) 重叠的块的定位器，并为每个定位器
// 附上重叠部分的文本作为引用片段。指向同一位置的相邻定位器（同一页但没有
// 区域、同一张幻灯片、连续的行）会被合并。
func (x *Index) Locators(start, end int) types.SourceLocators {
	if x == nil || end <= start {
		return nil
	}
	start = max(start, 0)
	end = min(end, len(x.runes))
	// i 之前的块都在 start 或之前结束。
	i := sort.Search(len(x.blocks), func(k int) bool { return x.maxEnd[k] > start })
	var out types.SourceLocators
	covered, partial := start, false
	for ; i < len(x.blocks) && x.blocks[i].Start < end; i++ {
		b := x.blocks[i]
		lo, hi := max(b.Start, start), min(b.End, end)
		if hi <= lo {
			continue
		}
		if lo > covered && normalizeMarkdown(fullQuote(string(x.runes[covered:lo]))).text != "" {
			partial = true
		}
		covered = max(covered, hi)
		quote := fullQuote(string(x.runes[lo:hi]))
		if quote == "" && b.Locator.Type != types.SourceLocatorPDF {
			continue
		}
		loc := b.Locator
		loc.BBox = append([]float64(nil), loc.BBox...)
		loc.Quote = quote
		out = AppendLocator(out, loc)
	}
	if covered < end && normalizeMarkdown(fullQuote(string(x.runes[covered:end]))).text != "" {
		partial = true
	}
	if partial {
		for i := range out {
			out[i].Partial = true
		}
	}
	return out
}

// LocatorsAt 返回包含字符偏移 pos 的块的定位器，不带引用片段。
// 用于定位 Markdown 中发现的图片引用。
func (x *Index) LocatorsAt(pos int) types.SourceLocators {
	if x == nil || pos < 0 {
		return nil
	}
	i := sort.Search(len(x.blocks), func(k int) bool { return x.maxEnd[k] > pos })
	var out types.SourceLocators
	for ; i < len(x.blocks) && x.blocks[i].Start <= pos; i++ {
		if b := x.blocks[i]; b.End > pos {
			loc := b.Locator
			loc.BBox = append([]float64(nil), loc.BBox...)
			loc.Quote = ""
			out = append(out, loc)
		}
	}
	return out
}

// AppendLocator 把 loc 追加到列表；当它与最后一项指向同一位置时合并进去。
func AppendLocator(list types.SourceLocators, loc types.SourceLocator) types.SourceLocators {
	if n := len(list); n > 0 && foldInto(&list[n-1], loc) {
		return list
	}
	return append(list, loc)
}

func foldInto(last *types.SourceLocator, loc types.SourceLocator) bool {
	if last.Type != loc.Type ||
		last.Partial != loc.Partial ||
		last.Mapping != loc.Mapping ||
		last.SourceHash != loc.SourceHash ||
		last.SourceID != loc.SourceID {
		return false
	}
	switch loc.Type {
	case types.SourceLocatorPDF:
		if last.Page != loc.Page || len(last.BBox) > 0 || len(loc.BBox) > 0 {
			return false
		}
	case types.SourceLocatorSlide:
		if last.Slide != loc.Slide {
			return false
		}
	case types.SourceLocatorSection:
		if last.Section != loc.Section {
			return false
		}
	case types.SourceLocatorSheet:
		if last.Sheet != loc.Sheet || loc.RowStart > last.RowEnd+1 || loc.RowEnd < last.RowStart {
			return false
		}
		last.RowStart = min(last.RowStart, loc.RowStart)
		last.RowEnd = max(last.RowEnd, loc.RowEnd)
	case types.SourceLocatorText:
		if loc.Start > last.End+2 || loc.End < last.Start {
			return false
		}
		last.Start = min(last.Start, loc.Start)
		last.End = max(last.End, loc.End)
	default:
		return false
	}
	last.Quote = joinQuote(last.Quote, loc.Quote)
	return true
}

// MergeLocators 合并两个定位器列表，去掉完全重复的项，用于聊天链路中
// 合并后的检索结果。
func MergeLocators(a, b types.SourceLocators) types.SourceLocators {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}
	out := append(types.SourceLocators(nil), a...)
	for _, loc := range b {
		dup := false
		for _, have := range out {
			if sameLocator(have, loc) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, loc)
		}
	}
	return out
}

func sameLocator(a, b types.SourceLocator) bool {
	if a.Partial != b.Partial ||
		a.Quote != b.Quote ||
		a.SourceID != b.SourceID ||
		a.SourceHash != b.SourceHash ||
		a.Mapping != b.Mapping ||
		a.Type != b.Type ||
		a.Page != b.Page ||
		a.Block != b.Block ||
		a.Slide != b.Slide ||
		a.Sheet != b.Sheet || a.RowStart != b.RowStart || a.RowEnd != b.RowEnd ||
		a.Start != b.Start || a.End != b.End || a.StartMs != b.StartMs || a.EndMs != b.EndMs ||
		a.Section != b.Section || len(a.BBox) != len(b.BBox) {
		return false
	}
	for i := range a.BBox {
		if a.BBox[i] != b.BBox[i] {
			return false
		}
	}
	return true
}

var (
	quoteImageRe = regexp.MustCompile(`!\[[^\]\n]*\]\([^)\n]*\)`)
	quoteLinkRe  = regexp.MustCompile(`\[([^\]\n]*)\]\([^)\n]*\)`)
	quoteTagRe   = regexp.MustCompile(`</?[a-zA-Z][^>\n]*>|<!--.*?-->`)
	quoteSpaceRe = regexp.MustCompile(`\s+`)
)

// CleanQuote 去掉文本中的 Markdown 图片/链接目标地址和 HTML 标签，
// 合并空白，并截断到 types.SourceLocatorQuoteMax 个字符。
func CleanQuote(text string) string {
	return capRunes(fullQuote(text), types.SourceLocatorQuoteMax)
}

// fullQuote 保留完整证据；提前截断可能把位于末尾的引用藏起来。
func fullQuote(text string) string {
	text = quoteImageRe.ReplaceAllString(text, " ")
	text = quoteLinkRe.ReplaceAllString(text, "$1")
	text = quoteTagRe.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = strings.TrimSpace(quoteSpaceRe.ReplaceAllString(text, " "))
	return text
}

func joinQuote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " " + b
}

func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
