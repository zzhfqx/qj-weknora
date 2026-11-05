package service

import (
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

// DecideOCR 是一个纯函数：无 ctx、无仓储、无模型。输入为观察到的属性与策略，
// 输出为是否跑 OCR。它是「是否需要 OCR」这一决策的唯一所在地，因此可单元测试、
// 可回放。
//
// 默认策略对表读取的每个属性编码为：
//
//	ocr = (contain.text == "block")
//	    || (contain.data_visual == true)
//	    || 该属性未被观察到（保守：OnUnobserved）
//
// 其中「未被观察到」既覆盖模型从未写出某行，也覆盖写出的值不在允许集合内 ——
// 解析器让这类属性保持缺席，而不是编造默认值。因此一次不完整的观察仍继续抽取，
// 漏掉一段正文只多花一次调用，而不会丢失。
//
// 其结果与旧的七分类表等价：decorative / logo / photo（文字可靠地缺失或已被
// 描述捕获）跳过 OCR；text_screenshot / table / chart / other 跑 OCR。
func DecideOCR(attrs types.ImageAttrs, actions types.ImageActionsConfig) bool {
	if len(actions.OCR.On) == 0 {
		// 空条件列表从来不是已解析的策略（ResolveImageActions 总会填它），只是
		// 一个未携带任何条件的负载 —— 它的零值 OnUnobserved=false 会悄悄丢掉
		// OCR。回退到内置表，即该负载字段承诺的同一个保守策略。
		actions = types.DefaultImageActions()
	}
	incomplete := false
	for _, cond := range actions.OCR.On {
		v, ok := attrs.Attrs[cond.Prop]
		if !ok {
			// 观察没有就该属性作答，因此它无法独自作出决策 —— 也不能被读成
			// 「条件未满足」，那会把一次失败的观察悄悄当成自信的否定。记下它，
			// 交给 OnUnobserved 决策。
			incomplete = true
			continue
		}
		if fmt.Sprint(v) == cond.Is {
			return true
		}
	}
	// 没有命中任何条。要么观察就策略读取的每个属性都作答了（一次真实、完整的
	// 否定），要么没有 —— 此时由保守子句决策。
	if incomplete {
		return actions.OCR.OnUnobserved
	}
	return false
}
