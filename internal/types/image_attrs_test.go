package types

import (
	"reflect"
	"strings"
	"testing"
)

// TestImageAttrRegistryHasActiveAttributes 钉住 v1 观察集合：注册表是驱动 OCR 决策与
// 前端面板的唯一事实源，因此往后加一个属性只是这里加一行、别处不动。它还钉住每一行
// 都能解释自己——设置面板由这些字段渲染，因此一行没有文案的属性会以空白行到达一个
// 非技术运营者面前，一个没有含义的值会以裸代码（如 "sparse"）出现。
func TestImageAttrRegistryHasActiveAttributes(t *testing.T) {
	t.Parallel()

	var haveText, haveDataVisual bool
	for _, spec := range ImageAttrRegistry {
		if strings.TrimSpace(spec.Label) == "" {
			t.Errorf("%s has no display label", spec.Name)
		}
		if strings.TrimSpace(spec.Description) == "" {
			t.Errorf("%s has no display description", spec.Name)
		}
		if len(spec.Values) == 0 {
			if spec.Type == AttrTypePresence {
				t.Errorf("%s is presence but declares no values", spec.Name)
				continue
			}
			t.Errorf("%s declares no values", spec.Name)
		}
		for _, v := range spec.Values {
			if strings.TrimSpace(v.Label) == "" {
				t.Errorf("%s value %q has no short label", spec.Name, v.Value)
			}
			if strings.TrimSpace(v.Description) == "" {
				t.Errorf("%s value %q has no human explanation", spec.Name, v.Value)
			}
		}
		switch spec.Name {
		case "contain.text":
			haveText = true
			if spec.Type != AttrTypeExtent {
				t.Errorf("contain.text type = %q, want extent", spec.Type)
			}
			if len(spec.Values) != 3 {
				t.Errorf("contain.text values = %v, want none|sparse|block", spec.Values)
			}
		case "contain.data_visual":
			haveDataVisual = true
			if spec.Type != AttrTypePresence {
				t.Errorf("contain.data_visual type = %q, want presence", spec.Type)
			}
		}
	}
	if !haveText || !haveDataVisual {
		t.Fatalf("registry missing an active attribute: text=%v data_visual=%v", haveText, haveDataVisual)
	}
}

// TestParseImageAttrsResponse 覆盖「观察并描述」解析器：它取出属性行与描述，丢掉模型
// 实际未作答的属性（而非编造一个默认值），且从不把协议噪声存成 caption。
func TestParseImageAttrsResponse(t *testing.T) {
	t.Parallel()

	t.Run("reads attributes and description", func(t *testing.T) {
		obs, ok := ParseImageAttrsResponse(
			"contain.text: block\ncontain.data_visual: true\nDESCRIPTION: A pricing table.")
		if !ok {
			t.Fatal("expected a usable description")
		}
		if obs.Attrs.Attrs["contain.text"] != "block" {
			t.Errorf("contain.text = %v, want block", obs.Attrs.Attrs["contain.text"])
		}
		if obs.Attrs.Attrs["contain.data_visual"] != true {
			t.Errorf("contain.data_visual = %v, want true", obs.Attrs.Attrs["contain.data_visual"])
		}
		if obs.Description != "A pricing table." {
			t.Errorf("description = %q", obs.Description)
		}
	})

	t.Run("joins multi-line descriptions and tolerates emphasis", func(t *testing.T) {
		obs, ok := ParseImageAttrsResponse(
			"**contain.text**: sparse\ncontain.data_visual: false\nDESCRIPTION: first line\nsecond line\n")
		if !ok {
			t.Fatal("expected a usable description")
		}
		if obs.Attrs.Attrs["contain.text"] != "sparse" {
			t.Errorf("contain.text = %v, want sparse", obs.Attrs.Attrs["contain.text"])
		}
		if obs.Description != "first line second line" {
			t.Errorf("description = %q", obs.Description)
		}
	})

	t.Run("keeps a prose answer that ignores the protocol", func(t *testing.T) {
		// 完全没有属性行：模型仍描述了图片，因此它成为 caption，但不对属性下任何
		// 结论——它们保持缺席，由调用方的 OnUnobserved 子句决定。
		obs, ok := ParseImageAttrsResponse("- A wiring diagram.\n")
		if !ok {
			t.Fatal("expected a usable description")
		}
		if obs.Description != "A wiring diagram." {
			t.Errorf("list marker not stripped: %q", obs.Description)
		}
		if obs.Observed {
			t.Error("a prose answer must not count as an observation")
		}
		if _, present := obs.Attrs.Attrs["contain.text"]; present {
			t.Errorf("an unanswered attribute must stay absent, got %v", obs.Attrs.Attrs["contain.text"])
		}
	})

	t.Run("drops a value it cannot read instead of inventing one", func(t *testing.T) {
		obs, _ := ParseImageAttrsResponse("contain.text: maybe_a_lot\nDESCRIPTION: x")
		if _, present := obs.Attrs.Attrs["contain.text"]; present {
			t.Errorf("an unusable value must not be stored, got %v", obs.Attrs.Attrs["contain.text"])
		}
		if obs.Observed {
			t.Error("a line with an unusable value is not an observation")
		}
	})

	t.Run("keeps only the attributes the model answered for", func(t *testing.T) {
		obs, ok := ParseImageAttrsResponse("contain.text: sparse\nDESCRIPTION: A logo.")
		if !ok {
			t.Fatal("expected a usable description")
		}
		if obs.Attrs.Attrs["contain.text"] != "sparse" {
			t.Errorf("contain.text = %v, want sparse", obs.Attrs.Attrs["contain.text"])
		}
		if _, present := obs.Attrs.Attrs["contain.data_visual"]; present {
			t.Errorf("a half-answered observation must not grow the missing attribute, got %v",
				obs.Attrs.Attrs["contain.data_visual"])
		}
		if !obs.Observed {
			t.Error("the answer did observe one attribute")
		}
	})

	t.Run("unformatted response is reported as unusable", func(t *testing.T) {
		if _, ok := ParseImageAttrsResponse(""); ok {
			t.Error("an empty answer must not count as a description")
		}
	})

	t.Run("label-only answer yields no description but real attributes", func(t *testing.T) {
		obs, ok := ParseImageAttrsResponse("contain.text: block\ncontain.data_visual: true")
		if ok {
			t.Error("an answer with no description must be reported as missing caption")
		}
		if obs.Attrs.Attrs["contain.text"] != "block" {
			t.Errorf("contain.text = %v, want block", obs.Attrs.Attrs["contain.text"])
		}
	})
}

// TestDefaultImageActions 钉住保守的内建表：OCR 以 contain.text == block 或
// contain.data_visual == true 为门，对一个未观察的文本属性的兜底是保持 OCR 开着。
// 正是这一点让一次漏观察的代价是多一次调用，而非丢失文字。
func TestDefaultImageActions(t *testing.T) {
	t.Parallel()

	actions := DefaultImageActions()

	if !actions.OCR.OnUnobserved {
		t.Error("default OCR.OnUnobserved must be true (conservative)")
	}
	wantOn := []ImageAttrCondition{
		{Prop: "contain.text", Is: "block"},
		{Prop: "contain.data_visual", Is: "true"},
	}
	if len(actions.OCR.On) != len(wantOn) {
		t.Fatalf("default OCR.On = %v, want %v", actions.OCR.On, wantOn)
	}
	for i, c := range wantOn {
		if actions.OCR.On[i] != c {
			t.Errorf("default OCR.On[%d] = %+v, want %+v", i, actions.OCR.On[i], c)
		}
	}
}

// TestMergeImageActions 整体替换 OCR 条件列表：一个显式动作行被整体信任，因此知识库
// 调 OCR 时不必逐条件拼写，未来加到默认里的动作键也不会被一份较旧的已存配置擦掉。
func TestMergeImageActions(t *testing.T) {
	t.Parallel()

	custom := ImageActionsConfig{
		OCR: ImageOCRAction{
			On:           []ImageAttrCondition{{Prop: "contain.text", Is: "block"}},
			OnUnobserved: false,
		},
	}
	merged := MergeImageActions(custom)
	if merged.OCR.OnUnobserved {
		t.Error("an explicit OCR.OnUnobserved must replace the default (true)")
	}
	if len(merged.OCR.On) != 1 {
		t.Errorf("custom OCR.On must replace the default list, got %v", merged.OCR.On)
	}

	// 一个 nil/空的自定义整体保留默认。
	if got := MergeImageActions(ImageActionsConfig{}); !reflect.DeepEqual(got, DefaultImageActions()) {
		t.Error("an empty custom table must keep the default image actions")
	}
}
