package service

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

func init() { registerImagePipeline(defaultPipeline{}) }

// defaultPipeline 是手动路径：面板上的开关决定哪些解析动作运行，每个选中的动作
// 对每张图片都跑，无观察、无策略。id 刻意取 "default" —— 它是知识库在没挑别的
// 任何东西时回退到的管线，也是每个已注册动作随其落地成为一个手动可选开关的
// 地方。
type defaultPipeline struct{}

func (defaultPipeline) ID() types.ImagePipelineID { return types.ImagePipelineDefault }

func (defaultPipeline) Name() string { return "Manual: pick the parsing actions" }

func (defaultPipeline) Description() string {
	return "The user switches the parsing actions on or off to fit the task."
}

// 本管线的字段键。声明为常量，因为 default 与键住在同一个结构体里：一个面板按
// 一种拼写渲染字段、运行时按另一种拼写读取，正是这个形态要让它不可能发生的 bug。
const (
	captionFieldKeyEnableCaption = "enable_caption"
	captionFieldKeyEnableOCR     = "enable_ocr"
)

// Fields 声明面板为本管线渲染的开关。caption 与 OCR 默认为 true，这正是在这些
// 字段存在之前配置的知识库得到的行为：两个动作都跑，一如从前。
func (defaultPipeline) Fields() []types.ImageFieldDef {
	return []types.ImageFieldDef{
		{
			Key:           captionFieldKeyEnableCaption,
			Type:          types.ImageFieldTypeBool,
			Label:         "Enable caption",
			Description:   "Ask the model for a one-line description of every image and store it as the caption.",
			Default:       true,
			DecidesAction: true,
		},
		{
			Key:           captionFieldKeyEnableOCR,
			Type:          types.ImageFieldTypeBool,
			Label:         "Enable OCR",
			Description:   "Extract the text that appears in the image.",
			Default:       true,
			DecidesAction: true,
		},
	}
}

// Rules 是面板必须被告知的内容：caption 与 OCR 都可以被手动关掉，而一个把两者
// 都关掉的知识库会对每张图片什么都不做地处理它，因此至少要有一个保持开启。
//
// 这一对读作一条规则而非两条，因为「都关」是唯一被禁止的组合：单关 caption 仍会
// 让图片被 OCR，单关 OCR 仍会让它被描述。
func (defaultPipeline) Rules() []types.ImagePipelineRules {
	return []types.ImagePipelineRules{{
		AtLeastOne: []string{captionFieldKeyEnableCaption, captionFieldKeyEnableOCR},
		MessageKey: "imagePipeline.errors.noActionEnabled",
	}}
}

// Validate 是上面那条规则，在无法争辩的地方强制执行。
func (defaultPipeline) Validate(params map[string]any) error {
	return firstImagePipelineError(EvaluateImagePipelineRules(
		defaultPipeline{}.ID(), defaultPipeline{}.Rules(), defaultPipeline{}.Fields(), params,
	))
}

func (defaultPipeline) Run(ctx context.Context, r *runContext) error {
	// 两个开关都只属于本管线。它们都不是整任务级的限制：由另一条管线处理的图片
	// 可能跳过 caption 而本管线仍捕获它，因此字段名只相对于本管线有意义，并按
	// 同样的名字记录。面板里看守了「两者都关」；若 API 调用方执意如此，本轮只是
	// 记录没有配置任何要发生的事。
	if r.BoolParam(captionFieldKeyEnableCaption) {
		if err := r.execute(ctx, types.ImageActionCaption); err != nil {
			return err
		}
	}
	if !r.BoolParam(captionFieldKeyEnableOCR) {
		return nil
	}
	if err := r.execute(ctx, types.ImageActionOCR); err != nil {
		return err
	}
	// 记录下来，好让一行 trace 显示当时实际生效的是哪些开关。没有它，一个读到
	// false 的开关与一个从未运行的动作在输出里看起来一模一样。
	r.out["params"] = r.ParamSnapshot(captionFieldKeyEnableCaption, captionFieldKeyEnableOCR)
	return nil
}
