package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// docScopeKnowledgeService 只实现 documentSearchTargets 真正会用到的
// GetKnowledgeByIDOnly；接口其余方法靠内嵌空接口留空（被测路径不会触达）。
type docScopeKnowledgeService struct {
	interfaces.KnowledgeService
	docs    map[string]*types.Knowledge
	lookups int
}

func (f *docScopeKnowledgeService) GetKnowledgeByIDOnly(_ context.Context, id string) (*types.Knowledge, error) {
	f.lookups++
	if k, ok := f.docs[id]; ok {
		return k, nil
	}
	return nil, nil
}

// newDocumentScopeServer 构造一个只带 knowledgeService 的 Server，
// kbs 与 docs 模拟官方夹具里的「自有库/共享库/越权库/伪造属主文档」布局。
func newDocumentScopeServer() (*Server, []*types.KnowledgeBase, *docScopeKnowledgeService) {
	kbs := []*types.KnowledgeBase{
		{ID: "kb-own", Name: "Own", TenantID: 1},
		{ID: "kb-shared", Name: "Shared", TenantID: 2},
	}
	docs := map[string]*types.Knowledge{
		"doc-a":      {ID: "doc-a", KnowledgeBaseID: "kb-own", TenantID: 1},
		"doc-b":      {ID: "doc-b", KnowledgeBaseID: "kb-own", TenantID: 1},
		"doc-shared": {ID: "doc-shared", KnowledgeBaseID: "kb-shared", TenantID: 2},
		// 属于未选中的库：传入的 kbs 里没有 kb-foreign
		"doc-foreign": {ID: "doc-foreign", KnowledgeBaseID: "kb-foreign", TenantID: 3},
		// 伪造属主：文档挂在 kb-shared 名下却声称属于租户 1
		"doc-spoof": {ID: "doc-spoof", KnowledgeBaseID: "kb-shared", TenantID: 1},
	}
	svc := &docScopeKnowledgeService{docs: docs}
	return &Server{knowledgeService: svc}, kbs, svc
}

func TestDocumentSearchTargets(t *testing.T) {
	t.Run("多文档按库聚合并去重，放宽阈值", func(t *testing.T) {
		srv, kbs, svc := newDocumentScopeServer()
		// 前后空格被裁掉，doc-a 重复出现只算一次；两个库各聚成一个 target。
		targets, err := srv.documentSearchTargets(t.Context(), kbs, []string{" doc-a ", "doc-b", "doc-a", "doc-shared"})
		require.NoError(t, err)
		require.Len(t, targets, 2)
		byKB := map[string]*types.SearchTarget{}
		for _, target := range targets {
			require.Equal(t, types.SearchTargetTypeKnowledge, target.Type)
			require.True(t, target.DisableRecallThresholds, "显式圈定文档后应放宽相关度阈值")
			byKB[target.KnowledgeBaseID] = target
		}
		require.ElementsMatch(t, []string{"doc-a", "doc-b"}, byKB["kb-own"].KnowledgeIDs)
		require.Equal(t, uint64(1), byKB["kb-own"].TenantID)
		require.ElementsMatch(t, []string{"doc-shared"}, byKB["kb-shared"].KnowledgeIDs)
		require.Equal(t, uint64(2), byKB["kb-shared"].TenantID)
		// 4 个入参但 doc-a 重复，实际文档查询只有 3 次。
		require.Equal(t, 3, svc.lookups)
	})

	t.Run("空名单报错并提示省略参数", func(t *testing.T) {
		srv, kbs, svc := newDocumentScopeServer()
		_, err := srv.documentSearchTargets(t.Context(), kbs, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "at least one")
		require.Zero(t, svc.lookups, "空名单必须在任何文档 I/O 之前拒绝")
	})

	t.Run("超过 100 个整体拒绝且不查库", func(t *testing.T) {
		srv, kbs, svc := newDocumentScopeServer()
		ids := make([]string, 101)
		for i := range ids {
			ids[i] = fmt.Sprintf("doc-%d", i)
		}
		_, err := srv.documentSearchTargets(t.Context(), kbs, ids)
		require.Error(t, err)
		require.Contains(t, err.Error(), "at most 100")
		require.Zero(t, svc.lookups, "超长名单必须在任何文档 I/O 之前拒绝")
	})

	t.Run("恰好 100 个通过", func(t *testing.T) {
		srv, kbs, _ := newDocumentScopeServer()
		ids := make([]string, 100)
		for i := range ids {
			id := fmt.Sprintf("doc-bulk-%d", i)
			ids[i] = id
			srv.knowledgeService.(*docScopeKnowledgeService).docs[id] =
				&types.Knowledge{ID: id, KnowledgeBaseID: "kb-own", TenantID: 1}
		}
		targets, err := srv.documentSearchTargets(t.Context(), kbs, ids)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		require.Len(t, targets[0].KnowledgeIDs, 100)
	})

	t.Run("空白文档 id 被拒绝", func(t *testing.T) {
		srv, kbs, _ := newDocumentScopeServer()
		_, err := srv.documentSearchTargets(t.Context(), kbs, []string{"doc-a", "   "})
		require.Error(t, err)
		require.Contains(t, err.Error(), "blank")
	})

	t.Run("文档不存在报错", func(t *testing.T) {
		srv, kbs, _ := newDocumentScopeServer()
		_, err := srv.documentSearchTargets(t.Context(), kbs, []string{"doc-a", "doc-missing"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found")
	})

	t.Run("文档不在所选知识库内报错", func(t *testing.T) {
		srv, kbs, _ := newDocumentScopeServer()
		_, err := srv.documentSearchTargets(t.Context(), kbs, []string{"doc-foreign"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "outside this endpoint's scope")
	})

	t.Run("伪造文档属主租户报错", func(t *testing.T) {
		srv, kbs, _ := newDocumentScopeServer()
		_, err := srv.documentSearchTargets(t.Context(), kbs, []string{"doc-spoof"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "outside this endpoint's scope")
	})
}

// TestSearchKnowledgeToolKnowledgeIDsSchema 验证对外工具 schema：
// knowledge_ids 为 1～100 的字符串数组，且非必填（省略=整库搜索）。
func TestSearchKnowledgeToolKnowledgeIDsSchema(t *testing.T) {
	raw, err := json.Marshal(searchKnowledgeTool())
	require.NoError(t, err)

	var schema struct {
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                  `json:"required"`
		} `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(raw, &schema))

	propertyRaw, ok := schema.InputSchema.Properties["knowledge_ids"]
	require.True(t, ok, "search_knowledge 必须声明 knowledge_ids 参数")

	var property struct {
		Type     string `json:"type"`
		MinItems int    `json:"minItems"`
		MaxItems int    `json:"maxItems"`
		Items    struct {
			Type      string `json:"type"`
			MinLength int    `json:"minLength"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(propertyRaw, &property))
	require.Equal(t, "array", property.Type)
	require.Equal(t, 1, property.MinItems)
	require.Equal(t, maxSearchKnowledgeDocuments, property.MaxItems)
	require.Equal(t, "string", property.Items.Type)
	require.Equal(t, 1, property.Items.MinLength)
	require.NotContains(t, schema.InputSchema.Required, "knowledge_ids")
}
