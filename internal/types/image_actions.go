package types

// 本文件是图片管线层的词汇表：一条管线可运行的动作 id、一个动作的声明形态、
// 以及管线自身的 id。一个动作*做什么*由 application/service 中的动作注册表
// （image_action_registry.go）绑定 —— 把两半分开，正是本包不依赖任何传输层
// 的原因。

// ImageActionID 是图片动作的派发键。管线按此 id 引用一个动作；该 id 实际运行
// 哪段代码由注册表决定，因此新增动作只是在此加一条再加一个绑定，而不是在某个
// 循环里加新分支。
type ImageActionID string

const (
	// ImageActionCaption 向模型索要图片的简短描述，并把它存为 caption 槽位。
	ImageActionCaption ImageActionID = "caption"
	// ImageActionObservationCaption 观察已注册属性
	// （contain.text、contain.data_visual……）并在同一应答中描述图片。一次 VLM
	// 请求同时填满属性槽与 caption 槽，这就是为什么框架里没有独立的观察动作、
	// 也没有任何「把观察与 caption 合并」的规则。
	ImageActionObservationCaption ImageActionID = "observation_caption"
	// ImageActionOCR 在策略需要时抽取图片承载的文字。
	ImageActionOCR ImageActionID = "ocr"
)

// ImageActionImpl 记录一个动作如何干活。这个标记不是装饰：一个调用模型去检查
// 已有值的动作，其实已经从「检查」重新变成「生成」了。
type ImageActionImpl string

const (
	// ImageActionImplPure 不调用模型即可运行。
	ImageActionImplPure ImageActionImpl = "pure"
	// ImageActionImplVLM 花费一次或多次模型请求。
	ImageActionImplVLM ImageActionImpl = "vlm"
)

// ImageActionSpec 是一个动作的声明半部分：它被允许写什么、声明要读什么、以及
// 设置面板可暴露哪些旋钮。可执行的半部分由注册表绑定。
type ImageActionSpec struct {
	// ID 在管线内派发该动作；也是管线引用它、以及画廊和设置面板命名它的方式。
	ID ImageActionID
	// Impl 说明该动作是否可以调用模型。
	Impl ImageActionImpl
	// Writes 列出该动作填充的输出槽位；nil 表示它不写任何槽。一条管线的两个
	// 动作可能刻意写同一个槽 —— iterate 与 validate 形态都会覆盖它们刚读到的
	// 东西 —— 因此这里刻意不断言唯一性。
	Writes []string
	// Reads 列出该动作消费的槽位。仅声明：它产出 trace 输出，并不 ordering 管线。
	Reads []string
	// Version 对该动作编码的提示与规则做指纹，因此一行 trace 可回溯到产出某张
	// 图片的修订版，而不仅是运行过的那条管线。
	Version string
	// Fields 是设置面板渲染的逐动作可调项。
	Fields []ImageFieldDef
}

// ImageFieldDef 是设置面板所见的一个动作可调项。它的 json 标签用 snake_case，
// 与图片 API 其余部分（包括外层的 ImagePipelineSpec）保持一致。
type ImageFieldDef struct {
	// Key 是动作以 r.Param(key) 读取的名字。
	Key string `json:"key"`
	// Type 选择面板渲染的控件。
	Type ImageFieldType `json:"type"`
	// Label 与 Description 用项目默认语言书写；前端叠加自己的翻译并回落到
	// 这里。
	Label       string `json:"label"`
	Description string `json:"description"`
	// Default 在知识库未覆盖该字段时生效。
	Default any `json:"default"`
	// Options 枚举一个 enum 字段的允许值。
	Options []string `json:"options"`
	// DecidesAction 标记一个会开启或关闭管线某个动作的字段，区别于那些只是
	// 调校管线总会发起的调用的字段。只有这类字段可能让一条管线无事可做，也只有
	// 这类字段会被 ImagePipelineRules 的规则点名。
	DecidesAction bool `json:"decides_action"`
}

// ImageFieldType 是一个可调项渲染成的控件。
type ImageFieldType string

const (
	// ImageFieldTypeBool 渲染为开关或复选框。
	ImageFieldTypeBool ImageFieldType = "bool"
	// ImageFieldTypeString 渲染为文本输入。
	ImageFieldTypeString ImageFieldType = "string"
	// ImageFieldTypeEnum 渲染为对该字段 Options 的下拉选择。
	ImageFieldTypeEnum ImageFieldType = "enum"
)

// SharedImageActions 是每条管线都从中起步的动作。它们在此声明，由注册表绑定到
// 处理器；只属于单条管线的动作则以 "<pipeline_id>.<action>" id 注册。
//
// 在提示移入动作并能在那里做指纹之前，Version 携带占位修订 "v1"。
var SharedImageActions = []ImageActionSpec{
	{
		ID:      ImageActionCaption,
		Impl:    ImageActionImplVLM,
		Writes:  []string{"caption"},
		Version: "v1",
	},
	{
		ID:     ImageActionObservationCaption,
		Impl:   ImageActionImplVLM,
		Writes: []string{"attrs", "caption"},
		// 观察读取图片与已注册属性表；caption 是同一请求的副产品。
		Reads:   []string{"attrs"},
		Version: "v1",
	},
	{
		ID:     ImageActionOCR,
		Impl:   ImageActionImplVLM,
		Writes: []string{"ocr_text"},
		// OCR 提示刻意不由知识库自定义指令构建，因此它不读另两个动作读的
		// 那个槽位。见 buildVLMOCRPrompt。
		Version: "v1",
	},
}

// ImagePipelineID 标记是哪条图片管线产出了一次运行。它记录在逐图片子跨度的
// 输入上，因为单看存储的 image_info 无法区分「管线观察了属性然后跳过 OCR」的
// 图片与「管线什么也没观察」的图片。
type ImagePipelineID string

const (
	// ImagePipelineSmartOCR 先观察已注册属性、在同一应答中写 caption，然后仅在
	// 属性策略允许时才 OCR。该 id 承诺观察先于决策；它不承诺这三个动作就是全部。
	ImagePipelineSmartOCR ImagePipelineID = "smartocr"
	// ImagePipelineDefault 是手动路径：用户挑选哪些解析动作运行，每个动作对每张
	// 图片都跑，无观察、无策略。它是任何其他东西都未被选中时的兜底。
	ImagePipelineDefault ImagePipelineID = "default"
	// ImagePipelineLegacyObCapOCR 是 ImagePipelineSmartOCR 在按面板所显示标签
	// 改名之前用过的 id。2026-09 以来保存的配置可能仍叫它，因此它被归一化而非
	// 被 honoring：见 NormalizeImagePipelineID。
	ImagePipelineLegacyObCapOCR ImagePipelineID = "ob_cap_ocr"
	// ImagePipelineLegacyCaptionOCR 是 ImagePipelineDefault 在面板重做之前用过的
	// id。已存储配置可能仍叫它，因此被归一化而非被 honoring：见
	// NormalizeImagePipelineID。
	ImagePipelineLegacyCaptionOCR ImagePipelineID = "caption_ocr"
)

// NormalizeImagePipelineID 把一个已存储的管线 id 映射到本构建注册的 id。一次
// 改名不能让存了旧拼写的知识库搁浅，因此每个曾经命名过已发布管线的 id 都继续
// 解析到它的后继；未知 id 原样透传，由调用方兜底。
func NormalizeImagePipelineID(id ImagePipelineID) ImagePipelineID {
	switch id {
	case ImagePipelineLegacyObCapOCR:
		return ImagePipelineSmartOCR
	case ImagePipelineLegacyCaptionOCR:
		return ImagePipelineDefault
	}
	return id
}

// ImagePipelineSpec 是一条管线向设置面板展示自己的方式。前端读取此形态，而不
// 按名字认识任何管线，因此新增管线只是加一个注册文件再加它的翻译，无需改 UI；
// Fields 携带与面板今日渲染相同的形态。
type ImagePipelineSpec struct {
	// ID 对应 imagePipeline.ID()；它是知识库存储的内容。
	ID ImagePipelineID `json:"id"`
	// Name 是人类可读标签；前端叠加自己的翻译并回落到此文本。
	Name string `json:"name"`
	// Description 告诉用户这条管线如何工作，显示在选择器下方。与 Name 一样，
	// 它被叠加翻译，以此文本为兜底。
	Description string `json:"description"`
	// Fields 是这条管线私有可调项。前端为每个字段渲染一个控件，并在该管线
	// 自己的键下回传它们，因此两条管线可以各自拥有一个名为 "enable_ocr"
	// 的字段而互不可见。
	Fields []ImageFieldDef `json:"fields"`
	// Rules 是在知识库保存之前其可调项必须满足的条件。为空表示该管线接受它
	// 所提供内容的任意组合。
	Rules []ImagePipelineRules `json:"rules,omitempty"`
}

// ImagePipelineIDFor 把「属性观察开关」映射到一个管线 id，因此 trace 标签与实际
// 运行的管线永远不会不一致。
func ImagePipelineIDFor(attrsEnabled bool) ImagePipelineID {
	if attrsEnabled {
		return ImagePipelineSmartOCR
	}
	return ImagePipelineDefault
}

// ImagePipelineRules 是一条管线的可调项在保存前必须满足的条件。它们随管线的
// spec 一起下发，设置面板以通用方式求值：前端自己不认识任何规则，只读取这里
// 声明的内容并上报结果。一个其选择不可能冲突的管线 —— 聪明那条自己调度动作 ——
// 不声明任何规则，它提供的任何开关都不可能被判为无效。
type ImagePipelineRules struct {
	// AtLeastOne 当 Fields 中至少一个字段为 on 时满足，全部为 off 时失败：
	// 这是一组会让图片什么处理都没做的开关。
	AtLeastOne []string `json:"at_least_one,omitempty"`
	// MessageKey 是该规则未满足时所显示警告的前端 i18n 键。
	MessageKey string `json:"message_key"`
	// Field 命名面板指向的控件，因此用户被告知该把哪个开关重新打开，而不只是
	// 「有东西不对」。留空时取 Fields 的第一个。
	Field string `json:"field,omitempty"`
}

// ImagePipelineValidationError 是一条管线在知识库为它保存的可调项按现状无法
// 运行时返回的东西。它携带一个 i18n 键而非一个句子：设置面板会问后端它所展示
// 的内容能否保存，而那个面板上的每个字符串都在前端被翻译 —— 这里放一句裸英文
// 会成为 UI 中唯一逃出叠加层的字符串。
type ImagePipelineValidationError struct {
	// MessageKey 命名问题所在的前端 i18n 键。
	MessageKey string `json:"message_key"`
	// Field 在单个字段有过错时命名该字段。面板指向那个控件而非整个分区，
	// 这正是「把其中一个重新打开」可操作的原因。为空表示整个配置有过错。
	Field string `json:"field,omitempty"`
}

func (e *ImagePipelineValidationError) Error() string {
	if e.Field == "" {
		return "image pipeline configuration is invalid: " + e.MessageKey
	}
	return "image pipeline field " + e.Field + " is invalid: " + e.MessageKey
}
