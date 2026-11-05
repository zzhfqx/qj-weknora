package service

import (
	"errors"
	"sort"
	"strings"
)

// ErrWikiWriteDroppedTableRows 是机器写入被拒绝时返回的明确错误信号——
// 当新写入的页面内容丢失了原页面中已存在的表格行时触发。
// 原页面保持不变。
//
// 两种保护策略都会返回此错误（见 UpdatePage）。
// 之所以需要单独的错误而不能用"version 没变"来判断，是因为：
// UpdatePage 在写入成功但内容无用户可见变化时也不会修改 version，
// 所以调用方（比如 wiki 摄入的 reduce 阶段）需要一个独立的信号来区分
// "写入成功但无变化" 和 "写入被拒绝"。
var ErrWikiWriteDroppedTableRows = errors.New("wiki page write dropped table rows the stored page still has")

// wikiWriteGuardLogLimit 限制拒绝日志中最多列出多少个丢失的行标识作为示例。
const wikiWriteGuardLogLimit = 5

// wikiWriteIngestRowIdentityLossLimit 是摄入阶段重写时允许丢失的行标识比例上限。
// 只有当丢失比例**严格大于**此值时才会拒绝写入，因此丢失恰好 1/5（0.20）的行
// 仍然可以通过。
//
// 摄入阶段需要宽容度是因为：编辑提示词明确允许去重合并行，
// 如果每次合法去重都被拒绝，贡献文档会一直在 Map 阶段循环重试，
// 最终耗尽重试预算进入死信队列，新增内容永远存不进去。
// Agent 整页写入则没有这样的宽容度（见 UpdatePage）。
const wikiWriteIngestRowIdentityLossLimit = 0.20

// wikiWriteEmphasisMarkers 是内联格式标记，只影响单元格的渲染效果
// 而不改变实体名称，因此在比较行标识时会被忽略。
var wikiWriteEmphasisMarkers = strings.NewReplacer("*", "", "_", "", "`", "")

// isWikiWriteTableRow 判断一行是否为 Markdown 表格行：
// 去除首尾空白后以 '|' 开头，并且至少包含两个竖线。
// 只有一个竖线的行视为散文中的偶然字符，不算表格。
func isWikiWriteTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "|") && strings.Count(trimmed, "|") >= 2
}

// isWikiWriteDelimiterRow 判断一行是否为表格分隔行（`| --- | :--: |`），
// 即所有单元格都只由 '-'、':' 和空白组成。分隔行不代表任何实体。
func isWikiWriteDelimiterRow(line string) bool {
	for _, cell := range strings.Split(line, "|") {
		if strings.Trim(cell, "-: \t") != "" {
			return false
		}
	}
	return true
}

// normalizeWikiWriteCell 归一化单元格内容，使同一名词的不同写法可以比较相等：
// 全角空格转为普通空格、移除强调/代码标记、合并连续空白、转为小写。
// 格式和大小写是模型重写页面时最常改动的两个因素，不能因此判定为行丢失。
func normalizeWikiWriteCell(cell string) string {
	cell = strings.ReplaceAll(cell, "　", " ")
	cell = wikiWriteEmphasisMarkers.Replace(cell)
	return strings.ToLower(strings.Join(strings.Fields(cell), " "))
}

// wikiWriteRowIdentity 返回表格行的归一化行标识（第一个非空单元格的内容），
// 即这一行描述的"主体"。如果行没有任何有意义的内容则返回空字符串。
func wikiWriteRowIdentity(line string) string {
	for _, cell := range strings.Split(line, "|") {
		if identity := normalizeWikiWriteCell(cell); identity != "" {
			return identity
		}
	}
	return ""
}

// wikiWriteTableRowIdentities 按文件顺序列出内容中所有 Markdown 表格的
// 数据行标识。表头行会被跳过：分隔行（`| --- |`）正上方的行是列名而非实体，
// 模型可以随意改写它，如果计入会导致正常的重写被误拒绝。
func wikiWriteTableRowIdentities(content string) []string {
	lines := strings.Split(content, "\n")
	identities := make([]string, 0, 8)
	for i, line := range lines {
		if !isWikiWriteTableRow(line) || isWikiWriteDelimiterRow(line) {
			continue
		}
		if i+1 < len(lines) && isWikiWriteTableRow(lines[i+1]) && isWikiWriteDelimiterRow(lines[i+1]) {
			continue
		}
		if identity := wikiWriteRowIdentity(line); identity != "" {
			identities = append(identities, identity)
		}
	}
	return identities
}

// wikiWriteUniqueRowIdentityCount 统计内容中**去重后**的数据行标识数量，
// 即丢失比例的分母。同一实体出现在多行中只计一次，
// 因此同一持有人多次出现不会稀释比例。
func wikiWriteUniqueRowIdentityCount(content string) int {
	identities := wikiWriteTableRowIdentities(content)
	if len(identities) == 0 {
		return 0
	}
	unique := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		unique[identity] = struct{}{}
	}
	return len(unique)
}

// wikiWriteRowIdentityLossRatio 计算重写后丢失的行标识占原页面
// 去重行标识总数的比例（丢失数 / 原有数）。
// 原页面无可识别行时比例为 0。
func wikiWriteRowIdentityLossRatio(missing, stored int) float64 {
	if stored <= 0 {
		return 0
	}
	return float64(missing) / float64(stored)
}

// wikiWriteMissingRowIdentities 返回 existing 中有但 rewritten 中不再存在的
// 数据行标识，结果按字母排序并去重。
//
// 行的匹配基于行标识而非整行内容，因此调整空白、加粗名称、
// 更新日期/数值列都不算丢失；只有行的主体消失了才算丢失。
//
// 标识按**集合**比较而非按出现次数：只要重写内容中出现过一次就算存在，
// 因此把若干行（标识相同）合并为一行不算丢失，无论原页面列了多少行。
//
// 行标识的范围刻意收窄——只看第一个非空单元格——因此这个函数只报告
// "消失的行"，不报告"信息丢失的行"：重写后保留了名称但清空、截断
// 或改写了其他列的，**不会**被报告。合并同标识行是被允许的，
// 但不保证其他列的信息完整性；本保护只防止整行消失，不防止单元格级数据丢失。
func wikiWriteMissingRowIdentities(existing, rewritten string) []string {
	oldIdentities := wikiWriteTableRowIdentities(existing)
	if len(oldIdentities) == 0 {
		return nil
	}
	// 按集合判断存在性，不看出现次数：只要重写内容里出现过就算还在。
	present := make(map[string]struct{}, len(oldIdentities))
	for _, identity := range wikiWriteTableRowIdentities(rewritten) {
		present[identity] = struct{}{}
	}
	missing := make([]string, 0, 4)
	reported := make(map[string]struct{}, 4)
	for _, identity := range oldIdentities {
		if _, ok := present[identity]; ok {
			continue
		}
		// 同一标识可能出现在多行中；标识消失算丢失一个，不是丢失 N 行。
		if _, ok := reported[identity]; ok {
			continue
		}
		reported[identity] = struct{}{}
		missing = append(missing, identity)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return missing
}

// wikiWriteDroppedRowExamples 对丢失行列表做截断，用于日志输出。
func wikiWriteDroppedRowExamples(missing []string) []string {
	if len(missing) > wikiWriteGuardLogLimit {
		return missing[:wikiWriteGuardLogLimit]
	}
	return missing
}
