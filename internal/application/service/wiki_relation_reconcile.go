package service

import (
	"context"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// 实体关系（wiki_page_relations）对账。
//
// 背景：Wiki 页面在重新解析、去重合并、条款质检时会被软删除，新页面通常以新的
// slug 重新生成；而关系表只增不删，导致大量关系指向已删除页面。关系页面标题
// 解析因此命中失败、退化为英文 slug，点击跳转也返回 404。
//
// 对账规则：
//  1. 端点 slug 仍存活：关系保持不变。
//  2. 端点已删除，但存在同「页面类型 + 标题」的唯一存活页面：把端点改写到
//     新 slug（即等价的新页面）。
//  3. 找不到等价页面：删除该关系，避免关系列表出现死链。
//  4. 改写/删除后同一对端点出现重复关系时，只保留一条。

// wikiRelationEndpoint 是对账时使用的页面最小信息。
type wikiRelationEndpoint struct {
	Slug     string
	PageType string
	Title    string
}

// wikiRelationAction 描述一条关系对账后的动作。零值表示保持不变。
type wikiRelationAction struct {
	delete       bool
	update       bool
	sourceSlug   string
	targetSlug   string
	sourceType   string
	targetType   string
}

// reconcilePageRelations 对单个知识库的实体关系执行对账。
func reconcilePageRelations(ctx context.Context, db *gorm.DB, kbID string) error {
	if db == nil || kbID == "" {
		return nil
	}

	// 加载所有存活页面。
	var liveRows []wikiRelationEndpoint
	if err := db.WithContext(ctx).
		Model(&types.WikiPage{}).
		Select("slug", "page_type", "title").
		Where("knowledge_base_id = ? AND deleted_at IS NULL", kbID).
		Scan(&liveRows).Error; err != nil {
		return err
	}

	// 加载全部关系。
	var relations []types.WikiPageRelation
	if err := db.WithContext(ctx).
		Where("knowledge_base_id = ?", kbID).
		Find(&relations).Error; err != nil {
		return err
	}
	if len(relations) == 0 {
		return nil
	}

	// 找出关系引用、但已不在存活集合中的 slug，加载它们最近的软删除版本，
	// 以便按「页面类型 + 标题」找到替代页面。
	liveSet := make(map[string]struct{}, len(liveRows))
	for _, row := range liveRows {
		liveSet[row.Slug] = struct{}{}
	}
	deadSet := make(map[string]struct{})
	for _, rel := range relations {
		if _, ok := liveSet[rel.SourceSlug]; !ok {
			deadSet[rel.SourceSlug] = struct{}{}
		}
		if _, ok := liveSet[rel.TargetSlug]; !ok {
			deadSet[rel.TargetSlug] = struct{}{}
		}
	}

	var ghostRows []wikiRelationEndpoint
	if len(deadSet) > 0 {
		deadSlugs := make([]string, 0, len(deadSet))
		for slug := range deadSet {
			deadSlugs = append(deadSlugs, slug)
		}
		// 同一 slug 可能有多个历史版本，按创建时间倒序，取最近一条。
		if err := db.WithContext(ctx).
			Unscoped().
			Model(&types.WikiPage{}).
			Select("slug", "page_type", "title").
			Where("knowledge_base_id = ? AND slug IN ? AND deleted_at IS NOT NULL", kbID, deadSlugs).
			Order("created_at DESC").
			Scan(&ghostRows).Error; err != nil {
			return err
		}
	}

	actions := planRelationReconciliation(liveRows, ghostRows, relations)

	// 先执行删除，再执行改写：（知识库、源、目标）上存在唯一索引，
	// 若改写的目标对端与现存行重复，必须先把重复行删掉才能更新。
	var deleteIDs []string
	for i, action := range actions {
		if action.delete {
			deleteIDs = append(deleteIDs, relations[i].ID)
		}
	}
	if len(deleteIDs) > 0 {
		if err := db.WithContext(ctx).
			Where("id IN ?", deleteIDs).
			Delete(&types.WikiPageRelation{}).Error; err != nil {
			return err
		}
	}

	updated := 0
	for i, action := range actions {
		if !action.update {
			continue
		}
		if err := db.WithContext(ctx).
			Model(&types.WikiPageRelation{}).
			Where("id = ?", relations[i].ID).
			Updates(map[string]interface{}{
				"source_slug":      action.sourceSlug,
				"target_slug":      action.targetSlug,
				"source_page_type": action.sourceType,
				"target_page_type": action.targetType,
			}).Error; err != nil {
			return err
		}
		updated++
	}

	if len(deleteIDs) > 0 || updated > 0 {
		logger.Infof(ctx, "wiki relations: 知识库 %s 对账完成，改写 %d 条，删除 %d 条",
			kbID, updated, len(deleteIDs))
	}
	return nil
}

// planRelationReconciliation 根据存活页面、软删除页面和现有关系，计算每条关系
// 的对账动作。纯函数实现，便于单元测试。
func planRelationReconciliation(
	live []wikiRelationEndpoint,
	ghosts []wikiRelationEndpoint,
	relations []types.WikiPageRelation,
) []wikiRelationAction {
	actions := make([]wikiRelationAction, len(relations))

	// 存活 slug -> 页面信息。
	liveBySlug := make(map[string]wikiRelationEndpoint, len(live))
	// （页面类型 + 标题）-> slug，仅在唯一时保留映射。
	titleCount := make(map[string]int)
	for _, page := range live {
		liveBySlug[page.Slug] = page
		if page.Title != "" {
			titleCount[relationTitleKey(page.PageType, page.Title)]++
		}
	}
	liveByTitle := make(map[string]string)
	for _, page := range live {
		if page.Title == "" {
			continue
		}
		key := relationTitleKey(page.PageType, page.Title)
		if titleCount[key] == 1 {
			liveByTitle[key] = page.Slug
		}
	}

	// 软删除 slug -> 最近版本信息（ghosts 已按创建时间倒序，第一条为准）。
	ghostBySlug := make(map[string]wikiRelationEndpoint, len(ghosts))
	for _, page := range ghosts {
		if _, exists := ghostBySlug[page.Slug]; !exists {
			ghostBySlug[page.Slug] = page
		}
	}

	// resolve 把端点 slug 解析到存活 slug；无法解析时返回空串。
	resolve := func(slug string) (string, wikiRelationEndpoint, bool) {
		if page, ok := liveBySlug[slug]; ok {
			return slug, page, true
		}
		if ghost, ok := ghostBySlug[slug]; ok && ghost.Title != "" {
			if nextSlug, ok := liveByTitle[relationTitleKey(ghost.PageType, ghost.Title)]; ok {
				return nextSlug, liveBySlug[nextSlug], true
			}
		}
		return "", wikiRelationEndpoint{}, false
	}

	type resolvedPair struct {
		source string
		target string
	}
	resolved := make([]resolvedPair, len(relations))
	okFlags := make([]bool, len(relations))

	// 先解析全部关系，再按目标端点对分组，保证重复关系只保留一条。
	pairMembers := make(map[resolvedPair][]int)
	for i, rel := range relations {
		sourceSlug, _, sourceOK := resolve(rel.SourceSlug)
		targetSlug, _, targetOK := resolve(rel.TargetSlug)
		okFlags[i] = sourceOK && targetOK
		if !okFlags[i] {
			continue
		}
		pair := resolvedPair{source: sourceSlug, target: targetSlug}
		resolved[i] = pair
		pairMembers[pair] = append(pairMembers[pair], i)
	}

	for pair, members := range pairMembers {
		// 选择保留项：优先保留原本就是这对端点的关系，否则取第一条。
		keeper := -1
		for _, idx := range members {
			if relations[idx].SourceSlug == pair.source && relations[idx].TargetSlug == pair.target {
				keeper = idx
				break
			}
		}
		if keeper == -1 {
			keeper = members[0]
		}

		for _, idx := range members {
			switch {
			case idx != keeper:
				// 重复关系：删除。
				actions[idx].delete = true
			case relations[idx].SourceSlug != pair.source || relations[idx].TargetSlug != pair.target:
				// 保留项需要改写到等价的新页面。
				sourceSlug, sourcePage, _ := resolve(relations[idx].SourceSlug)
				targetSlug, targetPage, _ := resolve(relations[idx].TargetSlug)
				actions[idx] = wikiRelationAction{
					update:     true,
					sourceSlug: sourceSlug,
					targetSlug: targetSlug,
					sourceType: sourcePage.PageType,
					targetType: targetPage.PageType,
				}
			}
		}
	}

	// 无法解析到存活页面的关系：删除。
	for i, ok := range okFlags {
		if !ok {
			actions[i].delete = true
		}
	}

	return actions
}

// relationTitleKey 生成「页面类型 + 标题」的组合键。
func relationTitleKey(pageType, title string) string {
	return pageType + "\x00" + title
}
