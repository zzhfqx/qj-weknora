package mcpserver

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// selectorRequest 构造携带指定 knowledge_base_ids 参数的工具调用请求；
// args 为 nil 时表示完全不带该参数。
func selectorRequest(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
}

// TestOptionalKnowledgeBaseSelectors 校验显式传入 knowledge_base_ids 时的
// fail-closed 行为：未传 → nil（走端点默认范围）；显式传了畸形值（非数组、
// 含非字符串元素、含空白元素）必须报错，只有合法选择器原样返回。空数组与
// 未传等价，不报错，由下游当作“不收窄”处理。
func TestOptionalKnowledgeBaseSelectors(t *testing.T) {
	t.Run("未传参数走默认范围", func(t *testing.T) {
		got, err := optionalKnowledgeBaseSelectors(selectorRequest(nil))
		if err != nil || got != nil {
			t.Fatalf("未传参数应返回 (nil, nil)，got=%v err=%v", got, err)
		}
	})

	t.Run("合法选择器原样返回", func(t *testing.T) {
		got, err := optionalKnowledgeBaseSelectors(selectorRequest(map[string]any{
			"knowledge_base_ids": []any{"kb-own", "kb-other"},
		}))
		if err != nil {
			t.Fatalf("合法选择器不应报错: %v", err)
		}
		if len(got) != 2 || got[0] != "kb-own" || got[1] != "kb-other" {
			t.Fatalf("选择器内容/顺序被改变: %v", got)
		}
	})

	t.Run("空数组不报错", func(t *testing.T) {
		got, err := optionalKnowledgeBaseSelectors(selectorRequest(map[string]any{
			"knowledge_base_ids": []any{},
		}))
		if err != nil {
			t.Fatalf("空数组不应报错（与未传等价，由下游处理）: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("空数组应返回空切片，got=%v", got)
		}
	})

	for _, tc := range []struct {
		name string
		args any
	}{
		{"裸字符串", "kb-own"},
		{"null 值", nil},
		{"元素为数字", []any{123}},
		{"数组中混入数字", []any{"kb-own", 123}},
		{"单个空白元素", []any{" "}},
		{"数组中混入空字符串", []any{"kb-own", ""}},
	} {
		t.Run("畸形输入报错/"+tc.name, func(t *testing.T) {
			_, err := optionalKnowledgeBaseSelectors(selectorRequest(map[string]any{
				"knowledge_base_ids": tc.args,
			}))
			if err == nil {
				t.Fatalf("畸形选择器 %v 必须报错，防止静默放大为全量范围", tc.args)
			}
		})
	}
}
