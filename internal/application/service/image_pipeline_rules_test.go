package service

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// TestManualPipelineRejectsEveryActionOff 覆盖手动管线声明的规则：它的两个动作开关
// 都关时，对一张图就无事可做，运行时只会如实记录而非抱怨，因此拒绝必须来自存储前
// 的这道检查。
func TestManualPipelineRejectsEveryActionOff(t *testing.T) {
	pipeline := imagePipelineRegistry[types.ImagePipelineDefault]
	err := pipeline.Validate(map[string]any{
		captionFieldKeyEnableCaption: false,
		captionFieldKeyEnableOCR:     false,
	})
	invalid, ok := err.(*types.ImagePipelineValidationError)
	if !ok {
		t.Fatalf("Validate(manual, both off) = %v, want an ImagePipelineValidationError", err)
	}
	if invalid.MessageKey == "" {
		t.Error("the rejection carries no message key, so the panel could not translate it")
	}
	// 指向一个开关正是告诉用户该把哪个打回去。
	if invalid.Field != captionFieldKeyEnableCaption {
		t.Errorf("field = %q, want %q (the first of the pair)", invalid.Field, captionFieldKeyEnableCaption)
	}
}

// TestManualPipelineAcceptsOneActionOn 是另一半：知识库可以只跑 caption 或只跑 OCR，
// 一个没人碰过的开关不得被读成「关了，因此无事可做」。
func TestManualPipelineAcceptsOneActionOn(t *testing.T) {
	pipeline := imagePipelineRegistry[types.ImagePipelineDefault]
	cases := []map[string]any{
		{captionFieldKeyEnableCaption: true, captionFieldKeyEnableOCR: false},
		{captionFieldKeyEnableCaption: false, captionFieldKeyEnableOCR: true},
		// 两个键都没带：都默认 true，这正是这些字段出现之前保存的知识库得到的。
		{},
		// 两个动作都开。
		{captionFieldKeyEnableCaption: true, captionFieldKeyEnableOCR: true},
		// 经表单到达管线的值可能以字符串形式到来。
		{captionFieldKeyEnableCaption: "true", captionFieldKeyEnableOCR: "false"},
	}
	for i, params := range cases {
		if err := pipeline.Validate(params); err != nil {
			t.Errorf("case %d (%v) = %v, want nil", i, params, err)
		}
	}
}

// TestManualPipelineRejectsOffByValue 钉住只有值说了算、与拼写无关：一个写成字符串的
// false 或一个 0，必须与布尔 false 过同一条规则，否则一个手工发 JSON 的调用方会
// 溜过检查。
func TestManualPipelineRejectsOffByValue(t *testing.T) {
	pipeline := imagePipelineRegistry[types.ImagePipelineDefault]
	cases := map[string]any{
		"boolean": false,
		"string":  "false",
		"zero int": 0,
	}
	for _, caption := range cases {
		for _, ocr := range cases {
			err := pipeline.Validate(map[string]any{
				captionFieldKeyEnableCaption: caption,
				captionFieldKeyEnableOCR:     ocr,
			})
			if err == nil {
				t.Errorf("both off as %#v and %#v = nil, want the rule to fire", caption, ocr)
			}
		}
	}
}

// TestUnknownPipelineIDSkipsValidation 让一个本构建不再发布的已存 id 不阻塞保存。运行
// 时以同样方式回退，因此一份无法再复现的配置仍应可编辑。
func TestUnknownPipelineIDSkipsValidation(t *testing.T) {
	err := ValidateImagePipelineParams(types.ImagePipelineID("pipeline_from_a_future_release"), map[string]any{})
	if err != nil {
		t.Errorf("Validate(unknown id) = %v, want nil", err)
	}
}

// TestValidationGoesThroughTheSavedPipeline 钉住检查按运行时的方式解析 id：一个从未点名
// 管线的知识库问的就是手动管线，被测试的仍是它的开关。
func TestValidationGoesThroughTheSavedPipeline(t *testing.T) {
	if err := ValidateImagePipelineParams("", nil); err != nil {
		t.Errorf("Validate(\"\") = %v, want nil (both actions default on)", err)
	}
	err := ValidateImagePipelineParams(types.ImagePipelineDefault, map[string]any{
		captionFieldKeyEnableCaption: false,
		captionFieldKeyEnableOCR:     false,
	})
	if err == nil || !strings.Contains(err.Error(), "noActionEnabled") {
		t.Errorf("Validate(manual, both off) = %v, want the noActionEnabled message key", err)
	}
}

// TestSmartOcrHasNoRules 钉住智能管线不声明任何规则：它的动作由模型观察到的属性调度，
// 而非由用户开关，因此无论参数怎么组合都不可能让一张图不被触碰，校验必然通过。
func TestSmartOcrHasNoRules(t *testing.T) {
	pipeline := imagePipelineRegistry[types.ImagePipelineSmartOCR]
	if err := pipeline.Validate(map[string]any{}); err != nil {
		t.Errorf("smartocr Validate(empty) = %v, want nil (no rules declared)", err)
	}
	if broken := BrokenImagePipelineRules(types.ImagePipelineSmartOCR, map[string]any{}); len(broken) != 0 {
		t.Errorf("smartocr BrokenImagePipelineRules = %v, want none", broken)
	}
}
