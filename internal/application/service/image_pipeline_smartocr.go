package service

import (
	"context"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

func init() { registerImagePipeline(smartOcrPipeline{}) }

// smartOcrPipeline 先观察，再决定下一步花什么。该 id 承诺观察先于决策，而不承诺
// caption 与 OCR 是之后唯一能发生的事 —— 两者之间的一个 refine 或 validate 动作
// 不会让这个名称成为谎言。
type smartOcrPipeline struct{}

func (smartOcrPipeline) ID() types.ImagePipelineID { return types.ImagePipelineSmartOCR }

func (smartOcrPipeline) Name() string { return "Observe, caption, then decide on OCR" }

func (smartOcrPipeline) Description() string {
	return "Observe the image's attributes and describe it first, then decide " +
		"from the attributes whether OCR is worth a pass, saving model calls."
}

// 本管线的字段键。allow_ocr 与 capture_caption 是管线私有键（本管线回答「究竟
// 可不可以花 OCR」，这是策略上的天花板而非要运行的步骤），刻意不复用默认管线的
// 步骤开关。
const (
	smartFieldKeyAllowOCR       = "allow_ocr"
	smartFieldKeyCaptureCaption = "capture_caption"
)

// Fields 声明本管线的面板控件。模型从观察到的属性判断 OCR 是否值得再跑一轮，而
// 观察产出的 caption 总是保留，因此没有天花板、也没有 caption 开关要暴露 ——
// 本管线不提供任何面板开关（thinking 开关本地已裁掉）。
func (smartOcrPipeline) Fields() []types.ImageFieldDef {
	return nil
}

// Rules 不返回任何东西，这就是「本管线不可以被设成什么样」的全部答案：观察、
// 描述与 OCR 由模型观察到的属性调度，而非由用户，因此它提供的开关无论怎么组合
// 都不可能让一张图不被触碰。通用求值规则的面板因此从不在此警告。
func (smartOcrPipeline) Rules() []types.ImagePipelineRules {
	return nil
}

// Validate 接受一切：见上面的 Rules。
func (smartOcrPipeline) Validate(_ map[string]any) error {
	return nil
}

func (smartOcrPipeline) Run(ctx context.Context, r *runContext) error {
	// 一次请求同时填满属性槽与 caption 槽，因此这里没有单独的 caption 动作要
	// 调度。allow_ocr 与 capture_caption 这两个天花板刻意不记录到 trace：面板
	// 从不声明它们，因此它们总是取默认值 —— 记录它们会读作好像已经作出了某个
	// 决策（在一个被拒绝的 OCR 旁边出现 "allow_ocr=true"）。决策与实际驱动它的
	// 输入记录在下面的 attr_policy 上。
	if err := r.execute(ctx, types.ImageActionObservationCaption); err != nil {
		return err
	}

	// 策略读取上面那个动作刚写下的属性；天花板只说明 OCR 究竟是否被允许。两行
	// 即使在 OCR 被跳过时也记录，因为 trace 必须展示作出的决策，而不是一张
	// 单纯缺失的 OCR 块。ocr_on_unobserved 随决策同行：它是观察失败或留下属性
	// 未作答时作决定的子句，正是它把「在无人产出的数据上被拒绝」与一次自信的
	// 否定区分开。
	want := r.BoolParamOr(smartFieldKeyAllowOCR, true) &&
		DecideOCR(r.imageInfo.Attrs, r.payload.ImageActions)
	r.out["attr_policy"] = types.JSONMap{
		"ocr":               want,
		"ocr_on_unobserved": r.payload.ImageActions.OCR.OnUnobserved,
	}
	r.out["image_attrs"] = r.imageInfo.Attrs.Attrs
	if !want {
		// 策略说「不」的原因：一次失败的观察没留下任何可供决策的属性（OnUnobserved
		// 子句拒绝了），这与「观察到了属性并对 OCR 投了反对票」是两回事。trace
		// 把两者分开，而不是用一个泛泛的 "attr_policy"，读作好像在无人产出的数据
		// 上作出了决策。
		observationFailed := r.out["observation_failed"] == true || r.out["caption_error"] != nil
		if observationFailed {
			logger.Infof(ctx, "[ImageMultimodal] Skipping OCR for %s (observation failed, attrs=%v)",
				r.payload.ImageURL, r.imageInfo.Attrs.Attrs)
			r.out["ocr_skipped"] = "no_ocr_after_observation_failed"
		} else {
			logger.Infof(ctx, "[ImageMultimodal] Skipping OCR for %s (attrs=%v)",
				r.payload.ImageURL, r.imageInfo.Attrs.Attrs)
			r.out["ocr_skipped"] = "attr_policy"
		}
		return nil
	}
	return r.execute(ctx, types.ImageActionOCR)
}
