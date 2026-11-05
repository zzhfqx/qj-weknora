package types

import "strings"

// AttrType 描述一个被观察属性值的形态。
type AttrType string

const (
	// AttrTypeExtent 是一组有序取值，例如 none < sparse < block。
	AttrTypeExtent AttrType = "extent"
	// AttrTypePresence 是一个布尔观察结果，例如 true / false。
	AttrTypePresence AttrType = "presence"
)

// AttrSpec 是一个可被观察的图片属性。注册表是本版本「有哪些属性」的唯一事实
// 来源：新增属性只需在此加一行再补一条决策子句，无需改动其他结构体。正因如此，
// 「加一个属性 = 加一行」才是真正低风险的操作。同一行还携带该属性的展示文案。
// 向非技术运营解释这些属性的设置面板正是由这些字段渲染的，所以新增属性也不需要
// 改前端：Label、Description 以及每个取值的文案都会经 schema 端点下发到 UI。
// 每个属性都同时配有用于紧凑空间的短标签和用于宽松位置的整句描述。
type AttrSpec struct {
	Name        string      `json:"name"`                  // 稳定键，例如 "contain.text"
	Type        AttrType    `json:"type"`                  // extent | presence
	Values      []AttrValue `json:"values,omitempty"`      // extent 属性按 低->高 有序排列
	Question    string      `json:"question"`              // 在描述轮次中向模型展示的问句
	Label       string      `json:"label"`                 // 设置面板用的可读名称
	Description string      `json:"description,omitempty"` // 一行说明该属性度量什么
	Consumers   []string    `json:"consumers,omitempty"`   // 读取该属性的下游步骤
}

// AttrValue 是属性的一个允许取值：原始值加上为其展示的文字。
//
// 展示文案刻意拆成两段。Label 是能塞进筛选复选框或表格单元格的短名；
// Description 是在有空间的地方（ tooltip、设置面板）解释该取值的整句。
// 两者都用项目默认语言书写；前端在其上叠加自己的翻译（见 web 端的
// imageAttrDisplay），未翻译的部分回落到这里的字符串。
type AttrValue struct {
	Value       string `json:"value"`                 // 原始机器值，永不本地化
	Label       string `json:"label"`                 // 屏幕短名
	Description string `json:"description,omitempty"` // 解释该取值的整句
}

// ImageAttrRegistry 是本版本被观察属性的规范、有序列表。当前启用两个：
//   - contain.text 通过其 "block" 取值驱动 OCR 决策；
//   - contain.data_visual 让图表（稀疏的坐标轴标签）留在 OCR 路径上，
//     而带稀疏路牌的照片则离开该路径，保持与旧七分类模型等价。
var ImageAttrRegistry = []AttrSpec{
	{
		Name: "contain.text",
		Type: AttrTypeExtent,
		Values: []AttrValue{
			{Value: "none", Label: "None", Description: "no text at all"},
			{Value: "sparse", Label: "Sparse", Description: "a few words — a logo, a road sign, a single label"},
			{Value: "block", Label: "Block", Description: "a block of body text: screenshot, table, document page"},
		},
		Question: "How much body text does the image carry? Answer exactly one of: " +
			"none (no text), sparse (a few words such as a logo or a road sign), " +
			"block (a block of body text such as a screenshot, table, or document page).",
		Label: "Text in the image",
		Description: "How much body text the picture itself carries. " +
			"Decides whether reading its text is worth a separate OCR pass.",
		Consumers: []string{"ocr"},
	},
	{
		Name: "contain.data_visual",
		Type: AttrTypePresence,
		Values: []AttrValue{
			{Value: "true", Label: "Yes", Description: "a chart, graph or diagram with plotted values"},
			{Value: "false", Label: "No", Description: "a photo, drawing, icon or decoration"},
		},
		Question: "Does the image present quantitative data as a chart, graph, diagram, or infographic " +
			"(axis labels, legends, plotted values)? Answer exactly one of: true, false.",
		Label: "Data visual",
		Description: "Whether the picture conveys data as a chart, graph, diagram or infographic. " +
			"Such images keep their labels on the OCR path even when the text looks sparse.",
		Consumers: []string{"ocr"},
	},
}

// ImageAttrSchemaVersion 标记 ImageAttrs.Attrs 的形态与语义。它是 attrs/2，
// 因为未被观察的属性不再被写成保守默认值：缺失的键现在表示「观察结果没有就该
// 属性作答」，并经 OCR 策略的 OnUnobserved 子句读取，因此存储观察结果的消费方
// 不能假设每个已注册键都存在。当某个属性的语义发生变化时才提升它，单纯新增
// 属性不提升。
const ImageAttrSchemaVersion = "attrs/2"

// ImageAttrSource 记录一次观察是如何产生的，以便拆分后的数据按来源分桶，
// 评估时不混淆不同来源。
type ImageAttrSource struct {
	Schema string `json:"schema"`           // ImageAttrSchemaVersion
	Prompt string `json:"prompt"`           // 提示指纹（版本 + 是否追加了自定义指令）
	Custom bool   `json:"custom,omitempty"` // 若描述提示携带了用户自定义指令则为 true
}

// ImageAttrs 是持久化的观察结果。Attrs 是 map 而非固定结构体，因此新增属性
// 永远不会破坏存储或破坏该属性出现之前写入的行。它以 JSON 形式存于
// chunks.image_info，无需迁移。
type ImageAttrs struct {
	Schema string          `json:"schema"`
	Source ImageAttrSource `json:"source"`
	Attrs  map[string]any  `json:"attrs,omitempty"`
}

// Observed 报告某次观察中是否出现了某个属性键。
func (a ImageAttrs) Observed(name string) bool {
	if a.Attrs == nil {
		return false
	}
	_, ok := a.Attrs[name]
	return ok
}

// TextExtent 返回观察到的 contain.text 取值；当该属性未被观察时返回 ""。
// 用 Observed("contain.text") 可区分「属性缺失」与「模型作答了」；
// "" 从来不是合法的观察值。
func (a ImageAttrs) TextExtent() string {
	if a.Attrs == nil {
		return ""
	}
	if v, ok := a.Attrs["contain.text"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// DataVisual 返回观察到的 contain.data_visual 取值；当该属性未被观察时返回
// false。缺失不等于观察到的 "false" —— 用 Observed 区分二者，DecideOCR
// 正是这么做的。
func (a ImageAttrs) DataVisual() bool {
	if a.Attrs == nil {
		return false
	}
	if v, ok := a.Attrs["contain.data_visual"]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// ImageActionsConfig 是「属性 -> 工作策略」。它以下游动作（目前只有 "ocr"）
// 为键；每个动作列出将其开启的属性条件，外加驱动属性未被观察时采用的保守值。
type ImageActionsConfig struct {
	OCR ImageOCRAction `json:"ocr"`
}

// ImageOCRAction 是 image_actions 策略的 OCR 子句。
type ImageOCRAction struct {
	// On 是开启 OCR 的属性条件列表。命中其中任意一条即开启 OCR。
	On []ImageAttrCondition `json:"on"`
	// OnUnobserved 是当观察结果没有就策略读取的某个属性作答时采用的 OCR 决策
	// ——该属性缺失，可能因为模型跳过了该行，或因为它写下的值不在允许集合内。
	// 默认 true：不完整的观察仍继续抽取，因此漏掉一段正文只多花一次调用，而不
	// 会丢失。只有当模型可靠到能回答每一个属性行时才关掉它。
	OnUnobserved bool `json:"on_unobserved"`
}

// ImageAttrCondition 是一条属性匹配：attr Prop 等于 Is。
type ImageAttrCondition struct {
	Prop string `json:"prop"`
	Is   string `json:"is"`
}

// DefaultImageActions 是内置的「属性 -> 工作」表。它刻意保守：当 contain.text
// 为 "block"、或 contain.data_visual 为 true、或观察结果没有就策略读取的某属性
// 作答（OnUnobserved 子句）时跑 OCR。它与旧的分类表等价：decorative / logo /
// photo（文字可靠地缺失或已被描述捕获）跳过 OCR；text_screenshot / table /
// chart / other 跑 OCR。
func DefaultImageActions() ImageActionsConfig {
	return ImageActionsConfig{
		OCR: ImageOCRAction{
			On: []ImageAttrCondition{
				{Prop: "contain.text", Is: "block"},
				{Prop: "contain.data_visual", Is: "true"},
			},
			OnUnobserved: true,
		},
	}
}

// MergeImageActions 把某个知识库的自定义动作表按动作键叠加到内置表之上。自定义
// 的 OCR.On 整体替换默认的 OCR.On（"on" 列表是一个整体）；自定义表未提及的部分
// 保留默认值。这让知识库可以调 OCR 而不必罗列每条条件，也让未来加到默认值的
// 动作键不会被旧的已存储配置抹掉。
func MergeImageActions(custom ImageActionsConfig) ImageActionsConfig {
	merged := DefaultImageActions()
	if len(custom.OCR.On) > 0 {
		merged.OCR = custom.OCR
	}
	return merged
}

// ResolveImageActions 返回生效策略：nil 配置用默认值；非 nil 配置被合并，
// 从而前向兼容。
func ResolveImageActions(p *ImageActionsConfig) ImageActionsConfig {
	if p == nil {
		return DefaultImageActions()
	}
	return MergeImageActions(*p)
}

// ImageAttrPromptVersion 是描述轮观察提示的指纹。它记录在 ImageAttrSource.Prompt
// 中，以便评估能区分不同提示版本产出的观察。
const ImageAttrPromptVersion = "observe/1"

// AttrObservation 是解析后的「观察并描述」应答：被观察的属性 + 为图片写下的描述。
type AttrObservation struct {
	Attrs       ImageAttrs
	Description string
	// Observed 报告模型是否返回了至少一条格式良好、取值可用的属性行。为 false
	// 时完全没有观察到任何属性，观察轮次实际上什么也没产出 —— 调用方把这一点
	// 镜像到 trace（observation_failed），让一个带偏属性协议的提示可见，而不是
	// 冒充一次自信的读取。它只是运行时信号，永不持久化：只有 Attrs 随块存储。
	Observed bool
}

// ParseImageAttrsResponse 把「观察并描述」应答拆成属性行与 DESCRIPTION。那个
// 布尔值报告应答是否产出了值得存储的描述；false 表示图片最终有属性但无 caption，
// 调用方会记录这一点，以免把缺失的 caption 误读为观察失败。
//
// 只有完全没有文字的应答、或全是属性行的应答才产出 false。跳过标签的散文会被
// 保留为描述：模型回答了问题，只是没有按格式作答。
//
// 应答没有写出的属性 —— 可能因为模型跳过了该行，或因为它写的值不在允许集合内
// —— 直接从 Attrs 缺席。不会在原地编造任何东西：在那里写保守默认值会让一个未
// 回答的问题看起来像自信的 "block"，并让策略的 OnUnobserved 子句不可达。正是
// 这种缺席被 DecideOCR 转成保守决策。
func ParseImageAttrsResponse(raw string) (AttrObservation, bool) {
	obs := AttrObservation{
		Attrs: ImageAttrs{
			Schema: ImageAttrSchemaVersion,
			Source: ImageAttrSource{Schema: ImageAttrSchemaVersion, Prompt: ImageAttrPromptVersion},
			Attrs:  map[string]any{},
		},
	}
	seen := map[string]bool{}
	var descriptions []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := trimAttrMarkers(line)
		if key, val, ok := cutAttrLine(trimmed); ok {
			if spec, found := lookupAttrSpec(key); found {
				// 不可用的值被丢弃而非设默认，因此该属性保持缺席，
				// 由策略的 OnUnobserved 子句决定如何处置。
				if v, usable := coerceAttrValue(spec, val); usable {
					obs.Attrs.Attrs[spec.Name] = v
					seen[spec.Name] = true
				}
				continue
			}
			if strings.EqualFold(key, "DESCRIPTION") {
				descriptions = append(descriptions, val)
				continue
			}
		}
		if strings.EqualFold(trimmed, "DESCRIPTION") {
			continue
		}
		if trimmed != "" {
			descriptions = append(descriptions, trimmed)
		}
	}
	// Observed 记录模型是否发出了任何格式良好的属性行。纯散文或空应答产出 false；
	// 这类回复被保留为描述但不带观察，因此调用方必须知道属性表对该图片什么也没说。
	obs.Observed = len(seen) > 0
	joined := strings.TrimSpace(strings.Join(descriptions, " "))
	// 模型可能给描述加了列表前缀；去掉它，让 caption 以散文开头。
	obs.Description = strings.TrimSpace(strings.TrimLeft(joined, "-*# "))
	return obs, obs.Description != ""
}

// lookupAttrSpec 把模型写下的键（例如 "CONTAIN.TEXT"、"contain_text"）
// 匹配到注册表中的某个属性。
func lookupAttrSpec(key string) (AttrSpec, bool) {
	norm := strings.ToLower(strings.TrimSpace(key))
	norm = strings.ReplaceAll(norm, " ", "_")
	norm = strings.ReplaceAll(norm, "-", "_")
	// 模型会给键套上 markdown 强调（"**contain.text**"）；去掉它，
	// 让尾部标记永远不会破坏匹配。
	norm = strings.ReplaceAll(norm, "*", "")
	norm = strings.ReplaceAll(norm, "#", "")
	for _, spec := range ImageAttrRegistry {
		if spec.Name == norm || strings.ReplaceAll(spec.Name, ".", "_") == norm {
			return spec, true
		}
	}
	return AttrSpec{}, false
}

// coerceAttrValue 把一个原始值对照属性的允许集合做校验。当模型写下了不可用的
// 东西 —— 空值或不在允许集合内的值 —— 时报告 false，调用方随后让该属性保持缺席，
// 而不是替换默认值。
func coerceAttrValue(spec AttrSpec, raw string) (any, bool) {
	v := strings.TrimSpace(strings.ToLower(raw))
	// 模型可能强调取值（"**block**"）；匹配前先去掉。
	v = strings.ReplaceAll(v, "*", "")
	v = strings.ReplaceAll(v, "#", "")
	switch spec.Type {
	case AttrTypeExtent:
		for _, allowed := range spec.Values {
			if v == allowed.Value {
				return allowed.Value, true
			}
		}
	case AttrTypePresence:
		switch v {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return nil, false
}

// cutAttrLine 返回一条 "KEY: value" 行的键与值。
func cutAttrLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

// trimAttrMarkers 去掉模型裹在属性行外的 markdown 强调与列表标记。
func trimAttrMarkers(line string) string {
	return strings.TrimLeft(line, "*_#- \t")
}
