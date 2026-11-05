package repository

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// ListImageAssets 从 chunk_images 返回某个知识库的一页图片。chunk_images 是
// chunks.image_info 由数据库触发器（迁移 000215 / SQLite 000015）自动维护的
// 投影。同一张图片被复制到多个 chunk 时只列一次，取没有其他副本能胜过的那个
// 副本（见 winnerClause），由每条候选行上的一次索引探测判定。因此普通列表直接从
// listing 索引读出该页并停止；筛选先收窄候选，再做探测。
func (r *chunkRepository) ListImageAssets(
	ctx context.Context, tenantID uint64, kbID string, q *types.ImageAssetQuery,
) ([]types.ImageAssetRow, int64, error) {
	if q == nil {
		q = &types.ImageAssetQuery{}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	offset := max(q.Offset, 0)

	d := imageAssetDialect{postgres: r.db.Name() == "postgres"}
	where, args, filtered := d.where(tenantID, kbID, q)
	// 未筛选时总数即不同图片数，copies 索引可直接给出，无需逐行探测。
	count, countArgs := "SELECT COUNT(*) FROM chunk_images i WHERE "+where, args
	if !filtered {
		countArgs = args[:2]
		// 用子查询里的 DISTINCT，而不是 COUNT(DISTINCT)：后者总是排序，而前者
		// 按顺序走 copies 索引。
		count = "SELECT COUNT(*) FROM (SELECT DISTINCT i.image_key FROM chunk_images i " +
			"WHERE i.knowledge_base_id = ? AND i.tenant_id = ?) d"
	}
	dir := " ASC"
	if q.SortDesc {
		dir = " DESC"
	}
	// 并列时（同一 chunk 的所有图片共享其时间戳）跟随排序方向，因此按
	// created_at 列出就是沿索引的一次反向遍历。
	order := d.sortKey(q.SortField) + dir + ", i.chunk_id" + dir + ", i.image_index" + dir
	query := "SELECT i.chunk_id, i.knowledge_id, i.chunk_type, i.is_enabled, i.status, i.created_at, " +
		"i.updated_at, i.image_index, i.url, i.original_url, i.caption, i.ocr_text, " + d.attrsText() +
		" AS attrs_json, (" + count + ") AS total_count " +
		"FROM chunk_images i WHERE " + where + " ORDER BY " + order + " LIMIT ? OFFSET ?"

	var rows []types.ImageAssetRow
	pageArgs := make([]any, 0, len(countArgs)+len(args)+2)
	pageArgs = append(append(append(pageArgs, countArgs...), args...), limit, offset)
	if err := r.db.WithContext(ctx).Raw(query, pageArgs...).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	if len(rows) > 0 {
		return rows, rows[0].TotalCount, nil
	}
	if offset == 0 {
		return rows, 0, nil
	}
	// 越过末尾的页没有行可读总数，单独查一次。
	var total int64
	err := r.db.WithContext(ctx).Raw(count, countArgs...).Scan(&total).Error
	return rows, total, err
}

// winnerClause 每张图片只保留一个副本：最近更新的 chunk 上的那个，并列时按
// chunk id、再按数组位置决胜。copies 索引服务于该探测。
const winnerClause = "NOT EXISTS (SELECT 1 FROM chunk_images o " +
	"WHERE o.knowledge_base_id = i.knowledge_base_id AND o.tenant_id = i.tenant_id " +
	"AND o.image_key = i.image_key AND (o.updated_at > i.updated_at " +
	"OR (o.updated_at = i.updated_at AND (o.chunk_id < i.chunk_id " +
	"OR (o.chunk_id = i.chunk_id AND o.image_index < i.image_index)))))"

// scopedWinnerClause 把 winnerClause 的副本范围限定在所列文档内：范围外的副本
// 不得遮蔽某个同样携带该图片的文档。
const scopedWinnerClause = "NOT EXISTS (SELECT 1 FROM chunk_images o " +
	"WHERE o.knowledge_base_id = i.knowledge_base_id AND o.tenant_id = i.tenant_id " +
	"AND o.image_key = i.image_key AND o.knowledge_id IN ? AND (o.updated_at > i.updated_at " +
	"OR (o.updated_at = i.updated_at AND (o.chunk_id < i.chunk_id " +
	"OR (o.chunk_id = i.chunk_id AND o.image_index < i.image_index)))))"

// imageAssetDialect 承载 postgres 与 sqlite 写法不同的表达式：读取 attrs JSON
// 与匹配关键字。
type imageAssetDialect struct {
	postgres bool
}

// where 是选出查询图片的谓词：知识库内每张图片的“所列副本”，且通过筛选。筛选
// 只评判所列副本，仿佛其他副本不存在。每个子句都写成非 NULL 形态，因此裁决子句
// 里的 NOT 不会把未观察的图片变成隐藏图片。filtered 报告除知识库范围外是否还有
// 其他约束。
func (d imageAssetDialect) where(
	tenantID uint64, kbID string, q *types.ImageAssetQuery,
) (sql string, args []any, filtered bool) {
	clauses := []string{"i.knowledge_base_id = ?", "i.tenant_id = ?"}
	args = []any{kbID, tenantID}
	if q.IsEnabled != nil {
		clauses = append(clauses, "i.is_enabled = ?")
		args = append(args, *q.IsEnabled)
	}
	if len(q.KnowledgeIDs) > 0 {
		clauses = append(clauses, "i.knowledge_id IN ?")
		args = append(args, q.KnowledgeIDs)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		pattern := "%" + escapeLikeKeyword(kw) + "%"
		var matches []string
		for _, f := range q.SearchFields {
			expr, ok := d.value(f)
			if !ok {
				continue
			}
			if d.postgres {
				// ILIKE 配合默认反斜杠转义，正是 caption / ocr_text 上
				// trigram 索引可以服务的形态。
				matches = append(matches, expr+" ILIKE ?")
			} else {
				// sqlite 的 LIKE 只折叠 ASCII 大小写，非 ASCII 关键字在那里
				// 按大小写敏感匹配。
				matches = append(matches, expr+" LIKE ? ESCAPE '"+likeEscapeChar+"'")
			}
			args = append(args, pattern)
		}
		if len(matches) == 0 {
			matches = []string{"1 = 0"}
		}
		clauses = append(clauses, "("+strings.Join(matches, " OR ")+")")
	}
	for _, set := range q.AttrFilters {
		expr, ok := d.value(set.Field)
		if !ok || len(set.Values) == 0 {
			continue
		}
		// 从未在该属性上被观察的图片不受其约束：缺省是沉默，不是不匹配。
		clauses = append(clauses, "("+expr+" IS NULL OR "+expr+" IN ?)")
		args = append(args, set.Values)
	}
	if off, offArgs := d.anyCarries(q.OffRules); off != "" {
		// “off” 隐藏携带其值的图片，除非某条 “on” 认领同一张图：强制显示
		// 胜过强制隐藏。
		on, onArgs := d.anyCarries(q.OnRules)
		if on == "" {
			on = "1 = 0"
		}
		clauses = append(clauses, "(("+on+") OR NOT ("+off+"))")
		args = append(append(args, onArgs...), offArgs...)
	}
	filtered = len(clauses) > 2
	winner := winnerClause
	if len(q.KnowledgeIDs) > 0 {
		winner = scopedWinnerClause
		args = append(args, q.KnowledgeIDs)
	}
	return strings.Join(append(clauses, winner), " AND "), args, filtered
}

// anyCarries 在图片携带任一集合的任一取值时为真。某字段无值的图片永不匹配。
func (d imageAssetDialect) anyCarries(sets []types.ImageAssetValueSet) (string, []any) {
	var clauses []string
	var args []any
	for _, set := range sets {
		expr, ok := d.value(set.Field)
		if !ok || len(set.Values) == 0 {
			continue
		}
		clauses = append(clauses, "("+expr+" IS NOT NULL AND "+expr+" IN ?)")
		args = append(args, set.Values)
	}
	return strings.Join(clauses, " OR "), args
}

// sortKey 是图片排序依据。某字段无值的图片按空串排序。
func (d imageAssetDialect) sortKey(f types.ImageAssetField) string {
	switch f.Builtin {
	case "created_at", "updated_at", "is_enabled":
		return "i." + f.Builtin
	}
	if expr, ok := d.value(f); ok {
		return "LOWER(COALESCE(" + expr + ", ''))"
	}
	return "i.created_at"
}

// value 取一张图片某字段的归一化文本，无值时为 NULL。布尔值在两种后端都读作
// “true”/“false”，与画廊契约列出取值的形式一致。时间戳仅供排序：没有可搜索、
// 可筛选的文本形态。
func (d imageAssetDialect) value(f types.ImageAssetField) (string, bool) {
	switch f.Builtin {
	case "":
	case "caption", "ocr_text":
		return "i." + f.Builtin, true
	case "is_enabled":
		return "(CASE WHEN i.is_enabled THEN 'true' ELSE 'false' END)", true
	default:
		return "", false
	}
	name := f.Attr
	// 属性名以字面量嵌入（上游已按画廊契约校验）；拒绝任何可能脱离 JSON 路径的
	// 内容。
	if name == "" || strings.ContainsAny(name, `'"\`) {
		return "", false
	}
	if d.postgres {
		return "(i.attrs->>'" + name + "')", true
	}
	path := `'$."` + name + `"'`
	return "(CASE json_type(i.attrs, " + path + ") WHEN 'true' THEN 'true' WHEN 'false' THEN 'false' " +
		"ELSE CAST(json_extract(i.attrs, " + path + ") AS TEXT) END)", true
}

// attrsText 把 attrs 列以 JSON 文本形式读回。
func (d imageAssetDialect) attrsText() string {
	if d.postgres {
		return "i.attrs::text"
	}
	return "i.attrs"
}
