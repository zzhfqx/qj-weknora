package types

// 画廊配置自下而上最多由四层合并，层级越高越具体。高层逐字段覆盖低层；某层
// 未提及的属性（或来源、维度）不会被它改动。
//
//	用户层  （每用户 JSON，users.preferences.gallery）       最高
//	知识库层（每知识库 JSON，知识库配置）                     预留
//	系统层  （平台 JSON，system_settings 的 “gallery.policy”）
//	来源层  （每个已注册属性来源自己声明的）                 最低
//
// 三个 JSON 层共用同一套 schema（GalleryPolicyTier），因此下面的合并引擎对层
// 不敏感，新的存储落点可以在不改动契约的情况下接入。只有用户层携带 mode/status
// ——搜索字段激活状态天生是个人的。
//
// 这里只配置属性的 USAGE（资格）；属性定义（名称 / 类型 / 取值 / 标签）完全来自
// 已注册来源，因此任何层都无法凭空造出一个管道从不产出的属性。

// GalleryUsageOverride 是字段级的 usage 覆盖。nil 字段表示“不改动低层取值”；
// 非 nil 字段则替换它。
type GalleryUsageOverride struct {
	InFilter      *bool `json:"in_filter,omitempty"`
	InSearchField *bool `json:"in_searchfield,omitempty"`
	InSortField   *bool `json:"in_sortfield,omitempty"`
}

// GalleryPolicyTier 是系统层、知识库层与用户层共用的统一 JSON schema。所有字段
// 都是可选的；空层即无操作。
type GalleryPolicyTier struct {
	// Sources 按来源 id 整体开关属性来源。提到某来源的最高层决定其状态；
	// 无人提及的来源默认启用。
	Sources map[string]bool `json:"sources,omitempty"`
	// Overrides 按命名空间属性 id（“system:contain.text”）逐属性调整 usage。
	// 覆盖逐字段叠加在低层解析结果之上。
	Overrides map[string]GalleryUsageOverride `json:"overrides,omitempty"`
	// Mode 是用户层的搜索激活模式：“all”（所有合格搜索字段都激活）或 “custom”
	// （逐字段 status 生效）。空表示 “all”。仅在用户层有意义。
	Mode string `json:"mode,omitempty"`
	// Status 是用户层逐属性搜索字段激活状态，按命名空间属性 id 记录，取值
	// “on”/“off”。仅在用户层有意义。
	Status map[string]string `json:"status,omitempty"`
}

// 画廊已解析配置的常量。
const (
	// GalleryModeAll 无视 status 激活所有搜索合格字段，为默认值，新用户开箱即用。
	// GalleryModeCustom 由逐字段 status 决定，未记录字段默认 off。
	GalleryModeAll    = "all"
	GalleryModeCustom = "custom"
	// GalleryStatusOn / GalleryStatusOff 是用户层 status 的取值。
	GalleryStatusOn  = "on"
	GalleryStatusOff = "off"
)

// GalleryResolvedAttr 是画廊契约下发给前端的一个属性：来源声明已合并各层覆盖
// 的结果。ID 为命名空间属性 id；Name 保留来源内名，用于在 image_info 中做裸键
// 查找。
type GalleryResolvedAttr struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	// Values 保留每个允许值的原始形式、要显示的短名与解释句。声明时未定义
	// 取值的来源（自由文本或日期属性）直接省略该字段。
	Values      []GalleryAttrValue `json:"values,omitempty"`
	Label       string             `json:"label"`
	Description string             `json:"description,omitempty"`
	Usage       GalleryUsage       `json:"usage"`
	// UsageFrom 标记最后改动该属性 usage 的最高层：“source” | “system” |
	// “kb” | “user”。用于排查“为什么这个属性出现/没出现”。
	UsageFrom string `json:"usage_from"`
}

// GalleryResolvedConfig 是合并后的画廊契约：启用了哪些来源、解析后的属性列表、
// 以及用户的搜索激活状态。
type GalleryResolvedConfig struct {
	// AttributeSources 按优先顺序列出启用中的来源 id。
	AttributeSources []string              `json:"attribute_sources"`
	Attributes       []GalleryResolvedAttr `json:"attributes"`
	// Mode 是用户的搜索激活模式（默认 “all”）。
	Mode string `json:"mode"`
	// Status 镜像用户逐字段的搜索开关；nil 表示用户尚未记录任何选择。
	Status map[string]string `json:"status"`
}

// UsageFrom 的层级标签，自低向高排列。
const (
	galleryFromSource = "source"
	galleryFromSystem = "system"
	galleryFromKB     = "kb"
	galleryFromUser   = "user"
)

// ResolveGalleryConfig 把已注册来源与配置层（系统 -> 知识库 -> 用户，逐层逐字段
// 覆盖）合并，返回画廊前端渲染所依据的契约。nil 层视为不存在；知识库层为按知识库
// 配置预留（尚未接线，引擎已接受）。
func ResolveGalleryConfig(kbID string, system, kb, user *GalleryPolicyTier) *GalleryResolvedConfig {
	res := &GalleryResolvedConfig{
		AttributeSources: []string{},
		Attributes:       []GalleryResolvedAttr{},
		Mode:             GalleryModeAll,
	}

	// 来源开关：提到该来源的最高层获胜。
	for _, src := range GalleryAttrSources(kbID) {
		if !gallerySourceEnabled(src.ID, system, kb, user) {
			continue
		}
		res.AttributeSources = append(res.AttributeSources, src.ID)
		for _, def := range src.Attrs {
			attr := GalleryResolvedAttr{
				ID:          GalleryAttrID(src.ID, def.Name),
				Source:      src.ID,
				Name:        def.Name,
				Type:        def.Type,
				Values:      def.Values,
				Label:       def.Label,
				Description: def.Description,
				Usage:       def.Usage,
				UsageFrom:   galleryFromSource,
			}
			galleryApplyTier(&attr, system, galleryFromSystem)
			galleryApplyTier(&attr, kb, galleryFromKB)
			galleryApplyTier(&attr, user, galleryFromUser)
			res.Attributes = append(res.Attributes, attr)
		}
	}

	// 用户层激活状态。Mode 经过校验；其他取值回退到全包含默认。
	if user != nil {
		if user.Mode == GalleryModeAll || user.Mode == GalleryModeCustom {
			res.Mode = user.Mode
		}
		if len(user.Status) > 0 {
			status := make(map[string]string, len(user.Status))
			for id, v := range user.Status {
				if v == GalleryStatusOn || v == GalleryStatusOff {
					status[id] = v
				}
			}
			if len(status) > 0 {
				res.Status = status
			}
		}
	}
	return res
}

// gallerySourceEnabled 报告某来源在各层下是否启用：用户 > 知识库 > 系统，未提及
// 的来源默认启用。
func gallerySourceEnabled(id string, tiers ...*GalleryPolicyTier) bool {
	for i := len(tiers) - 1; i >= 0; i-- {
		t := tiers[i]
		if t == nil {
			continue
		}
		if enabled, mentioned := t.Sources[id]; mentioned {
			return enabled
		}
	}
	return true
}

// galleryApplyTier 把某层对该属性的覆盖逐字段折叠进已解析的 usage；当它至少触及
// 一个字段时，把该层记入 UsageFrom。
func galleryApplyTier(attr *GalleryResolvedAttr, tier *GalleryPolicyTier, from string) {
	if tier == nil {
		return
	}
	if o, ok := tier.Overrides[attr.ID]; ok {
		applyGalleryOverride(&attr.Usage, &attr.UsageFrom, from, &o)
	}
}

// applyGalleryOverride 应用一条属性覆盖；返回该覆盖是否存在。
func applyGalleryOverride(usage *GalleryUsage, usageFrom *string, from string, o *GalleryUsageOverride) bool {
	if o == nil {
		return false
	}
	if o.InFilter != nil {
		usage.InFilter = *o.InFilter
		*usageFrom = from
	}
	if o.InSearchField != nil {
		usage.InSearchField = *o.InSearchField
		*usageFrom = from
	}
	if o.InSortField != nil {
		usage.InSortField = *o.InSortField
		*usageFrom = from
	}
	return true
}
