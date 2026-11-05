// 一条管线对自己私有可调项声明的规则，按知识库解析出的值求值。面板请求这次求值，
// 以便在用户提交前警告；保存路径在后端再次运行它，从而一个从未展示面板的调用方
// 也不能存储一条管线不会运行的东西。两侧都从 spec 读取同样的规则，因此在这里加
// 一条规则会出现在 UI 中，无需前端改动。

package service

import (
	"context"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// EvaluateImagePipelineRules 遍历一条管线的规则，按声明顺序报告其可调项破坏的
// 每一条。报告全部而非第一条正是目的：每条规则携带自己的措辞与自己的字段，因此
// 一个展示三条破损规则的面板能确切告诉用户改什么，而不是让他们一次修一个、反复
// 重读页面。空结果即管线会运行的配置。
//
// 一条规则点名它所涉及的字段，而非一个要计算的条件：「这些字段中至少一个为 on」
// 是一组动作开关所能意味的全部；一个无可关闭项可提供的管线干脆不声明规则，而非
// 一条永远成立的规则。
//
// 参数未提及的字段读取它声明的默认值，与运行时一致：一个在字段存在之前保存的
// 知识库，不会因从未提及它而被告知关掉了某个东西。
func EvaluateImagePipelineRules(
	pipelineID types.ImagePipelineID,
	rules []types.ImagePipelineRules,
	fields []types.ImageFieldDef,
	params map[string]any,
) []*types.ImagePipelineValidationError {
	// 用执行期解析器做命名空间优先级、默认值与布尔强转。另起一套解释可能接受一个
	// no-op 管线。
	r := &runContext{pipelineID: string(pipelineID), declared: fields, params: params}
	broken := make([]*types.ImagePipelineValidationError, 0, len(rules))
	for _, rule := range rules {
		if len(rule.AtLeastOne) == 0 || rule.MessageKey == "" {
			continue
		}
		satisfied := false
		for _, key := range rule.AtLeastOne {
			if r.BoolParam(key) {
				satisfied = true
				break
			}
		}
		if satisfied {
			continue
		}
		field := rule.Field
		if field == "" {
			field = rule.AtLeastOne[0]
		}
		broken = append(broken, &types.ImagePipelineValidationError{
			MessageKey: rule.MessageKey,
			Field:      field,
		})
	}
	return broken
}

// firstImagePipelineError 把一张破损规则列表变成保存路径上报的那个错误。一次拒绝
// 只能说出整个请求哪里不对，因此第一条破损规则代表全部。
func firstImagePipelineError(broken []*types.ImagePipelineValidationError) error {
	if len(broken) == 0 {
		return nil
	}
	return broken[0]
}

// validateImagePipelineConfig 是保存路径自己的检查，在设置面板已经询问并得到回答
// 之后运行。它在此处，是为了让一个从未展示面板的调用方 —— API 客户端、一次导入、
// 一个脚本 —— 也不能存储一条管线同样不会运行的组合。
//
// 管线 id 按运行时解析它的方式解析，而非按字面写下的字段取值：一个在 id 字段存在
// 之前保存的知识库不携带任何 id，那绝不应当让检查变成 no-op。
func (s *knowledgeBaseService) validateImagePipelineConfig(
	ctx context.Context,
	kb *types.KnowledgeBase,
) error {
	cfg := kb.ImageProcessingConfig
	pipelineID := cfg.ResolveImagePipelineID()
	err := ValidateImagePipelineParams(pipelineID, cfg.ImagePipelineParams)
	if err != nil {
		logger.Warnf(ctx, "[kb.image] rejecting pipeline %q params for knowledge base %q: %v",
			pipelineID, kb.ID, err)
	}
	return err
}
