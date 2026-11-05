package types

import "time"

// ImageAsset 表示从某个 chunk 的 image_info 数组投影出的单张图片。
// 它是画廊列出、筛选、排序并对外提供的基本单元。
type ImageAsset struct {
	// ID 在知识库内稳定且唯一：“<chunkID>#<数组下标>”。
	ID string `json:"id"`
	// ChunkID 为所属 chunk（内嵌图片对应文本 chunk；独立图片资产对应
	// image_ocr / image_caption chunk）。
	ChunkID string `json:"chunk_id"`
	// KnowledgeID 为图片所属的文档/知识条目。
	KnowledgeID string `json:"knowledge_id"`
	// SourceName 为来源知识条目（文档）的可读名称，由 KnowledgeID 解析得到。
	// 当该条目已不存在（例如被删除）时为空，此时仍会展示 KnowledgeID。
	SourceName string `json:"source_name"`
	// ChunkType 为所属 chunk 的类型；在文本（文档）chunk 上发现的图片该字段
	// 为空。用于界面标注来源。
	ChunkType string `json:"chunk_type"`
	// URL 为渲染用图片 URL（COS / 存储提供商标识）。
	URL string `json:"url"`
	// OriginalURL 为转换前的来源引用（不同时才有）。
	OriginalURL string `json:"original_url"`
	// Caption 为模型生成的图片描述。
	Caption string `json:"caption"`
	// OCRText 为从图片中提取的文本（若有）。
	OCRText string `json:"ocr_text"`
	// Attrs 为属性管道产出的观察属性集合（例如 contain.text、contain.data_visual）。
	// 缺键表示“未观察”。
	Attrs map[string]any `json:"attrs"`
	// IsEnabled 镜像所属 chunk 的启停标志。
	IsEnabled bool `json:"is_enabled"`
	// Status 镜像所属 chunk 的索引状态。
	Status int `json:"status"`
	// CreatedAt 为所属 chunk 的创建时间。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 为所属 chunk 的最近更新时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// ImageListFilter 保存来自界面的画廊查询约束。属性引用使用命名空间化的画廊属性
// id（“<来源ID>:<名称>”，例如 “builtin:caption”、“system:contain.text”）。
type ImageListFilter struct {
	// Keyword 以不区分大小写的子串方式，匹配 SearchIn 各字段取值的并集。
	Keyword string
	// SearchIn 列出要搜索的命名空间属性 id。为空时使用画廊默认（builtin 的
	// caption + ocr_text）。handler 只转发解析后 usage 满足 in_searchfield=true 的 id。
	SearchIn []string
	// SortBy 为满足 in_sortfield=true 的命名空间属性 id。仍接受旧的裸值
	// （“created_at”、“updated_at”、“caption”），并映射到对应的 builtin id。
	SortBy string
	// SortOrder 为 “asc” 或 “desc”。
	SortOrder string
	// AttrFilters 把命名空间属性 id 映射到一组允许值。同一属性内多值按 OR
	// 处理；属性之间按 AND 处理。某属性出现在此 map 中、但图片在该属性上无值，
	// 则匹配失败（因此“未观察”是一个独立的、可筛选的状态）。这是较早的、
	// 位置式的属性筛选形式；画廊面板现在编辑的逐值裁决见 AttrRules。
	AttrFilters map[string][]string
	// AttrRules 把命名空间属性 id 映射到“每个允许值”的裁决：“”（或缺省）
	// 表示不干预该图片，“off” 表示隐藏，“on” 表示显示。一条裁决只针对实际
	// 携带该值的图片，因此图片由它所命中的规则来评判。
	//
	// 当多条规则作用于同一张图片时，“on” 优先于 “off”：强制显示胜过强制隐藏。
	// 一切未被规则认领的图片都保持可见，这正是默认状态可用的原因——筛选只会
	// 收窄列表，绝不会把列表清空。
	AttrRules map[string]map[string]string
	// IsEnabled 非 nil 时，限定为该启停状态的 chunk。
	IsEnabled *bool
	// KnowledgeIDs 非空时，把列表限定为所属文档属于这些知识条目的图片。
	// handler 会对列表长度设上限，调用方无法发送无界 IN 子句。
	KnowledgeIDs []string
}

// ImageAssetField 标识画廊查询可搜索、筛选或排序的某个图片取值。两者恰有一个被设置。
type ImageAssetField struct {
	// Builtin 为 builtin 来源的某个字段：“caption”、“ocr_text”、
	// “created_at”、“updated_at” 或 “is_enabled”。
	Builtin string
	// Attr 为从 image_info 的 attrs 中读取的、来源内属性名
	// （例如 “contain.text”）。
	Attr string
}

// ImageAssetValueSet 把一个字段与一组原始值配对。
type ImageAssetValueSet struct {
	Field  ImageAssetField
	Values []string
}

// ImageAssetQuery 是一次画廊分页请求的存储中立形态。service 把命名空间属性 id
// 翻译成字段；repository 再把它编译成 SQL，于是去重、筛选、排序、分页全部在
// 数据库内完成，只有被请求的那一页会离开数据库。
type ImageAssetQuery struct {
	// Keyword 以小写子串方式匹配各 SearchFields 取值用空格拼接后的文本。
	// 为空时关闭关键字匹配。
	Keyword      string
	SearchFields []ImageAssetField
	// AttrFilters 各集合之间 AND、单个集合内 OR；在该字段上无值的图片
	// 不受其约束。
	AttrFilters []ImageAssetValueSet
	// OnRules / OffRules 为逐值裁决：携带某个 “off” 值的图片会被隐藏，
	// 除非它同时携带某个 “on” 值。
	OnRules  []ImageAssetValueSet
	OffRules []ImageAssetValueSet
	// IsEnabled 非 nil 时，限定为该启停状态的 chunk。
	IsEnabled *bool
	// KnowledgeIDs 非空时，限定为这些知识条目所属的图片。
	KnowledgeIDs []string
	SortField    ImageAssetField
	SortDesc     bool
	Offset       int
	Limit        int
}

// ImageAssetRow 表示 repository 返回的一张去重后图片：一行 chunk_images，
// 即所属 chunk 的列加上该图片条目的字段。
type ImageAssetRow struct {
	ChunkID     string    `gorm:"column:chunk_id"`
	KnowledgeID string    `gorm:"column:knowledge_id"`
	ChunkType   string    `gorm:"column:chunk_type"`
	IsEnabled   bool      `gorm:"column:is_enabled"`
	Status      int       `gorm:"column:status"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
	ImageIndex  int       `gorm:"column:image_index"`
	URL         string    `gorm:"column:url"`
	OriginalURL string    `gorm:"column:original_url"`
	Caption     string    `gorm:"column:caption"`
	OCRText     string    `gorm:"column:ocr_text"`
	// AttrsJSON 为观察属性集合的 JSON 文本。
	AttrsJSON  string `gorm:"column:attrs_json"`
	TotalCount int64  `gorm:"column:total_count"`
}
