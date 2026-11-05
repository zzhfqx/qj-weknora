package mcpserver

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestReadOnlyEndpointToolsAreNotDestructive 校验所有只读端点工具的四个 hint
// 被一次性正确声明：只读、不破坏、幂等、不访问外部世界。mcp.NewTool 的默认值
// 会把 destructive/openWorld 置为 true，只贴 readOnly 标签会让客户端误判。
func TestReadOnlyEndpointToolsAreNotDestructive(t *testing.T) {
	for _, tool := range []mcp.Tool{
		listKnowledgeBasesTool(),
		searchKnowledgeTool(),
		grepChunksTool(),
		listDocumentsTool(),
		readDocumentTool(),
		wikiSearchTool(),
		wikiReadPageTool(),
		wikiIndexTool(),
	} {
		ann := tool.Annotations
		if ann.ReadOnlyHint == nil || !*ann.ReadOnlyHint {
			t.Fatalf("%s 必须声明为只读", tool.Name)
		}
		if ann.DestructiveHint == nil || *ann.DestructiveHint {
			t.Fatalf("%s 不得声明为破坏性工具", tool.Name)
		}
		if ann.IdempotentHint == nil || !*ann.IdempotentHint {
			t.Fatalf("%s 必须声明为幂等", tool.Name)
		}
		if ann.OpenWorldHint == nil || *ann.OpenWorldHint {
			t.Fatalf("%s 不得声明为访问外部世界", tool.Name)
		}
	}
}

// TestMutatingEndpointToolsStayDestructive 校验写入类工具不被本次改动波及：
// ask 会创建会话、文档增删改本来就是写操作，均保持非只读/破坏性默认标记。
func TestMutatingEndpointToolsStayDestructive(t *testing.T) {
	ask := askTool()
	if ask.Annotations.ReadOnlyHint == nil || *ask.Annotations.ReadOnlyHint {
		t.Fatal("ask 不得声明为只读")
	}
	if ask.Annotations.DestructiveHint == nil || !*ask.Annotations.DestructiveHint {
		t.Fatal("ask 必须保持默认的破坏性提示")
	}
	for _, tool := range []mcp.Tool{
		addDocumentTool(),
		updateDocumentTool(),
		deleteDocumentTool(),
	} {
		if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Fatalf("%s 必须保持破坏性标记", tool.Name)
		}
	}
}
