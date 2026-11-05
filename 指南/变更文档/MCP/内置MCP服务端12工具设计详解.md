# 内置 MCP 服务端 12 工具设计详解（重写参考）

> 适用基线：pg-fqx 分支（VERSION 0.8.3 + fqx 二开用户校验）。
> 本文只讲**形态②：进程内置的入站 MCP 服务端**（`internal/mcpserver/`，对外路径 `/mcp/:endpoint_id`，基于 mark3labs/mcp-go，Streamable HTTP、无状态）。
> **不包含**另外两套 MCP：出站 MCP 客户端（`internal/mcp/` + `internal/agent/tools/mcp_*.go`，让 agent 去调别人的 MCP）与仓库根目录独立的 Python `mcp-server/`。这两套与本文无关，重写内置工具面时无需参考。
>
> 阅读本文前建议先读同目录《内置MCP机制原理详解-0.8.3基线与二开用户校验.md》了解端点、鉴权与装配全貌；本文只聚焦「12 个工具本身」的参数、校验、内部调用与输出，供逐工具重写使用。

---

## 0. 一页速查

### 0.1 12 个工具总表

注册顺序见 [server.go:286-301](../../../internal/mcpserver/server.go#L286-L301)（`tools/list` 就按这个顺序）。名字常量集中在 [mcp_endpoint_tools.go:22-33](../../../internal/types/mcp_endpoint_tools.go#L22-L33)。

| # | 工具名 | 分组 | readOnly 注解 | destructive 注解 | 默认端点是否开放 | 内部调用形态 |
|---|--------|------|--------------|-----------------|----------------|-------------|
| 1 | `list_knowledge_bases` | retrieve | true | 否 | 是 | **service 接口**：`kbService.ListKnowledgeBases`（经 scope 过滤） |
| 2 | `search_knowledge` | retrieve | true | 否 | 是 | **agent 工具**：`tools.NewSearchKnowledgeTool(...).Execute` |
| 3 | `grep_chunks` | retrieve | true | 否 | 是 | **agent 工具**：复用 SearchKnowledgeTool，强制 keyword + 正则后置过滤 |
| 4 | `list_documents` | retrieve | true | 否 | 是 | **service 接口**：`knowledgeService.ListPagedKnowledgeByKnowledgeBaseID` |
| 5 | `read_document` | retrieve | true | 否 | 是 | **agent 工具**：`tools.NewReadDocumentTool(...).Execute` |
| 6 | `ask` | chat | **false**（会建会话/消息） | 否 | 是 | **service 接口编排**：session/message/agent + EventBus 同步跑 QA |
| 7 | `wiki_search` | wiki | true | 否 | 是 | **agent 工具**：`tools.NewWikiSearchTool(...).Execute` |
| 8 | `wiki_read_page` | wiki | true | 否 | 是 | **agent 工具**：`tools.NewWikiReadPageTool(...).Execute` |
| 9 | `wiki_index` | wiki | true | 否 | 是 | **service 接口**：`wikiService.GetIndexView` |
| 10 | `add_document` | ingest | false | **true** | **否** | **service 接口**：`CreateKnowledgeFromURL` / `CreateKnowledgeFromManual` |
| 11 | `update_document` | ingest | （未显式设置） | **true** | **否** | **service 接口**：`UpdateManualKnowledge` |
| 12 | `delete_document` | ingest | （未显式设置） | **true** | **否** | **service 接口**：`DeleteKnowledge` |

- 分组定义：retrieve / chat / wiki / ingest，见 [mcp_endpoint_tools.go:12-15](../../../internal/types/mcp_endpoint_tools.go#L12-L15)；目录与 `Destructive` 标记见 [mcp_endpoint_tools.go:47-61](../../../internal/types/mcp_endpoint_tools.go#L47-L61)。
- 新端点默认勾选「全部非 destructive」工具 = 9 个（含 `ask`），见 `DefaultMCPEndpointTools()` [mcp_endpoint_tools.go:78-87](../../../internal/types/mcp_endpoint_tools.go#L78-L87)。ingest 三个写工具默认不开放。
- 两种内部调用形态是本文最重要的分类，重写时务必区分：
  - **直连 service 接口**：handler 直接调 `s.xxxService.方法(...)`，返回值由 handler 自己组装成 map。
  - **agent 工具复用**：handler new 一个 agent 层工具实例（构造时注入预先算好的 `types.SearchTargets` / `[]tools.WikiScope`），把 Go map 序列化成 JSON 当入参，调用 `.Execute(ctx, json.RawMessage)`，拿到的是给 LLM 用的 `types.ToolResult{Success, Output, Data, Error}`，再经 `toolResultFromAgentTool` 转成 MCP 结果。

### 0.2 源文件地图

| 文件 | 内容 |
|------|------|
| [internal/mcpserver/server.go](../../../internal/mcpserver/server.go) | `Server` 依赖、装配、工具目录注册、guard 中间件、结果转换 |
| [internal/mcpserver/tools_retrieve.go](../../../internal/mcpserver/tools_retrieve.go) | 工具 1-5 的 schema 定义与 handler；list/read 摘要结构；grep 正则切词 |
| [internal/mcpserver/tools_ask.go](../../../internal/mcpserver/tools_ask.go) | 工具 6 ask 全部逻辑（agent/会话解析、同步 QA、引用整理） |
| [internal/mcpserver/tools_wiki.go](../../../internal/mcpserver/tools_wiki.go) | 工具 7-9 的 schema 与 handler |
| [internal/mcpserver/tools_ingest.go](../../../internal/mcpserver/tools_ingest.go) | 工具 10-12 的 schema 与 handler |
| [internal/mcpserver/scope.go](../../../internal/mcpserver/scope.go) | 全部工具共用的知识库作用域解析、租户切换、KB 选择器 |
| [internal/mcpserver/resource_urls.go](../../../internal/mcpserver/resource_urls.go) | 出口统一改写存储 URL（私有存储 → 可访问链接） |
| [internal/agent/tools/search_knowledge.go](../../../internal/agent/tools/search_knowledge.go) | 工具 2/3 的真实实现 |
| [internal/agent/tools/read_document.go](../../../internal/agent/tools/read_document.go) | 工具 5 的真实实现 |
| [internal/agent/tools/wiki_tools.go](../../../internal/agent/tools/wiki_tools.go) | 工具 7/8 的真实实现 |
| [internal/types/mcp_endpoint_tools.go](../../../internal/types/mcp_endpoint_tools.go) | 工具名常量、分组、destructive 目录、默认工具、能力映射 |

---

## 1. 横切机制：每个工具都要穿过的同一条链路

重写任一工具前必须先保住下面这几层，否则会把鉴权/越权/多租户打穿。

### 1.1 请求守卫链 `guardTool`

所有 `tools/call` 都先过 [server.go:192-224](../../../internal/mcpserver/server.go#L192-L224)，顺序为：

1. `endpointFromContext` 取端点；取不到 → `NewToolResultError("unauthorized")`。
2. `ep.HasTool(工具名)`：端点工具白名单。隐藏工具按名直调 → 拒绝并记审计 `tool_disabled`。
3. `s.userKnowledgeBaseIDs(ctx, ep)`（fqx 二开）：从 ctx 取已验签的外部用户 id，调 `userPermissionSvc.GetUserPermission(tenantID, userID)`。取不到用户身份或服务不可用 → 拒绝（fail-closed，审计 `identity_or_permission_unavailable`）；授权列表为空 → 拒绝（`no_user_permission`）。
4. Redis 滑动窗口限流 `s.limiter.Allow(ctx, ep.ID, ep.RateLimitPerMinute)` → 超限 `rate_limited`。
5. `touchLastUsed`（每端点最多每分钟落一次 `last_used_at`）。
6. 放行审计日志（`auditToolCall`，含 user_id/endpoint_id/tool/knowledge_base_id/knowledge_id/request_id）。
7. 执行具体 handler。
8. **出口统一** `s.rewriteResourceURLs(ctx, ep, result)`：把正文与 structuredContent 里的私有存储引用改写成带鉴权的可访问 URL，见 [resource_urls.go:32-60](../../../internal/mcpserver/resource_urls.go#L32-L60)。

`tools/list` 侧另有 `filterTools` [server.go:162-186](../../../internal/mcpserver/server.go#L162-L186)：端点白名单 + 用户必须有非空授权，否则返回空数组（注意返回非 nil 空切片，避免序列化成 `null`）。

> 关键：**第 2、3 步对 12 个工具一视同仁**。即使用户对某库无权限，也只是「看不到/调不动」，handler 内部还会再做一次基于知识库集合的收窄（纵深防御，不能依赖 guard 就不校验 scope）。

### 1.2 知识库作用域三层交集

`allowedKnowledgeBases(ctx, ep)` [scope.go:24-59](../../../internal/mcpserver/scope.go#L24-L59) 计算「这个端点 + 这个用户」真正能碰的库集合：

```
最终集合 = 端点允许集 ∩ fqx 用户授权集，再逐个 access.ResolveKB 重授权（丢掉失效/跨租户/无权限的 id）
```

- 端点未显式限定时，端点集 = `kbService.ListKnowledgeBases(ctx)`（工作空间自有 + 共享进来的全部库），再与用户集求交。
- 端点显式限定（`ep.RestrictsKnowledgeBases()`）时，只遍历 `ep.KnowledgeBaseIDs`，跳过用户未授权的 id，其余逐个 `authorizedKnowledgeBase(..., OrgRoleViewer)`，重授权失败仅 warn-and-continue。
- `userKnowledgeBaseIDs` 必须有用户身份 + 授权服务在线，否则返回错误（[scope.go:82-97](../../../internal/mcpserver/scope.go#L82-L97)）。

在其上还有两个公共助手：

- `selectKnowledgeBases(ctx, ep, requested []string)` [scope.go:174-208](../../../internal/mcpserver/scope.go#L174-L208)：在允许集内按选择器收窄。选择器**可以是 id，也可以是大小写不敏感的精确库名**（`matchKnowledgeBase` [scope.go:210-222](../../../internal/mcpserver/scope.go#L210-L222)，先比 id 再 EqualFold 比 name）。无选择器返回全部允许集；命中越界/不存在的选择器直接报错。
- `knowledgeInScope(ctx, ep, knowledgeID)` [scope.go:277-297](../../../internal/mcpserver/scope.go#L277-L297)：先**跨租户**按 id 取文档（`GetKnowledgeByIDOnly`），再要求其所属库在允许集内 **且** `kb.TenantID == k.TenantID`，否则报越界。

能力位过滤：

- `retrievableKnowledgeBases`：保留 `IsVectorEnabled() || IsKeywordEnabled()` 的库（工具 2/3 用），[scope.go:248-256](../../../internal/mcpserver/scope.go#L248-L256)。
- `wikiKnowledgeBases`：保留 `IsWikiEnabled()` 的库（工具 7/8/9 用），[scope.go:259-267](../../../internal/mcpserver/scope.go#L259-L267)。

### 1.3 租户切换 `scopedKBContext`

凡落到**文档级**操作（list_documents / read_document / add / update / delete）都要调
`s.scopedKBContext(ctx, kb, 角色)` [scope.go:134-148](../../../internal/mcpserver/scope.go#L134-L148)：

- 先 `access.ResolveKB` 解析授权（读 = `types.OrgRoleViewer`，写 = `types.OrgRoleEditor`）；
- 返回的 context 携带显式授权（让 `access.RequireKBWrite` 认账）并把**执行租户切到知识库属主租户**（共享库的文档存在属主租户下，不切租户会查不到数据或写错租户）；跨属主时还会把 `TenantInfo` 换成属主工作空间的配置，[scope.go:152-169](../../../internal/mcpserver/scope.go#L152-L169)。

### 1.4 MCP 结果的三种返回方式

- **业务/参数错误**：一律 `mcp.NewToolResultError(msg)`（或 `NewToolResultErrorFromErr`）。注意这是 **tool-result 的 isError=true，不是 HTTP/RPC 错误**，handler 始终返回 `(result, nil)`。
- **直连接口的成功结果**：`jsonResult(v)` [tools_retrieve.go:379-385](../../../internal/mcpserver/tools_retrieve.go#L379-L385) = `mcp.NewToolResultStructured(v, 缩进JSON文本)`，同时给 structuredContent 和纯文本。
- **agent 工具结果**：`toolResultFromAgentTool(res, execErr)` [server.go:306-327](../../../internal/mcpserver/server.go#L306-L327)：
  - err 或 `res.Success==false` → `NewToolResultError`；
  - `len(res.Data)>0` → `NewToolResultStructured(res.Data, res.Output)`；
  - 否则 → `NewToolResultText(res.Output)`。

### 1.5 入参读取约定（mcp-go `CallToolRequest`）

handler 用以下方法读参，**MCP schema 层不做强类型校验，钳制/默认全部在 handler 或 agent 工具内做**：

- `req.RequireString(name)`：缺失/非字符串返回 error（handler 一般再查 TrimSpace 非空）。
- `req.GetString(name, 默认值)`、`req.GetInt(name, 默认值)`、`req.GetBool(name, 默认值)`、`req.GetStringSlice(name, nil)`。
- 布尔「区分未传与显式传 false」需用 `req.GetArguments()["regex"].(bool)` 的 comma-ok 写法（wiki_search 用到）。

---

## 2. 工具 1：`list_knowledge_bases`

- 定义：`listKnowledgeBasesTool()` [tools_retrieve.go:22-29](../../../internal/mcpserver/tools_retrieve.go#L22-L29)；handler [tools_retrieve.go:117-131](../../../internal/mcpserver/tools_retrieve.go#L117-L131)。
- 描述定位：引导客户端第一个调用，列出作用域内所有可用库及其检索能力。

### 2.1 参数

**无参数。**

### 2.2 内部调用链（service 接口 + scope 过滤）

```go
kbs, err := s.allowedKnowledgeBases(ctx, ep)
```

`allowedKnowledgeBases` 内部直连的接口方法：

- `kbService.ListKnowledgeBases(ctx)`（接口 `KnowledgeBaseService`，端点非限定时取全量）；
- 限定时走 `kbService.GetKnowledgeBaseByIDOnly(ctx, id)` + `access.ResolveKB(...)`；
- fqx 用户集来自 `userPermissionSvc.GetUserPermission(ctx, ep.TenantID, userID)`。

随后逐库 `summarizeKnowledgeBase(kb)` [tools_retrieve.go:103-116](../../../internal/mcpserver/tools_retrieve.go#L103-L116)，按能力位产出 capability 字符串：`IsVectorEnabled()→"semantic"`、`IsKeywordEnabled()→"keyword"`、`IsWikiEnabled()→"wiki"`（恒返回非 nil 数组，可能为空 `[]`）。

### 2.3 输出（structuredContent）

```json
{
  "knowledge_bases": [
    {
      "id": "知识库ID",
      "name": "库名",
      "description": "描述（omitempty）",
      "type": "document|faq|...",
      "capabilities": ["semantic", "keyword", "wiki"]
    }
  ],
  "total": 1
}
```

结构定义 `knowledgeBaseSummary` [tools_retrieve.go:93-100](../../../internal/mcpserver/tools_retrieve.go#L93-L100)。无租户切换（不触文档）。

---

## 3. 工具 2：`search_knowledge`

- 定义：`searchKnowledgeTool()` [tools_retrieve.go:31-47](../../../internal/mcpserver/tools_retrieve.go#L31-L47)；handler `handleSearchKnowledge` [tools_retrieve.go:133-153](../../../internal/mcpserver/tools_retrieve.go#L133-L153)。
- 真实实现：[internal/agent/tools/search_knowledge.go](../../../internal/agent/tools/search_knowledge.go)。

### 3.1 参数

| 参数 | 类型 | 必填 | 默认 / 范围 | 说明 |
|------|------|------|------------|------|
| `query` | string | **是** | TrimSpace 后非空 | 自然语言问题、主题或精确词 |
| `mode` | string(enum) | 否 | `hybrid`；枚举 `hybrid`/`semantic`/`keyword` | hybrid=语义+关键词，semantic=按语义，keyword=精确词/编号/代码 |
| `limit` | number | 否 | 默认 10，**最大 30** | 返回片段上限 |
| `knowledge_base_ids` | string[] | 否 | 作用域内全部可检索库 | 元素可以是 id 或**精确库名** |

校验：query 缺失或空白 → `"query is required"`。mode 的合法性不在 handler 校验，透传给 agent 工具后由 `normalizeSearchMode` 处理（非法值返回错误，[search_knowledge.go:149-160](../../../internal/agent/tools/search_knowledge.go#L149-L160)）。limit 为 0/缺省时不放进 args，由工具补默认。

### 3.2 作用域与租户

```go
kbs, _ := s.selectKnowledgeBases(ctx, ep, req.GetStringSlice("knowledge_base_ids", nil))
kbs = retrievableKnowledgeBases(kbs)   // 只留开了向量/关键词索引的库
if len(kbs) == 0 { 报错：none ... supports semantic or keyword retrieval }
```

### 3.3 内部调用链（agent 工具复用）

`runSearchKnowledge` → `runSearchKnowledgeWithFilter(..., pattern=nil)` [tools_retrieve.go:157-183](../../../internal/mcpserver/tools_retrieve.go#L157-L183)：

```go
tool := tools.NewSearchKnowledgeTool(
    s.kbService, s.knowledgeService, s.chunkService,
    searchTargetsFor(kbs), // 预计算 SearchTargets，每个 target 带 kb.ID 与属主 kb.TenantID
    nil,                   // rerankModel 恒为 nil：MCP 面不做 rerank
    s.cfg,
)
callArgs := map[string]any{
    "query":              query,
    "mode":               mode,
    "knowledge_base_ids": knowledgeBaseIDs(kbs), // 已解析为真实 id
}
if limit > 0 { callArgs["limit"] = limit }
args, _ := json.Marshal(callArgs)
res, execErr := tool.Execute(ctx, args)
```

构造函数签名（[search_knowledge.go:130-147](../../../internal/agent/tools/search_knowledge.go#L130-L147)）：

```go
func NewSearchKnowledgeTool(
    knowledgeBaseService interfaces.KnowledgeBaseService,
    knowledgeService     interfaces.KnowledgeService,
    chunkService         interfaces.ChunkService,
    searchTargets        types.SearchTargets,
    rerankModel          rerank.Reranker,   // MCP 传 nil
    cfg                  *config.Config,
) *SearchKnowledgeTool
```

`searchTargetsFor` [scope.go:226-236](../../../internal/mcpserver/scope.go#L226-L236) 把每个库包成 `&types.SearchTarget{Type: SearchTargetTypeKnowledgeBase, KnowledgeBaseID: kb.ID, TenantID: kb.TenantID}`（携带属主租户，供共享库跨租户检索）。

agent 工具内部（`Execute` [search_knowledge.go:174-328](../../../internal/agent/tools/search_knowledge.go#L174-L328)）依次做：

1. 反序列化 `SearchKnowledgeInput{Query, Mode, KnowledgeBaseIDs, Limit}`（[search_knowledge.go:89-95](../../../internal/agent/tools/search_knowledge.go#L89-L95)）。
2. mode 归一；`clampSearchLimit`（<=0→10，>30→30，[search_knowledge.go:162-171](../../../internal/agent/tools/search_knowledge.go#L162-L171)）。
3. 若 args 里带 `knowledge_base_ids`，再与注入的 searchTargets 做一次一致性校验（`validateKnowledgeBaseIDsInSearchTargets`，拒绝幻觉 handle）。
4. `GetKnowledgeBasesByIDsOnly` 取库 → `resolveKBSearchModes` **逐库决定实际检索路径**：某库不具备请求的索引会自动降级（keyword 无关键词索引→semantic；semantic 无向量→keyword；hybrid embedding 失败→keyword），降级信息回写到 `mode_fallback`。
5. 并发按 target 检索（向量 + BM25 全文）、合并、去重。rerank 因 model=nil 跳过；做 MMR 选择（`selectMMR`，取 `min(候选, limit)`）、再去重、按 score 排序。
6. 预算控制 `formatWithinBudget`：从最低分开始丢弃片段直到渲染体积 ≤ 工具输出预算的 4/5，最佳结果必保留，丢弃数写入 `omitted_for_budget`。
7. 为命中片段懒加载图片信息（按属主租户分批 `EnrichSearchResultsImageInfo`）。
8. 部分库失败 → `partial_failures`；全部库失败 → 工具返回 `Success:false`（不允许把「检索失败」误报成「无结果」）。

### 3.4 输出

`Output` 文本：`<search_results count mode><query>...<documents 元数据头><chunk/faq ...><content>...` 的 XML 风文本。

`Data`（structuredContent）顶层键（[search_knowledge.go:916-923](../../../internal/agent/tools/search_knowledge.go#L916-L923) 与 [999-1037](../../../internal/agent/tools/search_knowledge.go#L999-L1037)）：

```json
{
  "knowledge_base_ids": ["..."],
  "query": "...",
  "queries": ["..."],            // 旧前端兼容别名，恒为单元素
  "mode": "hybrid",
  "results": [
    {
      "result_index": 1,
      "content": "片段正文",
      "knowledge_id": "文档ID",
      "knowledge_base_id": "库ID",
      "knowledge_title": "文档标题",
      "knowledge_metadata": { },
      "match_type": "...",
      "match_snippet": "命中摘要",
      "score": 0.0,
      "source_query": "...",
      "query_type": "...",
      "knowledge_base_type": "document|faq|...",
      "chunk_id": "chunk UUID",
      "chunk_index": 0,
      "images": [ ]
    }
  ],
  "count": 1,
  "kb_counts": { "库ID": 1 },
  "display_type": "search_results",
  "omitted_for_budget": 0,       // 仅在有丢弃时出现
  "partial_failures": ["..."],   // 仅部分失败时出现
  "rerank_rejected": 0           // 仅 rerank 发生且全拒时出现（MCP 面 rerank=nil，不会出现）
}
```

FAQ 类型库的结果额外带 `faq_id/index/faq_standard_question/faq_similar_questions/faq_answers`，且不含 chunk_* 键。

---

## 4. 工具 3：`grep_chunks`

- 定义：`grepChunksTool()` [tools_retrieve.go:49-63](../../../internal/mcpserver/tools_retrieve.go#L49-L63)；handler [tools_retrieve.go:185-211](../../../internal/mcpserver/tools_retrieve.go#L185-L211)。
- 定位：语义检索可能漏掉的精确词/标识符/错误码/产品名，走「关键词索引取候选 + 正则后置过滤」。

### 4.1 参数

| 参数 | 类型 | 必填 | 默认/范围 | 说明 |
|------|------|------|----------|------|
| `query` | string | **是** | POSIX 正则、大小写不敏感；非法正则按字面量匹配 | 匹配片段正文 |
| `limit` | number | 否 | 默认 10，最大 30 | |
| `knowledge_base_ids` | string[] | 否 | 作用域内全部可检索库 | id 或精确库名 |

注意描述里写的是「POSIX 正则」，但实现用的是 **Go 的 RE2 正则**（`regexp.Compile("(?i)"+query)`），二者语法大体兼容但不完全等价，重写时注意措辞与行为。

### 4.2 正则 → 关键词候选：`grepPatternAndTerms`

[tools_retrieve.go:221-244](../../../internal/mcpserver/tools_retrieve.go#L221-L244)：

1. 尝试 `(?i)+query` 编译；编译失败则 `regexp.QuoteMeta(query)` 后按纯字面匹配。
2. 合法正则时，先用 `grepEscapeRE = \\[A-Za-z]` 把 `\d \b \s` 这类无字面文本的转义替换成空格。
3. 用 `strings.FieldsFunc` 按「非 Unicode 字母/数字/`_`/`-`」切词，小写去重，拼成空格分隔的 terms。
4. terms 为空（如 `^\d+$`）→ 报错，提示至少包含一个可被索引检索的字面词（[tools_retrieve.go:204-207](../../../internal/mcpserver/tools_retrieve.go#L204-L207)）。

### 4.3 内部调用链（复用 SearchKnowledgeTool）

```go
pattern, terms := grepPatternAndTerms(query)
s.runSearchKnowledgeWithFilter(ctx, kbs, terms, tools.SearchModeKeyword, limit, pattern)
```

与工具 2 的差异：

- mode **强制 `keyword`**；检索 query 用的是切出的 terms（不是原正则）。
- 构造后调用 `tool.WithPatternFilter(pattern)`（[search_knowledge.go:122-127](../../../internal/agent/tools/search_knowledge.go#L122-L127)）。该过滤器把**候选池取数上限扩大到 max=30**（`retrievalLimit = searchKnowledgeMaxLimit`，[search_knowledge.go:231-234](../../../internal/agent/tools/search_knowledge.go#L231-L234)），检索回来后在 [search_knowledge.go:285-293](../../../internal/agent/tools/search_knowledge.go#L285-L293) 用 `pattern.MatchString(getEnrichedPassage(...))` 二次过滤，最后仍截断到用户给的 `limit`。
- 匹配正文用 `getEnrichedPassage`（正文 + 图片 caption/OCR 文本，保留 markdown/URL，[search_knowledge.go:1043 起](../../../internal/agent/tools/search_knowledge.go#L1043)）；命中文本摘要走 `extractSnippetRegex`。

### 4.4 输出

与工具 2 完全同构（同一个 `formatOutput`），区别仅 `mode="keyword"`、`match_snippet` 由正则提取。

> 重写要点：grep 不是数据库正则扫描，而是「索引候选 + 内存正则校验」。这意味着**没有任何字面 token 的纯正则（如 `^\d+$`）无法工作**，因为根本没有可用于取候选的词。

---

## 5. 工具 4：`list_documents`

- 定义：`listDocumentsTool()` [tools_retrieve.go:65-76](../../../internal/mcpserver/tools_retrieve.go#L65-L76)；handler [tools_retrieve.go:272-323](../../../internal/mcpserver/tools_retrieve.go#L272-L323)。

### 5.1 参数

| 参数 | 类型 | 必填 | 默认/范围 | 说明 |
|------|------|------|----------|------|
| `knowledge_base_id` | string | **是** | — | id 或精确库名（单库） |
| `keyword` | string | 否 | — | 标题子串过滤（后端 LIKE file_name/title） |
| `page` | number | 否 | 默认 1，最小 1 | 1 基页码 |
| `page_size` | number | 否 | 默认 20，**最大 100** | |

### 5.2 作用域与租户

`selectKnowledgeBases(ctx, ep, []string{selector})` → 取 `kbs[0]` → `scopedKBContext(ctx, kb, OrgRoleViewer)`（切到库属主租户）。

### 5.3 内部调用链（直连 service 接口）

```go
result, err := s.knowledgeService.ListPagedKnowledgeByKnowledgeBaseID(
    ctx, kb.ID,
    &types.Pagination{Page: page, PageSize: pageSize},
    types.KnowledgeListFilter{Keyword: strings.TrimSpace(req.GetString("keyword", ""))},
)
```

接口签名：

```go
ListPagedKnowledgeByKnowledgeBaseID(
    ctx context.Context, kbID string,
    page *types.Pagination,                 // {Page, PageSize int}
    filter types.KnowledgeListFilter,       // 这里只用 Keyword
) (*types.PageResult, error)
```

`types.KnowledgeListFilter` 完整字段（重写若要扩过滤条件可用）：`TagIDs []string`、`Keyword string`（匹配 file_name/title）、`FileType`、`ParseStatus`、`Source`、`UpdatedFrom/UpdatedTo time.Time`、`FolderPath`。

返回的 `result.Data` 用类型断言 `[]*types.Knowledge` 取出，逐条 `summarizeKnowledge`。

### 5.4 输出

`summarizeKnowledge` 结构 `documentSummary` [tools_retrieve.go:246-270](../../../internal/mcpserver/tools_retrieve.go#L246-L270)：

```json
{
  "knowledge_base_id": "库ID",
  "total": 0,
  "page": 1,
  "page_size": 20,
  "documents": [
    {
      "id": "文档ID",
      "title": "标题",
      "file_name": "（omitempty）",
      "file_type": "（omitempty）",
      "source": "（omitempty）",
      "parse_status": "completed|...",
      "description": "（omitempty）",
      "created_at": "RFC3339",
      "updated_at": "RFC3339"
    }
  ]
}
```

`page/page_size/total` 取自 `result.Page/result.PageSize/result.Total`（注意不是回显入参）。

---

## 6. 工具 5：`read_document`

- 定义：`readDocumentTool()` [tools_retrieve.go:78-89](../../../internal/mcpserver/tools_retrieve.go#L78-L89)；handler [tools_retrieve.go:325-375](../../../internal/mcpserver/tools_retrieve.go#L325-L375)。
- 真实实现：[internal/agent/tools/read_document.go](../../../internal/agent/tools/read_document.go)。

### 6.1 MCP 层参数

| 参数 | 类型 | 必填 | 默认/范围 | 说明 |
|------|------|------|----------|------|
| `knowledge_id` | string | **是** | — | search/list 返回的文档 id |
| `offset` | number | 否 | 默认 0，最小 0 | 阅读序偏移（**不是 chunk_index**） |
| `limit` | number | 否 | 默认 20，**最大 100** | chunk 数 |
| `query` | string | 否 | — | 大小写不敏感的文档内定位词 |

> **MCP 层没有、但底层 agent 工具支持的两个参数：`regex`（bool）与 `context`（0-5）**。handler 只透传 `id/limit/offset/query`（[tools_retrieve.go:356-363](../../../internal/mcpserver/tools_retrieve.go#L356-L363)），永远不下发 regex/context。因此 MCP 面的 `query` 永远走底层的「空白分词、每词都要命中、乱序」字面匹配，而**不是** MCP 描述文案所说的「phrase（短语）」匹配——这是一处文案与实现不一致（见 §15）。

### 6.2 作用域与租户

`knowledgeInScope(ctx, ep, knowledgeID)`（跨租户取文档 + 库/租户双重匹配）→ `scopedKBContext(ctx, kb, OrgRoleViewer)`。

### 6.3 内部调用链（agent 工具复用）

```go
limit := clamp(req.GetInt("limit", 20))   // <1→20, >100→100（MCP 层先钳一次）
offset := max(0, req.GetInt("offset", 0))
tool := tools.NewReadDocumentTool(
    s.knowledgeService, s.chunkService,
    searchTargetsFor([]*types.KnowledgeBase{kb}),
)
callArgs := map[string]any{"id": k.ID, "limit": limit, "offset": offset}
if query != "" { callArgs["query"] = query }
res, _ := tool.Execute(ctx, mustJSON(callArgs))
```

构造函数（[read_document.go:98-109](../../../internal/agent/tools/read_document.go#L98-L109)）：

```go
func NewReadDocumentTool(
    knowledgeService interfaces.KnowledgeService,
    chunkService      interfaces.ChunkService,
    searchTargets     types.SearchTargets,
) *ReadDocumentTool
```

底层 `ReadDocumentInput{ID, Offset, Limit, Query, Regex, Context}`（[read_document.go:78-86](../../../internal/agent/tools/read_document.go#L78-L86)），`Execute` 分派逻辑（[read_document.go:120-189](../../../internal/agent/tools/read_document.go#L120-L189)）：

1. `resolveTarget(id)`（[read_document.go:195-223](../../../internal/agent/tools/read_document.go#L195-L223)）：**先按文档 id 查** `GetKnowledgeByIDOnly`（MCP 传的就是真实文档 UUID），查不到再当 chunk id 查 `GetChunkByIDOnly`；并在注入的 searchTargets 内做二次授权。
2. limit（<=0→20，>100→100）、offset（<0→0）、context（<0→0，>5→5）再次钳制。
3. 构造匹配器：
   - 无 query → 不分派到查询；
   - `regex=true` → 单个 `(?i)+query` 正则（MCP 面用不到）；
   - 默认 → `strings.Fields(query)` 每个词 `QuoteMeta` 成一个 `(?i)` 正则，**所有词都命中**才算匹配（乱序）。
4. 三条执行路径：
   - 有匹配器 → `readByQuery`：按阅读序从 offset 扫描（每页 100 chunk，`readDocumentScanPageSize`），命中 chunk 带前后各 1 块上下文，最多 `readDocumentMaxMatches=20` 个匹配，受输出预算约束；截断时给 `next_offset`（下一个未返回匹配的阅读序位置）。
   - id 命中 chunk 且无 query → `readAroundChunk`：单块 focus + 用 `ListChunkNeighbors` 按 chunk_index 取邻居。
   - 否则 → `readPage`：读 `[offset, offset+limit)`，`fitChunksToBudget` 按预算裁剪，给 `next_offset=offset+len(chunks)`（未到末尾时）。

### 6.4 MCP 层对结果的追加注入

成功后（[tools_retrieve.go:369-374](../../../internal/mcpserver/tools_retrieve.go#L369-L374)）：

```go
res.Data["document"]       = summarizeKnowledge(k)         // MCP 侧扁平文档摘要
res.Data["knowledge_base"] = map[string]any{"id": kb.ID, "name": kb.Name}
```

注意这会**覆盖**底层 `buildData` 里自带的富结构 `document`（documentInfo，含 chunk_count/metadata/file_size 等，[read_document.go:518-542](../../../internal/agent/tools/read_document.go#L518-L542)），最终对外只保留扁平摘要。

### 6.5 输出

底层公共 Data（`buildData` [read_document.go:544-570](../../../internal/agent/tools/read_document.go#L544-L570)）+ 路径专属键，再叠加 MCP 注入：

```json
{
  "display_type": "knowledge_chunks_list",
  "knowledge_id": "文档ID",
  "knowledge_title": "标题",
  "total_chunks": 0,
  "fetched_chunks": 0,
  "chunks": [
    {
      "chunk 字段（id/index/content 等，由 chunkDataMap 产出）": "",
      "role": "match|context_before|context_after|focus（仅查询/邻居路径出现）",
      "match_snippet": "（仅查询命中出现）"
    }
  ],
  "document": { "id": "...", "title": "...", "parse_status": "..." },
  "knowledge_base": { "id": "...", "name": "..." },

  "offset": 0, "page_size": 20, "page": 1,
  "next_offset": 20,

  "query": "...", "match_count": 0, "truncated": false
}
```

- `offset/page_size/page/next_offset` 仅 readPage 路径出现。
- `query/match_count/truncated/next_offset` 仅 readByQuery 路径出现。
- `single_chunk/focus_chunk_id/faq_id/faq_question` 仅 readAroundChunk 路径出现（MCP 面传文档 UUID，正常不会走该路径）。
- 越界 offset 会得到 `Success:false` 错误，Data 带 `total_chunks/requested_offset/requested_limit/suggested_offset`。

---

## 7. 工具 6：`ask`

- 定义：`askTool()` [tools_ask.go:26-43](../../../internal/mcpserver/tools_ask.go#L26-L43)；handler `handleAsk` [tools_ask.go:62-119](../../../internal/mcpserver/tools_ask.go#L62-L119)。
- 定位：同步问答（可能跑 agent，长达数分钟），返回带引用的合成答案。**readOnly=false**，因为会建会话/消息，且配置的 agent 可能执行自己的工具。

### 7.1 参数

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `question` | string | **是** | TrimSpace 后非空 | 问题 |
| `session_id` | string | 否 | 新建会话 | 上一次 ask 返回的会话 id，用于多轮 |
| `knowledge_base_ids` | string[] | 否 | 见 7.3 三级回退 | id 或精确库名 |

**调用方不能指定 agent**：agent 由端点强制决定。

### 7.2 agent 解析 `resolveAskAgent`

[tools_ask.go:125-138](../../../internal/mcpserver/tools_ask.go#L125-L138)：

```go
agentID := strings.TrimSpace(ep.DefaultAgentID)
if agentID == "" { agentID = types.BuiltinQuickAnswerID }
agent, err := s.agentService.GetAgentByID(ctx, agentID)
// 再用 types.MCPEndpointAgentAllowed(agent, ep.TenantID) 拦截内部专用 agent
```

`MCPEndpointAgentAllowed` 会挡掉带写/Shell 工具的内建 agent（如 wiki 修复、技能安装等），防止借 MCP 端点绕过工具白名单。

### 7.3 知识库范围三级回退 `resolveAskKnowledgeBases`

[tools_ask.go:145-179](../../../internal/mcpserver/tools_ask.go#L145-L179)：

1. 请求显式带 `knowledge_base_ids`（有任一非空）→ `selectKnowledgeBases` 解析，返回 id 列表；
2. 否则端点是限定模式（`ep.RestrictsKnowledgeBases()`）→ 用端点全部允许库；
3. 否则看 agent 配置 `Config.KBSelectionMode`：agent 自己没配库（mode=`none`，或 mode=`selected`/空且 `Config.KnowledgeBases` 为空）→ 回退到端点全部允许库；
4. 其余情况返回 **nil**，表示「交给 agent 自身配置决定检索哪些库」。

### 7.4 会话解析 `resolveAskSession`

[tools_ask.go:181-209](../../../internal/mcpserver/tools_ask.go#L181-L209)：

- 会话属主固定为端点影子主体：`ownerID := types.MCPEndpointPrincipal(ep.TenantID, ep.ID).StorageID()`。
- 带了 session_id：`sessionService.GetSessionByID(ctx, ep.TenantID, sessionID)`，并校验 `session.UserID == ownerID`，否则报「该会话不属于此端点」（防止跨端点/跨用户串会话）。
- 不带：新建会话，标题取问题前 60 个 rune（`askSessionTitleMaxLen`），`Description: "MCP endpoint "+ep.Name`，先 `CreateSession` 再 `SetSessionOwnerID(tenantID, sessionID, ownerID)` 绑定。

### 7.5 同步 QA 执行 `runQA`

[tools_ask.go:213-400](../../../internal/mcpserver/tools_ask.go#L213-L400)：

- 总超时 `askTimeout = 4 分钟`；`event.NewEventBus()` 同步收集事件。
- 用同一 `uuid` requestID 建 user 消息与 assistant 消息（`Channel: "mcp"`，常量 `askChannel`）：
  - `messageService.CreateMessage(ctx, &types.Message{SessionID, Role:"user", Content:question, RequestID, IsCompleted:true, Channel:"mcp"})`
  - 再建一条 `Role:"assistant", IsCompleted:false` 占位。
- 订阅事件：
  - `EventAgentFinalAnswer`：增量拼答案，`Done=true` 时关闭 `done`；
  - `EventAgentReferences`：累积 `[]*types.SearchResult` 引用；
  - `EventAgentComplete`：记录最终答案与最终引用（非空则覆盖中间引用），关闭 `complete`；
  - `EventError`：记录错误并关闭两个通道。
- goroutine 内根据 `agent.IsAgentMode()` 分派：
  - agent 模式 → `sessionService.AgentQA(ctx, req, bus)`；
  - 否则 → `sessionService.KnowledgeQA(ctx, req, bus)`。
- 构造的 `types.QARequest` 只显式设置以下字段：

  ```go
  &types.QARequest{
      Session:            session,
      Query:              question,
      AssistantMessageID: assistantMsg.ID,
      UserMessageID:      userMsg.ID,
      CustomAgent:        agent,
      KnowledgeBaseIDs:   kbIDs,          // 可能为 nil（交给 agent 配置）
      WebSearchEnabled:   false,          // 硬编码：MCP 面永不联网
  }
  ```

  > `WebSearchEnabled:false` 是刻意的安全收口：内建 quick-answer agent 默认会开联网，不强制关掉会让一个只读端点能访问公网。重写时必须保留。

- 等待：`<-done` 后，若为 agent 模式再最多等 `askCompleteWaitTimeout = 15s` 的 `complete`；超时仅告警继续。`ctx.Done()`（4 分钟）则把 assistant 消息置为 "interrupted" 并返回 timeout 错误。
- 收尾：取答案文本（流式为空则用 complete 的 final，再为空给兜底文案），`UpdateMessage` 持久化 assistant 消息（带 `KnowledgeReferences`）。错误且无文本时把失败原因写入消息。

### 7.6 引用整理与输出

`summarizeReferences`（[tools_ask.go:417-443](../../../internal/mcpserver/tools_ask.go#L417-L443)）对 `[]*types.SearchResult` 按 chunk id（或 knowledgeID+chunkIndex）去重，产出 `askReference`；摘要 `truncateExcerpt` 最多 **300 rune**，且**不会截断到存储引用中间**（避免输出半截图片句柄）。图片单独放 `images`（`askImage{URL, Caption, OCRText}`，来自 `ImageInfo` JSON，URL 去重合并）。

structuredContent：

```json
{
  "answer": "合成答案",
  "session_id": "会话ID",
  "agent_id": "实际使用的 agent ID",
  "references": [
    {
      "knowledge_id": "文档ID",
      "knowledge_title": "标题（omitempty）",
      "chunk_id": "chunk ID",
      "score": 0.0,
      "excerpt": "≤300 rune 摘要",
      "images": [ { "url": "...", "caption": "...", "ocr_text": "..." } ]
    }
  ]
}
```

纯文本：答案 + `Sources:` 编号列表（标题缺失时用 knowledge_id；图片用 `strconv.Quote` 保留原始 URL）+ 末尾 `session_id:`。无引用时仅答案 + session_id。

---

## 8. 工具 7：`wiki_search`

- 定义：`wikiSearchTool()` [tools_wiki.go:20-34](../../../internal/mcpserver/tools_wiki.go#L20-L34)；handler [tools_wiki.go:94-127](../../../internal/mcpserver/tools_wiki.go#L94-L127)。
- 真实实现：[wiki_tools.go](../../../internal/agent/tools/wiki_tools.go) 的 `wikiSearchTool`。

### 8.1 参数

| 参数 | 类型 | 必填 | 默认/范围 | 说明 |
|------|------|------|----------|------|
| `query` | string | **是** | 词或正则 | TrimSpace 非空 |
| `regex` | bool | 否 | **缺省即默认正则语义** | 见 8.4 重要坑 |
| `knowledge_base_ids` | string[] | 否 | 作用域内全部 wiki 库 | id/精确库名 |
| `limit` | number | 否 | 默认 10，**最大 50** | |

### 8.2 作用域

`wikiScopeFromRequest(ctx, ep, requested)` [tools_wiki.go:57-69](../../../internal/mcpserver/tools_wiki.go#L57-L69) = `selectKnowledgeBases` 后 `wikiKnowledgeBases` 过滤；一个 wiki 库都没有 → `errNoWikiInScope`（提示先 list_knowledge_bases 挑 capabilities 含 wiki 的库）。

### 8.3 内部调用链（agent 工具复用）

```go
tool := tools.NewWikiSearchTool(
    s.wikiService, s.knowledgeService,
    wikiScopesFor(kbs),              // = NewWikiScopesFromKBIDs(ids)，去重后每库一个 WikiScope
    tools.NewWikiRouteResolver(),
)
callArgs := map[string]any{"query": query, "limit": limit}
if v, ok := req.GetArguments()["regex"].(bool); ok { callArgs["regex"] = v }
res, _ := tool.Execute(ctx, mustJSON(callArgs))
return toolResultFromAgentTool(withOutputInData(res), execErr), nil
```

构造函数：

```go
func NewWikiSearchTool(
    wikiService      interfaces.WikiPageService,
    knowledgeService interfaces.KnowledgeService,
    scopes           []WikiScope,                 // {KnowledgeBaseID, KnowledgeIDs, TagIDs}
    routes           *WikiRouteResolver,
) types.Tool
```

底层入参是匿名结构 `{Query any, Queries any, Limit int, KnowledgeBaseID string}`（[wiki_tools.go:812-818](../../../internal/agent/tools/wiki_tools.go#L812-L818)），`Query/Queries` 都接受「字符串或字符串数组」（`parseStringOrArray`）。逐 scope 调：

```go
pages, err := t.wikiService.SearchPages(ctx, kbID, query, params.Limit)
```

接口与实现：

```go
// interfaces.WikiPageService
SearchPages(ctx context.Context, kbID string, query string, limit int) ([]*types.WikiPage, error)
// service: 直接转 repo
func (s *wikiPageService) SearchPages(...) { return s.repo.Search(ctx, kbID, query, limit) }
```

repo `Search`（[wiki_page.go:1283-1314](../../../internal/application/repository/wiki_page.go#L1283-L1314)）：limit 再钳一次（<=0→10，>50→50），用 **PostgreSQL POSIX 正则 `~*`** 在 title/slug/summary/content 上匹配，并按命中位置打 rank（title=4 > slug=3 > summary=2 > content=1），`match_rank DESC, updated_at DESC` 排序，排除 `status='archived'`。**这是强 PG 方言，OceanBase MySQL 模式迁移时必须重写**（见适配方案 A 线）。

所有库都检索失败（如正则非法导致 PG 报错）→ 工具返回 `Success:false`，错误为各库错误拼接（[wiki_tools.go:956-957](../../../internal/agent/tools/wiki_tools.go#L956-L957)）；部分失败 → 文本尾部 `<errors>`。

### 8.4 重要坑：`regex` 参数当前是「死参数」

MCP handler 确实会把 `regex` 放进 args（[tools_wiki.go:121-123](../../../internal/mcpserver/tools_wiki.go#L121-L123)），但底层 `wikiSearchTool` 的入参结构体**根本没有 `regex` 字段**，`json.Unmarshal` 时被静默丢弃。结果：

- 无论传 `true`/`false`/不传，后端一律把 query 当 PG 正则执行；
- MCP 描述承诺的「`false` 强制字面匹配」「非法正则按字面匹配」**都不成立**——非法正则在 PG 侧报错。

重写时要么真正实现 literal（`regexp.QuoteMeta` 或改 ILIKE）并把开关透传到 repo，要么删掉该参数与误导文案。

### 8.5 输出

agent Data 仅有 `found_kbs`（slug → 命中库 id 列表，[wiki_tools.go:966-968](../../../internal/agent/tools/wiki_tools.go#L966-L968)）。MCP 层 `withOutputInData`（[tools_wiki.go:81-92](../../../internal/mcpserver/tools_wiki.go#L81-L92)）额外注入 `content = Output`，便于只读 structuredContent 的客户端拿到正文：

```json
{
  "found_kbs": { "concept/rag": ["kb-uuid"] },
  "content": "<search_results count query>...<page><knowledge_base_id>..<link>[[slug|title]]..<type>..<summary>..<match_snippet>..</page></search_results>"
}
```

---

## 9. 工具 8：`wiki_read_page`

- 定义：`wikiReadPageTool()` [tools_wiki.go:36-45](../../../internal/mcpserver/tools_wiki.go#L36-L45)；handler [tools_wiki.go:129-152](../../../internal/mcpserver/tools_wiki.go#L129-L152)。

### 9.1 参数

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `slug` | string | **是** | wiki_search/wiki_index 返回的 slug，如 `concept/rag` |
| `knowledge_base_id` | string | 否 | 仅当同 slug 在多个 wiki 中存在时用于消歧；在 MCP 层用于把 scope 收窄到单库 |

### 9.2 内部调用链（agent 工具复用）

```go
var requested []string
if kbSel != "" { requested = []string{kbSel} }
kbs := s.wikiScopeFromRequest(ctx, ep, requested)
tool := tools.NewWikiReadPageTool(s.wikiService, s.knowledgeService, wikiScopesFor(kbs), tools.NewWikiRouteResolver())
args, _ := json.Marshal(map[string]any{"slug": slug})
res, _ := tool.Execute(ctx, args)
```

构造函数签名与 wiki_search 同构：

```go
func NewWikiReadPageTool(
    wikiService interfaces.WikiPageService, knowledgeService interfaces.KnowledgeService,
    scopes []WikiScope, routes *WikiRouteResolver,
) types.Tool
```

底层入参结构（[wiki_tools.go:492-508 附近](../../../internal/agent/tools/wiki_tools.go#L500-L508)）同时接受 `slug` 与 `slugs`（字符串或数组），合并去重（`parseStringOrArray` + `dedupNonEmptyStrings`）。**MCP 层只下发单个 `slug`**；`knowledge_base_id` 不进 args，而是通过收窄 `scopes` 实现消歧。

底层行为：

- 路由优先采用已记忆的 search/link 出处（`WikiRouteResolver`），否则在每个 wiki scope 内查 slug；
- 多库同时命中同一 slug 记为歧义（`ambiguous_slugs`），歧义时把每个匹配页都返回；
- 多页正文按输出预算渲染，超预算的页正文标记 `(body omitted: output budget exhausted)`（truncated），整页未渲染记入 omitted；
- 成功 Data（[wiki_tools.go:741-746](../../../internal/agent/tools/wiki_tools.go#L741-L746)）：

```json
{
  "found_kbs": { "slug": ["kbID"] },
  "ambiguous_slugs": { "slug": ["kbA", "kbB"] },
  "truncated_slugs": ["..."],
  "omitted_slugs": ["..."]
}
```

经 `withOutputInData` 注入 `content = Output`（markdown 正文 + 元数据 + 出链）。错误（slug 全部找不到等）以 `Success:false` 返回。

---

## 10. 工具 9：`wiki_index`

- 定义：`wikiIndexTool()` [tools_wiki.go:47-55](../../../internal/mcpserver/tools_wiki.go#L47-L55)；handler [tools_wiki.go:154-183](../../../internal/mcpserver/tools_wiki.go#L154-L183)。

### 10.1 参数

| 参数 | 类型 | 必填 | 默认/范围 | 说明 |
|------|------|------|----------|------|
| `knowledge_base_id` | string | **是** | id 或精确库名 | 单库 |
| `limit` | number | 否 | 默认 50，**最大 200** | 每个页面类型分组的条目上限 |

### 10.2 内部调用链（直连 service 接口，无 agent 工具）

`wikiScopeFromRequest(..., []string{selector})` 确保该库开了 wiki 后：

```go
view, err := s.wikiService.GetIndexView(ctx, kbs[0].ID, nil, limit, "")
```

接口签名：

```go
GetIndexView(
    ctx context.Context, kbID string,
    pageTypes []string, // MCP 恒传 nil（全部类型：summary/entity/concept/...）
    limit int,
    cursor string,      // MCP 恒传 ""（首页）
) (*types.WikiIndexResponse, error)

type WikiIndexResponse struct {
    Intro  string
    Version int
    Groups []WikiIndexGroup
}
```

### 10.3 输出

```json
{
  "knowledge_base_id": "库ID",
  "intro": "wiki 导言",
  "groups": [ /* WikiIndexGroup：按页面类型分组，组内含页面条目（slug/title 等），条数受 limit 限制 */ ]
}
```

注意只回显了 `Intro/Groups`，`Version` 未对外输出。

---

## 11. 工具 10：`add_document`

- 定义：`addDocumentTool()` [tools_ingest.go:12-26](../../../internal/mcpserver/tools_ingest.go#L12-L26)；handler [tools_ingest.go:55-108](../../../internal/mcpserver/tools_ingest.go#L55-L108)。destructive=true，端点需开 ingest 能力且默认不开放。

### 11.1 参数

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `knowledge_base_id` | string | **是** | — | id 或精确库名 |
| `title` | string | **是** | TrimSpace 非空 | 标题 |
| `content` | string | 条件必填 | — | Markdown 正文；与 `url` 至少一个 |
| `url` | string | 条件必填 | — | 网页/文件 URL，异步抓取解析 |
| `publish` | bool | 否 | **true** | true=立即发布，false=草稿（仅文本路径生效） |

校验顺序：kb 选择器 → title 非空 → `TrimSpace(content)=="" && url==""` 报 "either content or url is required"。注意 **content 非空时 url 被忽略**（分支以 `if url != ""` 判定，即同时给 url+content 会走 URL 路径、content 丢弃）。

### 11.2 写鉴权（双重）

```go
kb := kbs[0]
ctx, _ = s.scopedKBContext(ctx, kb, types.OrgRoleEditor) // ① editor 级授权 + 切属主租户
if err := access.RequireKBWrite(ctx, kb); err != nil {     // ② 显式写授权校验
    return NewToolResultError("this endpoint is not allowed to write to knowledge base " + kb.ID)
}
```

`access.RequireKBWrite(ctx, kb)` 见 [kb_write.go:12](../../../internal/application/access/kb_write.go#L12)。**add 是三个写工具里唯一显式调用 `RequireKBWrite` 的**（update/delete 只靠 editor scope，见 §15）。

### 11.3 内部调用链（直连 service 接口，二选一）

URL 路径：

```go
created, err := s.knowledgeService.CreateKnowledgeFromURL(
    ctx, kb.ID,
    url,        // url
    "",         // fileName（空）
    "",         // fileType（空）
    nil,        // enableMultimodel *bool（nil，用默认）
    title,      // title
    nil,        // tagIDs []string（无标签）
    "mcp",      // channel
    nil,        // processOverrides *types.KnowledgeProcessOverrides（nil）
)
```

完整签名：

```go
CreateKnowledgeFromURL(
    ctx context.Context, kbID, url, fileName, fileType string,
    enableMultimodel *bool, title string, tagIDs []string,
    channel string, processOverrides *types.KnowledgeProcessOverrides,
) (*types.Knowledge, error)
```

文本路径：

```go
created, err := s.knowledgeService.CreateKnowledgeFromManual(
    ctx, kb.ID,
    &types.ManualKnowledgePayload{
        Title:   title,
        Content: content,
        Status:  manualStatus(req.GetBool("publish", true)), // "publish" | "draft"
        Channel: "mcp",
    },
    "mcp", // channel
)
```

`manualStatus` [tools_ingest.go:48-53](../../../internal/mcpserver/tools_ingest.go#L48-L53)：true→`types.ManualKnowledgeStatusPublish`（"publish"），false→`types.ManualKnowledgeStatusDraft`（"draft"）。`ManualKnowledgePayload` 完整字段：`Title, Content, Status string; TagIDs []string; Channel string; ProcessConfig *KnowledgeProcessOverrides`（MCP 只设了前四项）。

### 11.4 输出

```json
{
  "knowledge_base_id": "库ID",
  "document": { /* documentSummary 扁平结构，同 list_documents，含 parse_status */ },
  "note": "Indexing runs asynchronously; the document becomes searchable once parse_status is completed."
}
```

索引/解析异步进行，返回时通常尚未可检索。

---

## 12. 工具 11：`update_document`

- 定义：`updateDocumentTool()` [tools_ingest.go:28-38](../../../internal/mcpserver/tools_ingest.go#L28-L38)；handler [tools_ingest.go:110-148](../../../internal/mcpserver/tools_ingest.go#L110-L148)。仅适用于文本创建的 Markdown 文档（更新 URL 导入文档的行为由后端决定）。

### 12.1 参数

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `knowledge_id` | string | **是** | — | 文档 id |
| `content` | string | **是** | TrimSpace 非空 | 新 Markdown 正文（整页替换） |
| `title` | string | 否 | 留空则**沿用原标题** | 新标题 |
| `publish` | bool | 否 | **true** | 发布/草稿 |

### 12.2 作用域与调用链

`knowledgeInScope` 取 `existing, kb` → `scopedKBContext(ctx, kb, OrgRoleEditor)`（**无** `RequireKBWrite`）：

```go
title := TrimSpace(req.GetString("title", ""))
if title == "" { title = existing.Title }
updated, err := s.knowledgeService.UpdateManualKnowledge(
    ctx, existing.ID,
    &types.ManualKnowledgePayload{
        Title: title, Content: content,
        Status: manualStatus(req.GetBool("publish", true)),
        Channel: "mcp",
    },
)
```

接口签名：`UpdateManualKnowledge(ctx context.Context, knowledgeID string, payload *types.ManualKnowledgePayload) (*types.Knowledge, error)`。

### 12.3 输出

```json
{
  "document": { /* documentSummary */ },
  "note": "Re-indexing runs asynchronously."
}
```

改完触发异步重建索引。

---

## 13. 工具 12：`delete_document`

- 定义：`deleteDocumentTool()` [tools_ingest.go:40-46](../../../internal/mcpserver/tools_ingest.go#L40-L46)；handler [tools_ingest.go:150-175](../../../internal/mcpserver/tools_ingest.go#L150-L175)。永久删除文档及其索引数据。

### 13.1 参数

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `knowledge_id` | string | **是** | 文档 id（仅此一个参数） |

### 13.2 作用域与调用链

`knowledgeInScope` → `scopedKBContext(ctx, kb, OrgRoleEditor)`（同样**无** `RequireKBWrite`）：

```go
err := s.knowledgeService.DeleteKnowledge(ctx, existing.ID)
```

接口签名：`DeleteKnowledge(ctx context.Context, id string) error`（内部连带删除 chunk/向量/索引等）。

### 13.3 输出

```json
{
  "deleted": true,
  "knowledge_id": "文档ID",
  "title": "原标题"
}
```

---

## 14. 后端依赖接口签名汇总（重写装配清单）

`Server` 全部依赖见 [server.go:53-77](../../../internal/mcpserver/server.go#L53-L77)，构造函数 `NewServer(...)` 顺序见 [server.go:80-98](../../../internal/mcpserver/server.go#L80-L98)。12 工具实际用到的方法：

| 接口 | 用到的方法 | 服务于工具 |
|------|-----------|-----------|
| `KnowledgeBaseService` | `ListKnowledgeBases(ctx)`；`GetKnowledgeBasesByIDsOnly(ctx, ids)`；`GetKnowledgeBaseByIDOnly(ctx, id)` | 1/2/3/4/5/6/7/8/9 + scope |
| `KnowledgeService` | `ListPagedKnowledgeByKnowledgeBaseID(ctx, kbID, *Pagination, KnowledgeListFilter) (*PageResult, error)`；`GetKnowledgeByIDOnly(ctx, id) (*Knowledge, error)`；`CreateKnowledgeFromURL(...)`；`CreateKnowledgeFromManual(ctx, kbID, *ManualKnowledgePayload, channel) (*Knowledge, error)`；`UpdateManualKnowledge(ctx, id, *ManualKnowledgePayload) (*Knowledge, error)`；`DeleteKnowledge(ctx, id) error`；`GetKnowledgeTags(...)`（wiki 工具内） | 2/4/5/7/8/10/11/12 |
| `ChunkService` | 经 `GetRepository()` 做 `ListPagedChunksByKnowledgeID` / `ListChunkNeighbors` / `GetChunkByIDOnly` / 图片信息富化 | 2/3/5 |
| `WikiPageService` | `SearchPages(ctx, kbID, query, limit) ([]*WikiPage, error)`；`GetIndexView(ctx, kbID, pageTypes []string, limit int, cursor string) (*WikiIndexResponse, error)`；以及 wiki_read_page 用的取页/路由方法 | 7/8/9 |
| `SessionService` | `CreateSession(ctx, *Session) (*Session, error)`；`GetSessionByID(ctx, tenantID uint64, id) (*Session, error)`；`SetSessionOwnerID(ctx, tenantID uint64, sessionID, ownerID) error`；`AgentQA(ctx, *QARequest, *EventBus) error`；`KnowledgeQA(...) error` | 6 |
| `MessageService` | `CreateMessage(ctx, *Message) (*Message, error)`；`UpdateMessage(ctx, *Message) error` | 6 |
| `CustomAgentService` | `GetAgentByID(ctx, id) (*CustomAgent, error)` | 6 |
| `KBShareService` | 供 `access.ResolveKB` 解析共享授权 | scope 全部 |
| `TenantService` | `GetTenantByID(ctx, tenantID)` 切换属主租户配置 | scope 全部 |
| fqx `MCPUserPermissionService` | `GetUserPermission(ctx, tenantID uint64, userID string) ([]string, error)` | 全部（二开） |
| `MCPEndpointRepository` | `TouchLastUsed(ctx, endpointID)` | guard |
| `ResourceCatalog` | KB 资源/消息文件绑定反查（URL 改写授权门） | 出口改写 |
| `FileService` / `StorageBackendResolver` | 存储 URL 签名解析 | 出口改写 |

关键值对象：

- `types.Pagination{Page, PageSize int}`；`types.PageResult{Total int64, Page, PageSize int, Data any}`。
- `types.SearchTargets []*SearchTarget`，`SearchTarget{Type, KnowledgeBaseID, TenantID, KnowledgeIDs, TagIDs...}`；MCP 只造整库 target。
- `tools.WikiScope{KnowledgeBaseID string; KnowledgeIDs []string; TagIDs []string}`。
- `types.QARequest`：MCP 仅设 Session/Query/AssistantMessageID/UserMessageID/CustomAgent/KnowledgeBaseIDs/WebSearchEnabled 七个字段。

---

## 15. 重写注意事项与已知坑（务必逐条评估）

1. **三层鉴权缺一不可**：guard 的端点白名单 + fqx 用户授权（fail-closed），handler 内的 `selectKnowledgeBases/knowledgeInScope` 作用域校验，文档级操作的 `scopedKBContext` 租户切换。agent 工具内还会基于注入的 SearchTargets/WikiScope 再校验一次（第四层）。重写时每层都要保留。
2. **租户切换只在文档级调用**：list_documents/read_document/add/update/delete 用 `scopedKBContext`；检索类工具（search/grep/wiki_search）靠 SearchTargets/WikiScope 上携带的 `TenantID` 做跨租户检索，不切换 ctx 租户。`list_knowledge_bases/wiki_index/ask` 不切。
3. **写工具鉴权不对称**：add 同时做 `OrgRoleEditor` scope + `access.RequireKBWrite`；update/delete 只做 editor scope，没有再调 `RequireKBWrite`。重写若要统一，需评估这是否为有意设计。
4. **wiki_search 的 `regex` 是死参数**（§8.4）：底层无该字段，恒为 PG `~*` 正则语义；`false`/非法正则的承诺不成立。
5. **wiki 搜索是 PostgreSQL 方言**：`~*` POSIX 正则 + rank CASE + `to_tsvector('simple')`（另一处 alias ILIKE 检索）。迁 OceanBase MySQL 模式 / ES 时必须重写这一段（对应适配方案 A/B 线）。
6. **read_document 的 MCP 描述与实现不一致**：描述说 query 是「phrase」，实际是「空白分词、所有词都命中、乱序」；且底层支持的 `regex` 与 `context`（0-5）未透传到 MCP。重写时决定是补透传还是改文案。
7. **grep_chunks 不是库端正则**：RE2 正则只做后置过滤，候选来自关键词索引（无关键词索引的库降级语义索引），纯正则无字面 token 时直接报错；候选池固定扩大到 30，最终截到用户 limit。
8. **搜索结果预算裁剪**：search/read/wiki 都有 4/5 输出预算裁剪并回传 `omitted_for_budget/next_offset/truncated_slugs/omitted_slugs`，重写要保留分页/裁剪信号，否则长结果会打爆上下文。
9. **ask 是同步阻塞 + 事件总线**：4 分钟总超时、agent 模式再等 15 秒 complete；硬编码 `WebSearchEnabled:false`；agent 由端点强制（客户端不能选）；会话归属端点影子主体并做属主校验。这些都是安全/资源护栏。
10. **错误全部走 tool-result isError**，不是 transport error；handler 返回的 Go error 恒为 nil（除 panic recovery）。结构化错误（如 read 越界）也可能带 Data。
11. **出口 URL 改写对 12 工具统一生效**：正文 TextContent 与 StructuredContent 都会被 `rewriteResourceURLs` 递归改写并做逐资源授权。重写若自定义输出结构，需确认 rewriter 能遍历到新结构里的 URL 字段。
12. **schema 校验弱、钳制在代码里**：所有默认值/上下限（list 20/100、chunk 20/100、search 10/30、wiki search 10/50、wiki index 50/200）都在 handler 或 agent 工具内手工钳制，MCP JSON Schema 大多没写 minimum/maximum。重写建议把约束前移到 schema，同时保留服务端钳制。
13. **与 pristine 0.8.3 的差距**（端口升级时参考）：当前 fork 只用 `WithReadOnlyHintAnnotation`；0.8.3 另有 `readOnlyEndpointAnnotations()`，会同时清掉 destructiveHint/openWorldHint 并置 idempotentHint。0.8.3 还升级了 grep/read 的 query 语义（空白全词、乱序、next_offset 分页）——其中语义升级本分支已具备，但注解增强未合入。重写若以新版 mcp-go 为基，建议直接采用 0.8.3/更新版的完整注解集合。
14. **wiki_index 丢弃了 `Version`**，list_documents 的分页数字回显自 `PageResult` 而非入参——重写若前端依赖这些字段要注意保持。
