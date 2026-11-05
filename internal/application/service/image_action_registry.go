package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/vlm"
	"github.com/Tencent/WeKnora/internal/types"
)

// imagePipeline 是一条图片管线：一个作用于动作的函数，而非一张数据拓扑图。
// Run owns 管线的整个控制流，因此本包内没有调度器、没有轮次模型。
type imagePipeline interface {
	// ID 是管线的稳定 id；它同时也是 trace 行上记录的值。
	ID() types.ImagePipelineID
	// Name 是设置面板中显示的可读名。
	Name() string
	// Description 告诉用户这条管线如何工作，显示在设置面板选择器下方。
	Description() string
	// Fields 是本管线的可调项；nil 表示暂无。
	Fields() []types.ImageFieldDef
	// Rules 是这些可调项必须满足的条件。它们是声明式的而非实现式的，因为
	// 「一组设置为何不连贯」是管线的事实，而非渲染它的面板的事实：手动管线
	// 可能把每个动作都关掉，聪明那条自己调度工作、不可能如此。面板照样向后端
	// 求证 —— 保存路径会复核面板已批准的内容 —— 因此这里无需信任面板。
	Rules() []types.ImagePipelineRules
	// Validate 报告一组已保存的可调项能否运行。返回 nil 表示管线接受交到它
	// 手上的任何东西。
	Validate(params map[string]any) error
	// Run 执行该管线。
	Run(ctx context.Context, r *runContext) error
}

var imagePipelineRegistry = make(map[types.ImagePipelineID]imagePipeline)

func registerImagePipeline(p imagePipeline) {
	if _, dup := imagePipelineRegistry[p.ID()]; dup {
		// 包内 init() 顺序按文件名排列，因此一次静默覆盖会一直躺到有人发现
		// 跑错了管线才被察觉。
		panic(fmt.Sprintf("image pipeline: duplicate pipeline id %q", p.ID()))
	}
	imagePipelineRegistry[p.ID()] = p
}

// selectImagePipeline 解析一个任务所要求的管线。两条管线都从各自文件的 init()
// 注册。空或未注册的 id 回退到属性观察开关：管线字段出现之前入队的负载携带的
// 就是这个值，选择器出现之前保存的知识库要求的也是它。回退而非失败，让一个本
// 构建不再发布的已存储 id 不会搁浅它的图片。
func selectImagePipeline(payload *types.ImageMultimodalPayload) imagePipeline {
	id := types.ImagePipelineID(strings.TrimSpace(string(payload.ImagePipelineID)))
	return resolveImagePipeline(id, payload.ImageAttrsEnabled)
}

// resolveImagePipeline 是 selectImagePipeline 的查找逻辑，单独拆出以便校验入口
// 能对尚未变成负载的管线问同一个问题。
func resolveImagePipeline(pipelineID types.ImagePipelineID, attrsEnabled bool) imagePipeline {
	fallback := types.ImagePipelineIDFor(attrsEnabled)
	// 已存储的 id 可能以本构建已改名的拼写命名某条管线；归一化让这类配置留在
	// 它的后继上，而非悄悄掉到兜底。
	id := types.NormalizeImagePipelineID(pipelineID)
	if id == "" {
		id = fallback
	}
	if p, ok := imagePipelineRegistry[id]; ok {
		return p
	}
	return imagePipelineRegistry[fallback]
}

// ValidateImagePipelineParams 就一个 id 命名的管线，询问它知识库即将存储的
// 可调项能否运行。该 id 的解析方式与一个运行中任务完全一致，因此答案不可能与
// 实际处理图片的管线相左。
func ValidateImagePipelineParams(pipelineID types.ImagePipelineID, params map[string]any) error {
	return firstImagePipelineError(BrokenImagePipelineRules(pipelineID, params))
}

// BrokenImagePipelineRules 列出指定管线的已存储可调项破坏了它的每条规则。与
// ValidateImagePipelineParams 返回的单个错误不同，这里让每条违规保持独立，以便
// 设置面板一次性全部展示 —— 每行一条规则，各自指向需要改动的控件。一条不声明
// 规则的管线产出空列表，这正是「动作自行运行的管线永远不会被判错」的方式。
func BrokenImagePipelineRules(
	pipelineID types.ImagePipelineID,
	params map[string]any,
) []*types.ImagePipelineValidationError {
	pipeline := resolveImagePipeline(pipelineID, false)
	if pipeline == nil {
		return nil
	}
	return EvaluateImagePipelineRules(pipeline.ID(), pipeline.Rules(), pipeline.Fields(), params)
}

// ListImagePipelines 把每条已注册管线渲染成设置面板所见的形态。按 id 排序，
// 以便面板选项顺序稳定、不随包 init 顺序漂移。
func ListImagePipelines() []types.ImagePipelineSpec {
	ids := make([]types.ImagePipelineID, 0, len(imagePipelineRegistry))
	for id := range imagePipelineRegistry {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	specs := make([]types.ImagePipelineSpec, 0, len(ids))
	for _, id := range ids {
		p := imagePipelineRegistry[id]
		specs = append(specs, types.ImagePipelineSpec{
			ID:          p.ID(),
			Name:        p.Name(),
			Description: p.Description(),
			Fields:      p.Fields(),
			Rules:       p.Rules(),
		})
	}
	return specs
}

// actionHandler 是一个图片动作的可执行半部分。刻意没有服务接收者：当前每个动作
// 都是运行上下文的纯函数，因此运行一个动作不需要任何装配。
type actionHandler func(ctx context.Context, r *runContext) error

// actionSpec 把 types 中声明的半部分与它的处理器绑在一起。
type actionSpec struct {
	types.ImageActionSpec
	handler actionHandler
}

var imageActionRegistry = make(map[types.ImageActionID]actionSpec)

func registerAction(spec actionSpec) {
	if _, dup := imageActionRegistry[spec.ID]; dup {
		panic(fmt.Sprintf("image pipeline: duplicate action id %q", spec.ID))
	}
	imageActionRegistry[spec.ID] = spec
}

// init 把共享动作绑到它们的处理器上。绑定放在这里而非声明旁，是为了让「每个动作
// 恰好在一处注册」随管线增长继续成立。
func init() {
	handlers := map[types.ImageActionID]actionHandler{
		types.ImageActionCaption:            runCaptionAction,
		types.ImageActionObservationCaption: runObservationCaptionAction,
		types.ImageActionOCR:                runOCRAction,
	}
	for _, spec := range types.SharedImageActions {
		handler, bound := handlers[spec.ID]
		if !bound {
			panic(fmt.Sprintf("image pipeline: no handler bound to shared action %q", spec.ID))
		}
		registerAction(actionSpec{ImageActionSpec: spec, handler: handler})
	}
}

// runContext 是管线或动作得到的唯一框架入口。它不向外派发 VLM 句柄：与模型
// 交谈是动作自己的事，只能经 execute 到达。
//
// 与官方蓝本相比，本地裁掉了 thinking 开关（本地 VLM 只有 Predict，没有
// PredictWithOptions）与逐动作子跨度（本地 Handle 已用单一 imgSpan 包住整张图，
// 动作只往 out 写 trace 字段）。
type runContext struct {
	payload    *types.ImageMultimodalPayload
	model      vlm.VLM
	imageBytes []byte
	vlmCfg     types.VLMConfig
	imageInfo  *types.ImageInfo
	out        types.JSONMap
	// pipelineID 是为本轮运行服务的管线，用于给管线私有参数查找加命名空间
	// （"<pipeline>.<key>"）。手工构造 runContext 的测试中为空，只读裸键。
	pipelineID string
	// params 是本管线的私有可调项，按知识库解析得到。除此之外不得读取它：
	// 一条管线理解的键对另一条说明不了什么，因此该键只需在声明它的管线内无歧义。
	params map[string]any
	// declared 是管线自己的 imagePipeline.Fields()，随身携带，以便一个未触碰
	// 的键解析到面板承诺的默认值而非零值。它就是前端渲染的同一份列表。
	declared []types.ImageFieldDef
	// actions 记录本张图实际按序执行了哪些动作。它是逐图片回答「这张图经历了
	// 什么」的方式，单靠 trace 行的管线标签说不出来。
	actions []types.ImageActionID
	// vlmErr 保留本轮记录的第一个有类型 VLM 错误。动作记录失败而非传播它们，
	// 但当运行结束时什么也没产出，任务层需要这个有类型错误来区分永久失败与
	// 瞬时失败。
	vlmErr error
}

// Param 读取一个私有可调项，回落到管线为它声明的默认值。运行中的管线从未声明的
// 键读到 nil，这是刻意的：一个已存储的知识库可能携带本构建已没有的管线的参数，
// 运行不得因一个陈旧键而失败。
//
// 管线私有键先在运行管线的命名空间下查找（"<pipeline>.<key>"），因此两条管线
// 将来可能声明同一字段键而互不读取对方的存储值；裸键仍能命中，这让命名空间化
// 之前写下的已存储配置保持可读。
func (r *runContext) Param(key string) any {
	if r.params != nil {
		if r.pipelineID != "" {
			if v, ok := r.params[r.pipelineID+"."+key]; ok && v != nil {
				return v
			}
		}
		if v, ok := r.params[key]; ok && v != nil {
			return v
		}
	}
	for _, field := range r.declared {
		if field.Key == key {
			return field.Default
		}
	}
	return nil
}

// BoolParam 读取一个布尔可调项。JSON 把解包后的 "true" 变成真正的 bool，但手写
// 的配置可能用字符串，因此两者都读；其余回落到 false。
func (r *runContext) BoolParam(key string) bool {
	return r.BoolParamOr(key, false)
}

// BoolParamOr 与 BoolParam 一样读取布尔可调项，但当已存储参数与已声明字段默认值
// 都不作答时回落到 def。一个读取它未在 Fields() 中声明的键的管线 —— 即一个刻意
// 留在面板之外的控件 —— 把它的工作默认值钉在这个唯一读取该键的地方。
func (r *runContext) BoolParamOr(key string, def bool) bool {
	switch v := r.Param(key).(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "on"
	}
	return def
}

// ParamSnapshot 把给定键渲染成一张普通 map，供 trace 行使用。
func (r *runContext) ParamSnapshot(keys ...string) types.JSONMap {
	snapshot := make(types.JSONMap, len(keys))
	for _, key := range keys {
		if _, declared := r.fieldByKey(key); declared {
			snapshot[key] = r.Param(key)
		}
	}
	if len(snapshot) == 0 {
		return nil
	}
	return snapshot
}

// predict 向模型发起一次 VLM 调用，purpose 在 usage 日志中标注这次底层调用，
// 以便事后把观察步与 OCR 区分开：普通 caption 传 "image_caption"，观察 caption
// 传 "image_observation"，OCR 传 "image_ocr"。
//
// 本地只有 Predict（无 thinking 选项），因此这里直接调用。
func (r *runContext) predict(ctx context.Context, prompt, purpose string) (string, error) {
	return r.model.Predict(types.WithLLMCallMetadata(ctx, purpose, ""), [][]byte{r.imageBytes}, prompt)
}

func (r *runContext) fieldByKey(key string) (types.ImageFieldDef, bool) {
	for _, field := range r.declared {
		if field.Key == key {
			return field, true
		}
	}
	return types.ImageFieldDef{}, false
}

// execute 按 id 运行一个动作。一个缺失或未绑定的动作只损失那一个字段，而非整张
// 图：中断会为了一条警告丢掉同一张图的所有后续槽位。
func (r *runContext) execute(ctx context.Context, id types.ImageActionID) error {
	spec, ok := imageActionRegistry[id]
	if !ok || spec.handler == nil {
		logger.Warnf(ctx, "[ImageMultimodal] Action %q is not implemented; skipping", id)
		r.out["action_skipped"] = string(id)
		return nil
	}
	r.actions = append(r.actions, id)
	return spec.handler(ctx, r)
}

// runCaptionAction 索要一行描述并把它存为 caption。
func runCaptionAction(ctx context.Context, r *runContext) error {
	raw, err := r.predict(ctx, buildVLMCaptionPrompt(ctx, r.vlmCfg), "image_caption")
	if err != nil {
		// 只记录、不额外打日志：下面的观察失败是同类事件并会打自己的一行，
		// caption 缺失不应比本轮其余部分更吵。
		r.out["caption_error"] = err.Error()
		if r.vlmErr == nil {
			r.vlmErr = err
		}
		return nil
	}
	if text := strings.TrimSpace(raw); text != "" {
		r.imageInfo.Caption = text
		r.out["caption_chars"] = len([]rune(text))
		r.out["caption_preview"] = previewText(text, 200)
	}
	return nil
}

// runObservationCaptionAction 在一次应答中观察已注册属性并描述图片。描述只有在
// 本轮想要 caption 时才到达 caption 槽 —— 观察本身总会发生，因为属性才是这个
// 动作的意义，OCR 策略会把它们读回去。
func runObservationCaptionAction(ctx context.Context, r *runContext) error {
	raw, err := r.predict(ctx, buildImageAttrsPrompt(ctx, r.vlmCfg), "image_observation")
	if err != nil {
		logger.Warnf(ctx, "[ImageMultimodal] Describe and observe failed for %s: %v", r.payload.ImageURL, err)
		r.out["caption_error"] = err.Error()
		// 请求本身失败：属性永远不会到来。标记它，好让管线的 OCR 决策能区分
		// 「观察了并拒绝」与「没有观察可据以决策」。
		r.out["observation_failed"] = true
		if r.vlmErr == nil {
			r.vlmErr = err
		}
		return nil
	}
	obs, _ := types.ParseImageAttrsResponse(raw)
	// 描述是否到达 caption 槽是本管线的事，因此观察读它自己的键而非负载字段。
	// 该键未在 Fields() 中声明，工作默认值钉在此处：开启时 caption 仍是副产品；
	// 传 false 的 API 调用方只观察属性、不写 caption。
	applyImageObservation(r.imageInfo, obs.Attrs, obs.Description, r.out,
		r.BoolParamOr(smartFieldKeyCaptureCaption, true))
	if !obs.Observed {
		// 模型无视了属性协议 —— 可能一条用户自定义指令带偏了格式，或它用散文
		// 作答。不编造东西填补空缺：属性表保持为空，OCR 策略回落到保守的
		// OnUnobserved 子句。
		r.out["observation_failed"] = true
	}
	return nil
}

// runOCRAction 抽取图片承载的文字。
//
// 本地二开刻意保留两点（不跟上游）：① OCR 提示仍拼接知识库自定义指令；② 输出
// 经本地 sanitizeOCRText 清洗（上游改用 validateOCRText + html_table_normalizer，
// 本期不移植）。失败只记录到 out、不让整张图失败 —— 这与本地既有行为一致。
func runOCRAction(ctx context.Context, r *runContext) error {
	// 失败的抽取绝不能留下上一次尝试的文字。
	r.imageInfo.OCRText = ""
	r.out["ocr_chars"] = 0

	// OCR 提示：扫描件走扫描件专用提示，否则用默认提示。本地二开保留拼接 KB
	// 自定义指令的做法。
	prompt := vlmOCRPrompt
	if r.payload.ImageSourceType == "scanned_pdf" {
		prompt = vlmOCRScannedPDFPrompt
		logger.Infof(ctx, "[ImageMultimodal] Using scanned PDF prompt for OCR: %s", r.payload.ImageURL)
		r.out["ocr_prompt"] = "scanned_pdf"
	} else {
		r.out["ocr_prompt"] = "default"
	}
	prompt = types.AppendCustomPromptInstructions(prompt, r.vlmCfg.CustomInstructions, "image_ocr")

	ocrText, err := r.predict(ctx, prompt, "image_ocr")
	if err != nil {
		// 模型侧失败不等于「这张图没有文字」，但重试整张图也不是答案 —— caption
		// 可能已有效。记录失败，让结果总结给图片分类。
		logger.Warnf(ctx, "[ImageMultimodal] OCR failed for %s: %v", r.payload.ImageURL, err)
		if r.vlmErr == nil {
			r.vlmErr = err
		}
		r.out["ocr_status"] = "failed"
		r.out["ocr_error"] = err.Error()
		return nil
	}
	ocrText = sanitizeOCRText(ocrText)
	if ocrText == "" {
		logger.Warnf(ctx, "[ImageMultimodal] OCR returned empty/invalid content for %s, discarded", r.payload.ImageURL)
		r.out["ocr_status"] = "no_text"
		r.out["ocr_skipped"] = "empty_or_invalid"
		return nil
	}
	r.imageInfo.OCRText = ocrText
	r.out["ocr_status"] = "succeeded"
	r.out["ocr_chars"] = len([]rune(ocrText))
	r.out["ocr_preview"] = previewText(ocrText, 200)
	return nil
}

// buildImageAttrsPrompt 要求模型观察已注册的图片属性，并在同一应答中描述图片，
// 以便在花费图片的 OCR 调用之前已知属性。属性列表与问句来自注册表，因此提示
// 不可能与解析器把答案折回的属性漂移。
func buildImageAttrsPrompt(ctx context.Context, cfg types.VLMConfig) string {
	language := strings.TrimSpace(cfg.DescriptionLanguage)
	if language == "" {
		language = types.LanguageNameFromContext(ctx)
	}
	var b strings.Builder
	fmt.Fprintf(&b,
		"Observe the following about the image, then describe its main content in %s.\n\n"+
			"Output one line per observation using exactly this format, then a DESCRIPTION line:\n\n",
		language)
	for _, spec := range types.ImageAttrRegistry {
		allowed := make([]string, 0, len(spec.Values))
		for _, value := range spec.Values {
			allowed = append(allowed, value.Value)
		}
		fmt.Fprintf(&b, "%s: <%s>\n", spec.Name, strings.Join(allowed, " | "))
	}
	b.WriteString("DESCRIPTION: <one or two sentences>\n\nRules:\n")
	for _, spec := range types.ImageAttrRegistry {
		fmt.Fprintf(&b, "- %s: %s\n", spec.Name, spec.Question)
	}
	b.WriteString("- Output only those lines. No preamble, no summary, no extra commentary.\n")
	return types.AppendCustomPromptInstructions(b.String(), cfg.CustomInstructions, "image_description")
}

// applyImageObservation 把一份解析后的观察结果复制到图片记录，好随块持久化，
// 并镜像到 trace map。属性总是保留 —— 它们是下一步 OCR 策略读取的内容；而描述
// 只在本轮想要 caption 时存储：即使关掉 caption，观察轮次也不是可选的，因为
// 属性正是它的全部意义。
func applyImageObservation(
	imageInfo *types.ImageInfo,
	attrs types.ImageAttrs,
	description string,
	out types.JSONMap,
	wantCaption bool,
) {
	if attrs.Attrs != nil {
		imageInfo.Attrs = attrs
	}
	if description == "" {
		return
	}
	if !wantCaption {
		out["caption_skipped"] = "disabled"
		return
	}
	imageInfo.Caption = description
	out["caption_chars"] = len([]rune(description))
	out["caption_preview"] = previewText(description, 200)
}
