package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// TestDecideOCR 钉住「是否跑 OCR」这一纯决策：文字成段或数据可视化即跑；一次完整、
// 自信的否定（两类属性都观察到且都不命中）才跳过；一次不完整的观察（属性缺席）按
// OnUnobserved 兜底，保守为跑。
func TestDecideOCR(t *testing.T) {
	t.Parallel()

	actions := types.DefaultImageActions() // contain.text==block || contain.data_visual==true；OnUnobserved=true

	mkAttrs := func(kv map[string]any) types.ImageAttrs {
		return types.ImageAttrs{Attrs: kv}
	}

	cases := []struct {
		name string
		attrs map[string]any
		onUnobserved bool
		want bool
	}{
		{
			name: "block text triggers OCR",
			attrs: map[string]any{"contain.text": "block", "contain.data_visual": false},
			onUnobserved: true,
			want: true,
		},
		{
			name: "data visual triggers OCR",
			attrs: map[string]any{"contain.text": "none", "contain.data_visual": true},
			onUnobserved: true,
			want: true,
		},
		{
			name: "complete confident negative skips OCR",
			attrs: map[string]any{"contain.text": "none", "contain.data_visual": false},
			onUnobserved: true,
			want: false,
		},
		{
			name: "unobserved text falls back to OnUnobserved=true",
			attrs: map[string]any{"contain.data_visual": false},
			onUnobserved: true,
			want: true,
		},
		{
			name: "unobserved attribute with OnUnobserved=false skips OCR",
			attrs: map[string]any{"contain.data_visual": false},
			onUnobserved: false,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := actions
			cfg.OCR.OnUnobserved = tc.onUnobserved
			if got := DecideOCR(mkAttrs(tc.attrs), cfg); got != tc.want {
				t.Errorf("DecideOCR(%v) = %v, want %v", tc.attrs, got, tc.want)
			}
		})
	}
}

// TestDecideOCREmptyOnFallsBackToDefault 钉住空条件列表不是一份已解析策略：一个未携带
// 任何条件的负载回退到内置保守表，而非用零值 OnUnobserved=false 悄悄丢掉 OCR。
func TestDecideOCREmptyOnFallsBackToDefault(t *testing.T) {
	t.Parallel()
	// 空 On + 缺席属性：若误用零值 OnUnobserved 会得到 false；回退默认（true）应为 true。
	got := DecideOCR(types.ImageAttrs{Attrs: map[string]any{}}, types.ImageActionsConfig{})
	if !got {
		t.Error("empty On must fall back to the conservative default (OCR on for unobserved)")
	}
}
