package service

import (
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// 画廊属性的管道装配
//
// 画廊以命名空间 id（“<来源ID>:<名称>”）寻址每个属性。builtin 属性映射到资产
// 自身的字段；其他来源都从 image_info 中存储的通用属性集合读取。仓储层在 SQL
// 中一并求值。这样画廊与任何具体属性管道解耦：无论观察功能是否部署，它都能
// 编译并运行；新来源接入时这里也无需改动。
// ---------------------------------------------------------------------------

// galleryBuiltinSourceID 是 types 包注册的 builtin 属性来源 id；其取值来自资产
// 字段而非 image_info。
const galleryBuiltinSourceID = "builtin"

// galleryDefaultSearchFields 是请求未指定搜索字段时使用的字段集（旧客户端、
// API 调用方）。
var galleryDefaultSearchFields = []string{
	types.GalleryAttrID(galleryBuiltinSourceID, "caption"),
	types.GalleryAttrID(galleryBuiltinSourceID, "ocr_text"),
}

// galleryField 把命名空间画廊属性 id 映射到仓储查询读取的字段：builtin id
// 指向资产自身的列，其他来源则按来源内名读取 image_info 的 attrs 集合。
func galleryField(id string) (types.ImageAssetField, bool) {
	source, name, ok := types.SplitGalleryAttrID(id)
	if !ok {
		return types.ImageAssetField{}, false
	}
	if source == galleryBuiltinSourceID {
		return types.ImageAssetField{Builtin: name}, true
	}
	return types.ImageAssetField{Attr: name}, true
}

// buildImageAssetQuery 把画廊请求翻译成存储中立的查询，由仓储编译成 SQL。map
// 遍历经过排序，使同一请求总是产生相同的语句。
func buildImageAssetQuery(filter *types.ImageListFilter, page *types.Pagination) *types.ImageAssetQuery {
	q := &types.ImageAssetQuery{
		SortField: types.ImageAssetField{Builtin: "created_at"},
		SortDesc:  true,
		Offset:    page.Offset(),
		Limit:     page.GetPageSize(),
	}
	if filter == nil {
		return q
	}
	q.Keyword = strings.TrimSpace(filter.Keyword)
	q.IsEnabled = filter.IsEnabled
	if len(filter.KnowledgeIDs) > 0 {
		q.KnowledgeIDs = filter.KnowledgeIDs
	}

	searchIn := filter.SearchIn
	if len(searchIn) == 0 {
		searchIn = galleryDefaultSearchFields
	}
	for _, id := range searchIn {
		if f, ok := galleryField(id); ok {
			q.SearchFields = append(q.SearchFields, f)
		}
	}

	for _, id := range sortedKeys(filter.AttrFilters) {
		f, ok := galleryField(id)
		if !ok {
			continue
		}
		var values []string
		for _, v := range filter.AttrFilters[id] {
			values = append(values, strings.TrimSpace(v))
		}
		if len(values) > 0 {
			q.AttrFilters = append(q.AttrFilters, types.ImageAssetValueSet{Field: f, Values: values})
		}
	}

	for _, id := range sortedKeys(filter.AttrRules) {
		f, ok := galleryField(id)
		if !ok {
			continue
		}
		var on, off []string
		verdicts := filter.AttrRules[id]
		for _, value := range sortedKeys(verdicts) {
			switch verdicts[value] {
			case galleryRuleOn:
				on = append(on, value)
			case galleryRuleOff:
				off = append(off, value)
			}
		}
		if len(on) > 0 {
			q.OnRules = append(q.OnRules, types.ImageAssetValueSet{Field: f, Values: on})
		}
		if len(off) > 0 {
			q.OffRules = append(q.OffRules, types.ImageAssetValueSet{Field: f, Values: off})
		}
	}

	switch filter.SortBy {
	case "":
	case "created_at", "updated_at", "caption":
		// 旧的裸字段名映射到对应 builtin id。
		q.SortField = types.ImageAssetField{Builtin: filter.SortBy}
	default:
		if f, ok := galleryField(filter.SortBy); ok {
			q.SortField = f
		}
	}
	q.SortDesc = filter.SortOrder != "asc"
	return q
}

// 属性规则可携带的两种裁决。
const (
	galleryRuleOff = "off"
	galleryRuleOn  = "on"
)

// sortedKeys 返回 map 键的排序切片，保证同一请求产生同一语句。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
