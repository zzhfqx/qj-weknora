package types

// KnowledgeProcessOverrides stores per-upload parse config overrides in knowledge metadata.
type KnowledgeProcessOverrides struct {
	ParserEngineRules        []ParserEngineRule        `json:"parser_engine_rules,omitempty"`
	ChunkingConfig           *ChunkingConfig           `json:"chunking_config,omitempty"`
	EnableMultimodel         *bool                     `json:"enable_multimodel,omitempty"`
	VLMConfig                *VLMConfig                `json:"vlm_config,omitempty"`
	ASRConfig                *ASRConfig                `json:"asr_config,omitempty"`
	QuestionGenerationConfig *QuestionGenerationConfig `json:"question_generation_config,omitempty"`
	GraphEnabled             *bool                     `json:"graph_enabled,omitempty"`
	ExtractConfig            *ExtractConfig            `json:"extract_config,omitempty"`
	// ImageAttrsEnabled 覆盖知识库的「属性观察图片管线」开关，仅对本文档生效。
	// nil 表示沿用知识库设置。
	ImageAttrsEnabled *bool `json:"image_attrs_enabled,omitempty"`
	// ImagePipelineID 覆盖本文档实际运行哪条图片管线。nil 沿用知识库解析出的
	// 管线。设置时 ImagePipelineParams 随行：一条管线的私有参数对另一条毫无
	// 意义，因此二者作为整体替换。
	ImagePipelineID *ImagePipelineID `json:"image_pipeline,omitempty"`
	// ImagePipelineParams 是被覆盖管线的私有可调项，仅与 ImagePipelineID
	// 一同生效。
	ImagePipelineParams map[string]any `json:"image_pipeline_params,omitempty"`
	// ImageActions 覆盖本文档的「属性 -> 工作」表（按动作键叠加在知识库表之上）。
	// nil 沿用知识库设置。
	ImageActions *ImageActionsConfig `json:"image_actions,omitempty"`
	// ParserEngineOverrides passes key-value configuration to docreader parsers
	// (e.g. pdf_force_scanned=true). Merged with workspace-level overrides in the
	// parse pipeline; per-upload values take priority on conflict.
	ParserEngineOverrides map[string]string `json:"parser_engine_overrides,omitempty"`
}

// EffectiveProcessConfig is the merged view used by the parse pipeline.
type EffectiveProcessConfig struct {
	ChunkingConfig           ChunkingConfig
	EnableMultimodel         bool
	VLMConfig                VLMConfig
	ASRConfig                ASRConfig
	QuestionGenerationConfig QuestionGenerationConfig
	GraphEnabled             bool
	ExtractConfig            ExtractConfig
	// 图片管线生效视图（知识库配置叠加逐文档覆盖后）。
	ImageAttrsEnabled   bool
	ImageActions        ImageActionsConfig
	ImagePipelineID     ImagePipelineID
	ImagePipelineParams map[string]any
}
