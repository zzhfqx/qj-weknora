package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// relationForTest 构造一条用于测试的关系。
func relationForTest(id, source, target, sourceType, targetType string) types.WikiPageRelation {
	return types.WikiPageRelation{
		ID:             id,
		SourceSlug:     source,
		TargetSlug:     target,
		SourcePageType: sourceType,
		TargetPageType: targetType,
	}
}

func TestPlanRelationReconciliation(t *testing.T) {
	live := []wikiRelationEndpoint{
		{Slug: "entity/new-company", PageType: "entity", Title: "人身保险公司"},
		{Slug: "entity/accident-insurance", PageType: "entity", Title: "意外伤害保险"},
		{Slug: "entity/unique-only", PageType: "entity", Title: "唯一标题"},
	}
	// ghosts 按创建时间倒序：同 slug 第一条即最近版本。
	ghosts := []wikiRelationEndpoint{
		{Slug: "entity/old-company", PageType: "entity", Title: "人身保险公司"},
		{Slug: "entity/old-company-older", PageType: "entity", Title: "人身保险公司"},
		{Slug: "entity/gone-forever", PageType: "entity", Title: "已经消失"},
	}
	relations := []types.WikiPageRelation{
		// 旧端点改写到同标题的唯一存活页面。
		relationForTest("r1", "entity/old-company", "entity/accident-insurance", "entity", "entity"),
		// 两端都改写。
		relationForTest("r2", "entity/old-company", "entity/old-company-older", "entity", "entity"),
		// 无等价存活页面：删除。
		relationForTest("r3", "entity/gone-forever", "entity/accident-insurance", "entity", "entity"),
		// 已存活且不变：保持。
		relationForTest("r4", "entity/new-company", "entity/accident-insurance", "entity", "entity"),
		// 与 r1 改写后重复：r1 被删除（r4 原本就是这对端点）。
		relationForTest("r5", "entity/old-company", "entity/accident-insurance", "entity", "entity"),
	}

	actions := planRelationReconciliation(live, ghosts, relations)

	if actions[0].delete != true {
		t.Fatalf("r1 与 r4 重复，应删除 r1，实际: %+v", actions[0])
	}

	if !actions[1].update ||
		actions[1].sourceSlug != "entity/new-company" ||
		actions[1].targetSlug != "entity/new-company" ||
		actions[1].sourceType != "entity" {
		t.Fatalf("r2 两端都应改写到 entity/new-company，实际: %+v", actions[1])
	}

	if actions[2].delete != true {
		t.Fatalf("r3 无等价存活页面，应删除，实际: %+v", actions[2])
	}

	if actions[3].delete || actions[3].update {
		t.Fatalf("r4 已存活且不变，应保持，实际: %+v", actions[3])
	}

	if actions[4].delete != true {
		t.Fatalf("r5 与 r4 重复，应删除 r5，实际: %+v", actions[4])
	}
}

func TestPlanRelationReconciliationAmbiguousTitle(t *testing.T) {
	// 同类型同标题有两个存活页面：不可作为唯一映射，旧端点解析失败。
	live := []wikiRelationEndpoint{
		{Slug: "entity/a", PageType: "entity", Title: "歧义标题"},
		{Slug: "entity/b", PageType: "entity", Title: "歧义标题"},
		{Slug: "entity/accident-insurance", PageType: "entity", Title: "意外伤害保险"},
	}
	ghosts := []wikiRelationEndpoint{
		{Slug: "entity/old", PageType: "entity", Title: "歧义标题"},
	}
	relations := []types.WikiPageRelation{
		relationForTest("r1", "entity/old", "entity/accident-insurance", "entity", "entity"),
	}

	actions := planRelationReconciliation(live, ghosts, relations)
	if actions[0].delete != true {
		t.Fatalf("同标题存在多个存活页面时不应改写，应删除死链，实际: %+v", actions[0])
	}
}
