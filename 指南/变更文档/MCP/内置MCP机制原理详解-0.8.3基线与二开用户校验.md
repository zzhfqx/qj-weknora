# 内置 MCP 机制原理详解（v0.8.3 基线 + fqx 二开按用户校验）

> 适用分支：`pg-fqx`（基线 WeKnora v0.8.3）
> 内容：从数据模型、出站客户端、入站服务端到 Python 独立服务，端到端梳理 WeKnora 内置 MCP 的完整设计；并逐文件标注 fqx 二开新增的「按用户校验」机制。
> 阅读约定：文中所有路径均相对仓库根目录；链接按本文所在目录（`指南/变更文档/MCP/`）回退三级书写。

---

## 0. 一句话总览

WeKnora 里同时存在**三套名字都叫 MCP、但方向和职责完全不同**的东西：

| # | 形态 | WeKnora 角色 | 入口 | 代码位置 | v0.8.3 有无 | fqx 改动 |
|---|---|---|---|---|---|---|
| ① | **出站 MCP 客户端** | Client：Agent 去连外部 MCP 服务器，把外部工具挂进智能体 | 设置页「MCP 服务」CRUD | [internal/mcp/](../../../internal/mcp/)、[internal/agent/tools/mcp_*.go](../../../internal/agent/tools/) | 有 | 无（随基线） |
| ② | **内置 MCP 服务端** | Server：外部 MCP 客户端（Claude Desktop / Cursor / OpenCode/千机）连进 WeKnora 用知识库工具 | `POST/GET/DELETE /mcp/:endpoint_id` | [internal/mcpserver/](../../../internal/mcpserver/) | 有 | **重点：新增按用户身份与授权裁决** |
| ③ | **Python 独立 MCP Server** | Server（独立进程）：把 WeKnora REST API 包装成 MCP 工具，走租户 API Key | 容器 `mcp`，端口 8082 | [mcp-server/](../../../mcp-server/) | 有 | 仅多了两个备份/英文版文件，无机制改动 |

fqx 二开的全部增量都在**形态②**上，核心是回答一个官方基线没回答的问题：

> 官方设计中，一枚端点 Bearer Token 代表一台机器（工作空间），**所有经该 token 进来的最终用户共享同一份知识库可见范围**。千机/OpenCode 网关多用户复用同一端点时，无法做到"A 用户只能看共享给他的库"。二开在不改动官方信任模型的前提下，加了第二层「用户身份票据 + 按用户授权交集」。

---

## 1. 全景架构图

```
┌──────────────────────────────────────────────────────────────────────────┐
│ 形态① 出站（Agent 当 MCP 客户端）                                          │
│                                                                            │
│  用户在设置页配置外部 MCP 服务 ──► mcp_services 表                          │
│        │                                                                   │
│        ▼                                                                   │
│  MCPManager（连接池/单飞/OAuth） ──► 外部 MCP Server（SSE/Streamable）      │
│        │                                                                   │
│        ▼ 惰性发现（不直连、不灌全量 schema）                                 │
│  Agent 工具注册表只装两个元工具：discover_mcp_tools / call_mcp_tool         │
│  执行时：启用策略复查 → 人工审批门(可选) → OAuth 刷新 → CallTool            │
└──────────────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────────────┐
│ 形态② 入站（WeKnora 当 MCP 服务端）                                        │
│                                                                            │
│  外部 MCP 客户端（千机网关/OpenCode/Claude Desktop…）                       │
│   │                                                                        │
│   │  Authorization: Bearer mcp_xxx            （端点令牌，L1）             │
│   │  X-User-Credential: <HS256 JWT>           （用户票据，L2，fqx 新增）   │
│   ▼                                                                        │
│  /mcp/:endpoint_id                                                         │
│   ├─ L1 中间件 MCPEndpointAuth  → 端点? 启用? token 哈希常量时间比对        │
│   │      └─ 合成机器用户 mcp-<epid> + Principal + APIKeyScope             │
│   ├─ L2 中间件 MCPUserIdentity  → 票据验签? tid/aud 与端点交叉校验?【二开】│
│   │      └─ ctx 写入外部 user_id                                           │
│   ▼                                                                        │
│  mcp-go Streamable HTTP（stateless）                                       │
│   ├─ tools/list  → filterTools：端点白名单 ∩ 用户有授权库 ⇒ 否则空 []      │
│   ├─ tools/call  → guardTool：白名单 → 用户授权 → 限流 → 审计 → 执行       │
│   └─ 12 个内置工具（检索/问答/Wiki/写入）复用内部 service 层                │
│                                                                            │
│  知识库范围 = 端点 KB 白名单 ∩ mcp_user_permission 用户授权表 ∩             │
│              access.ResolveKB 实时重授权（含组织共享库）                  │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## 2. 数据模型

### 2.1 表清单

| 表 | 方向 | 作用 | 定义文件 | 引入迁移 |
|---|---|---|---|---|
| `mcp_services` | 出站 | 外部 MCP 服务配置（传输方式、URL、认证、stdio、builtin 标记） | [types/mcp.go:24](../../../internal/types/mcp.go) | 早期基线；`is_builtin` 列见 000017 |
| `mcp_tool_approvals` | 出站 | 每服务每工具的**启用开关 + 人工审批策略**（工具清单本身来自 ListTools，此表只存覆盖项） | [types/mcp.go:132](../../../internal/types/mcp.go) | 000042（审批）/000091（enabled） |
| `mcp_oauth_clients` / `mcp_oauth_tokens` | 出站 | OAuth 动态注册客户端、按 (租户,用户,服务) 的令牌 | [types/mcp_oauth.go:19,77](../../../internal/types/mcp_oauth.go) | 000062 + 000074（刷新租约）+ 000118（时间戳时区） |
| `mcp_metadata` | 出站 | 显式同步的工具目录快照（含指纹、8MiB 上限、OAuth 按 principal 隔离） | [types/mcp_metadata.go:25](../../../internal/types/mcp_metadata.go) | 000092 |
| `mcp_endpoints` | **入站** | 工作空间发布的 MCP 端点（信任单元：token 哈希、KB 范围、工具白名单、限流、默认 agent） | [types/mcp_endpoint.go:21](../../../internal/types/mcp_endpoint.go) | 000102 |
| **`mcp_user_permission`** | **入站（二开）** | **每工作空间每外部用户一行的「可读知识库 id 列表」** | [types/mcp_user_permission.go:20](../../../internal/types/mcp_user_permission.go) | **000216（PG）/ sqlite 000016** |

### 2.2 出站配置实体 `MCPService`

[internal/types/mcp.go:24-145](../../../internal/types/mcp.go) 关键字段：

- `TransportType`：`sse` / `http-streamable` / `stdio`。**stdio 在管理器中被硬性禁用**（安全考虑，见 [manager.go:97-100](../../../internal/mcp/manager.go)），类型保留只为兼容旧数据与管理面展示。
- `AuthConfig`：四种策略 —— `""`(无) / `api_key`（静态头，默认 `X-API-Key`，头名可配）/ `bearer`（静态 `Authorization`）/ `oauth`（MCP OAuth 2.0 授权码 + 动态客户端注册 + PKCE，按用户走）。
- 密钥字段 `APIKey/Token` 通过 GORM `Valuer/Scanner` 落盘前做 **AES-256-GCM 加密**（复用 `SYSTEM_AES_KEY`，旧明文行宽松兼容，密钥丢失时降级为空并打日志）——[mcp.go:222-272](../../../internal/types/mcp.go)。
- 响应 DTO 刻意不携带密钥（[dto/mcp.go](../../../internal/handler/dto/mcp.go) 是编译期不变量），密钥增删走独立子资源 `PUT /mcp-services/:id/credentials`。

### 2.3 入站端点实体 `MCPEndpoint`（官方信任单元）

[internal/types/mcp_endpoint.go](../../../internal/types/mcp_endpoint.go)：

| 字段 | 语义 |
|---|---|
| `TokenHash` | 明文 token 只在创建/轮换时返回一次；库存 SHA-256 hex；前缀 `mcp_`，与租户 API Key（`sk-`）、embed token（`em_`）肉眼可分 |
| `TokenHint` | 前 12 字符，UI 展示用 |
| `KnowledgeBaseIDs` | 端点级 KB 白名单（jsonb 数组）；**空=工作空间全部自有+共享库** |
| `Tools` | 12 个内置工具名的白名单，写入时经 `NormalizeMCPEndpointTools` 丢弃未知/重名 |
| `DefaultAgentID` | `ask` 工具用的智能体；调用方不能任意指定 agent |
| `RateLimitPerMinute` | 默认 60，上限 6000 |

端点即「一张缩权 API Key + 一个 URL 路径」，这是官方设计的核心：**端点是信任边界，token 认证的是端点而不是人**。

### 2.4 二开授权表 `mcp_user_permission`（fqx 新增）

[internal/types/mcp_user_permission.go](../../../internal/types/mcp_user_permission.go) + [迁移 000216](../../../migrations/versioned/000216_mcp_user_permission.up.sql)：

```sql
CREATE TABLE mcp_user_permission (
  id                 VARCHAR(36) PRIMARY KEY DEFAULT uuid_generate_v4(),
  tenant_id          BIGINT NOT NULL,
  user_id            VARCHAR(128) NOT NULL DEFAULT '',   -- 千机外部用户 id
  knowledge_base_ids JSONB NOT NULL DEFAULT '[]',        -- 可读知识库 id 数组
  ...
);
CREATE UNIQUE INDEX idx_mcp_user_permission_user ON mcp_user_permission (tenant_id, user_id);
```

三条关键设计纪律：

1. **weknora 不建影子用户**：`user_id` 是外部（千机）标识，与官方 users/tenants 表无外键、无写依赖 → 跟上游升级不产生结构冲突。
2. **该表由千机侧业务流程（建库/共享/收回）upsert 写入，weknora 只读**；仓库里只有一个仓储方法 `GetByUser`（[repository/mcp_user_permission.go](../../../internal/application/repository/mcp_user_permission.go)），没有任何创建/更新 API。
3. **二元 fail-closed 语义**：无行 或 列表为空 ⇒ 该用户看不到任何 MCP 知识库工具。不存在"默认放行"。

---

## 3. 形态① 出站 MCP 客户端（Agent 工具源）端到端

### 3.1 管理面

路由集中在 [router/routes_infra.go:147-198](../../../internal/router/routes_infra.go)，组 `/api/v1/mcp-services`，走 `apiKeyManageMCPServices` 能力门控：

- 写操作（POST/PUT/DELETE、test、credentials、tool-approvals）Admin；读 Viewer。
- OAuth 回调 `GET /api/v1/mcp-oauth/callback` 在组外（浏览器重定向，无会话）。
- 凭证子资源 `PUT /:id/credentials`、`DELETE /:id/credentials/:field` 独立成 handler（[mcp_credentials.go](../../../internal/handler/mcp_credentials.go)）。
- embed 渠道另有同构的 OAuth 授权 URL/状态/裁决路由（routes_agent.go:247-250）。

Handler 723 行在 [handler/mcp_service.go](../../../internal/handler/mcp_service.go)，Service 实现在 [application/service/](../../../internal/application/service/) 同名领域，Repository 落 mcp_services 等表。

### 3.2 连接管理：`MCPManager`

[internal/mcp/manager.go](../../../internal/mcp/manager.go)：

- **连接缓存**：`map[cacheKey]MCPClient`。普通服务 key=serviceID；**OAuth 服务 key=`serviceID \x00 principal.StorageID()`**，每个用户身份一条独立连接（[manager.go:75-80](../../../internal/mcp/manager.go)）。
- **版本失效**：连接带 `version=service.UpdatedAt`，配置一改，旧连接被断开重建。
- **单飞（singleflight）**：并发首个请求只发起一次连接（`connecting` map + `pending.done` channel），后来者等同一结果；等待者可独立取消（`ctx.Err()`）；连接协程用的是脱离请求 deadline 的生命周期 ctx，SSE 建连不被单次对话超时杀死。
- **空闲清理**：5 分钟扫描掉线连接；`CloseClient(serviceID)` 用前缀匹配一次回收某 OAuth 服务的所有 per-principal 连接。
- **stdio 拒绝**：管理器入口直接返回错误，杜绝配置侧命令注入面。

### 3.3 传输与认证：`mcpGoClient`

[internal/mcp/client.go](../../../internal/mcp/client.go)（687 行）基于 `mark3labs/mcp-go`：

- SSE → `NewSSEMCPClient`；Streamable HTTP → `NewStreamableHttpClient`；静态认证在传输层注入头（`applyAuthHeaders`，client.go:86）。
- OAuth → `NewOAuthSSEClient` / `NewOAuthStreamableHttpClient` + `transport.OAuthConfig`，配套四个文件：
  - [oauth_state.go](../../../internal/mcp/oauth_state.go)：state 与"会话内授权尝试"两级 Redis 存储（一次性 Take、attempt 完成标记、GC）；
  - [oauth_manager.go](../../../internal/mcp/oauth_manager.go)：发起授权（RFC 9728 元数据发现 → 动态注册 → PKCE 授权 URL）、回调完成、状态查询、撤销；
  - [oauth_lifecycle.go](../../../internal/mcp/oauth_lifecycle.go)：调用前 `ensureFresh`，过期自动刷新；**刷新加分布式租约**（000074）防多副本同时刷令牌；区分永久失败（需重新授权）与临时失败（可重试）；
  - [oauth_tokenstore.go](../../../internal/mcp/oauth_tokenstore.go)：令牌落库适配器。
- 服务端返回 401 且声明 OAuth 资源保护时，包装成 `OAuthRequiredError`（client.go:124），向上冒泡到对话里的「MCP 授权卡片」（前端 [McpOAuthCard.vue](../../../frontend/src/views/chat/components/McpOAuthCard.vue)），用户在聊天中点授权即可继续，不用跑设置页。
- 接口 `MCPClient`（client.go:25）封装 Initialize/ListTools/CallTool/ReadResource/IsConnected 等，方便测试替身。

### 3.4 工具如何进 Agent：目录代理模式（重点设计）

不是"每配一个服务就把它所有工具的完整 JSON Schema 注册给 LLM"——那会撑爆工具表、还要在启动时连所有服务器。实际做法（[mcp_tool.go:505-560](../../../internal/agent/tools/mcp_tool.go)）：

1. Agent 构建时调 `RegisterMCPTools`，工具注册表里**只装两个元工具**：
   - `discover_mcp_tools`（[definitions.go:10](../../../internal/agent/tools/definitions.go)）：分页浏览/搜索已配置服务的工具摘要；
   - `call_mcp_tool`（definitions.go:11）：按 `服务+工具` 引用执行。
2. 目录数据 `MCPCatalog`（[mcp_catalog.go:126](../../../internal/agent/tools/mcp_catalog.go)）按需加载：
   - **快照模式**：读 `mcp_metadata` 表（管理面点"刷新工具"时显式同步），快照标记 stale 则报错引导去刷新；
   - **live 模式**：当场连服务 ListTools，并可回写快照；
   - OAuth 服务在非交互上下文里目录缺失时直接报"请先授权"。
3. 模型先 discover 再 call；call 时才构造真正的 `MCPTool` 包装器执行。

### 3.5 单次外部工具调用的六道关卡

[`MCPTool.Execute`](../../../internal/agent/tools/mcp_tool.go)（mcp_tool.go:113 起）：

1. **启用策略复查**（`approval.MCPApproval.IsEnabled`）：注册时查一次、执行时再查一次——运行中的 agent 实例可能比设置改动活得久，禁用必须当轮生效（mcp_tool.go:119-128）；
2. 参数 JSON 解析 + JSON Schema 校验（`jsonschema/v6`，懒加载 `sync.Once`）；
3. **人工审批门**：`RequireApproval=true` 的工具经 EventBus 发 pending，阻塞等待用户在 UI 批准/拒绝/改参数（issue #1173）；等待用 ApprovalCtx（可超 60s 工具超时），取消信号照常传播；
4. OAuth 取连接（`getOrCreateMCPClientWithOAuthRetry`，必要时触发 401→刷新/重新授权流程）；
5. CallTool，失败重试（连接陈旧则断开重连一次，见 loadMCPServiceTools 的同款处理）；
6. 结果处理：文本提取、**外部图片数据消毒**（redactImageData）、工具名 `mcp_<服务名>_<工具名>` 清洗 + 64 字符截断 + 注册表 first-wins 防抢名（GHSA-67q9-58vj-32qx）；描述统一加 `[MCP Service: xxx (external)]` 前缀以缓解间接提示注入。

注册入口在 [agent_service.go:339](../../../internal/application/service/agent_service.go)，每个 agent 按其可见的服务集合 scoped 安装。

---

## 4. 形态② 内置 MCP 服务端（入站）——二开主战场

### 4.1 端点管理面

- 路由 `/api/v1/mcp-endpoints`（[routes_mcp_endpoint.go:17-29](../../../internal/router/routes_mcp_endpoint.go)）：Viewer 可读，Admin 可写，且整个组要求 `manage_channels` 能力（定位同 embed 渠道）。
- Service：[application/service/mcp_endpoint.go](../../../internal/application/service/mcp_endpoint.go)。
- 令牌生命周期：
  - 生成：`crypto/rand` 32 字节 → base64url → 加 `mcp_` 前缀（mcp_endpoint.go:61-67）；
  - 存储：只存 SHA-256；UI 只见 hint；创建/轮换时明文返回**仅此一次**；
  - 认证：按哈希取行后，用 `subtle.ConstantTimeCompare` 比对路径里的 endpoint id，防跨端点计时探测（mcp_endpoint.go:267-288）；
  - 禁用/删除/不存在对调用方不可区分地收敛为 401/403。
- **越权放大防护** `requireKeyCoversEndpoint`（mcp_endpoint.go:239-265）：用缩权租户 API Key 来建端点时，端点的 KB 范围必须是该 Key 白名单的子集，所需 capability（retrieve/chat/ingest/read_agents）也必须被 Key 持有——新凭证不能比签发者权限大。
- 建/改时逐 KB 走 `access.ResolveKB` 重授权，共享库可解析、无权限的 id 一律按 not found 拒绝。

### 4.2 HTTP 挂载与两层中间件

服务面在全局 Auth 之前注册（与 embed 公共路由同策略），[routes_mcp_endpoint.go:34-51](../../../internal/router/routes_mcp_endpoint.go)：

```go
// v0.8.3 基线
r.Handle(method, "/mcp/:endpoint_id", auth, h)
// fqx 二开
r.Handle(method, "/mcp/:endpoint_id", auth, identity, h)
//                                L1 端点令牌 ──┘    └── L2 用户票据
```

**L1：`MCPEndpointAuth`**（[middleware/mcp_endpoint_auth.go](../../../internal/mcp/mcp_endpoint_auth.go)，95 行，基线能力，未被二开修改）：

1. 只接受 `Authorization: Bearer`，拒绝 query string token（不进访问日志）；
2. `Authenticate(endpointID, token)` 验端点；查租户状态（inactive/disabled → 403）；
3. 合成**机器用户**：`ID=Username=mcp-<endpointID>`、邮箱 `mcp-<id>@mcp.local`，角色 Viewer；
4. 写入 Principal `MCPEndpointPrincipal(tenant, endpoint)`；
5. 由端点工具清单反推 API Key 风格 scope（`MCPEndpointScope`，[mcp_endpoint_tools.go:148](../../../internal/types/mcp_endpoint_tools.go)），于是下游所有 service 层的 KB/能力校验**原封不动复用**，无需为 MCP 写第二套权限。

**L2：`MCPUserIdentity`**（[middleware/mcp_user_identity.go](../../../internal/middleware/mcp_user_identity.go)，94 行，**纯二开**，详见第 5 节）。

### 4.3 协议层：mcp-go + Streamable HTTP stateless

[mcpserver/server.go](../../../internal/mcpserver/server.go) 用 `mark3labs/mcp-go`：

- `server.NewMCPServer(..., WithToolCapabilities(false), WithRecovery(), WithToolFilter(s.filterTools), WithToolHandlerMiddleware(s.guardTool), WithInstructions(...))`；
- 传输 `NewStreamableHTTPServer(..., WithStateLess(true))`：**无服务端会话亲和**，任何副本可接任何请求（水平扩展前提；初始化/session 状态在客户端侧）；
- POST/GET/DELETE 同路径：POST 走 JSON-RPC（initialize/tools/list/tools/call 等），GET 是 SSE 流，DELETE 结束流。

### 4.4 12 个内置工具与四组能力映射

目录常量在 [types/mcp_endpoint_tools.go:21-61](../../../internal/types/mcp_endpoint_tools.go)，处理器挂在 server.go:286-301：

| 组 | 工具 | 下游 | 能力 |
|---|---|---|---|
| retrieve | list_knowledge_bases / search_knowledge / grep_chunks / list_documents / read_document | kb/knowledge/chunk 检索服务 + agent tools 的 WikiScope | retrieve |
| chat | ask | 解析端点默认 agent → 建/续 session → 跑 QA，返回答案+引用+`session_id`（[tools_ask.go](../../../internal/mcpserver/tools_ask.go)） | retrieve + chat + read_agents |
| wiki | wiki_search / wiki_read_page / wiki_index | wiki 服务（只暴露有 wiki 的库） | retrieve |
| ingest | add_document / update_document / delete_document（**Destructive，新建端点默认不勾**） | 写入服务 | ingest |

端点勾选的工具组 → `MCPEndpointCapabilitiesForTools` 反推 capabilities → 注入 L1 scope → 下游 API 层按同一套规则裁决。`ask` 的 agent 还要过 `MCPEndpointAgentAllowed`：租户自有 agent 或"面向用户的内置 agent"才允许，wiki fixer、skill installer 这类带写/shell 工具的内部 agent 永远不可达。

### 4.5 请求裁决链（guardTool / filterTools / scope）

#### tools/list —— `filterTools`（server.go:162-186）

```
端点缺失 → 返回 []          ┐
用户身份/授权查询失败 → []  ├─ 全部返回非 nil 空切片
用户授权 KB 列表为空 → []   ┘  （序列化为 "tools": [] 而非 null，
否则按端点白名单过滤目录      OpenCode 等客户端会把 null 当 list 失败）
```

#### tools/call —— `guardTool`（server.go:205-224）

顺序固定：

1. ctx 有端点；
2. **端点工具白名单**（防按名字直调隐藏工具）；
3. **用户授权非空**（二开；错误文案统一 `tool not permitted for this user`，不回吐授权明细）；
4. **限流**：Redis 滑动窗口（ZSET，[ratelimit/limiter.go](../../../internal/ratelimit/limiter.go)），key 按端点，窗口 1 分钟，额度取端点配置；
5. `touchLastUsed`：每端点每分钟最多落一次 `last_used_at`（sync.Map 节流 + 后台协程，不阻塞响应）；
6. 审计日志（见 5.4）；
7. 执行真实 handler；
8. `rewriteResourceURLs`：结果中的文件/图片 URL 改写为带端点可达性的形式（[resource_urls.go](../../../internal/mcpserver/resource_urls.go)）。

#### 知识库范围三重收窄 —— `allowedKnowledgeBases`（[scope.go:24-59](../../../internal/mcpserver/scope.go)）

```
最终可见库 = 端点 KnowledgeBaseIDs（空=全工作空间）
           ∩ mcp_user_permission.knowledge_base_ids（二开，空=啥也没有）
           ∩ access.ResolveKB 实时重授权通过的库（含组织共享；失效/跨租户 id warn-and-drop）
```

- 端点未限定时：列工作空间全部库后与用户集做交集（`intersectKnowledgeBases`）；
- 端点限定时：只对交集中的 id 逐个重授权，不在用户列表里的端点 id **静默跳过**（不算异常、不打 warn）；
- 文档级操作再过一次 `knowledgeInScope`（按文档反查所属库，且校验文档 tenant 与库 owner 一致），杜绝用 knowledge_id 越权；
- **共享库的租户切换**：`scopedKBContext` 把执行上下文切到库 owner 租户（数据物理存在 owner 名下），TenantInfo/模型/存储配置都按 owner 解析（scope.go:122-169）。

### 4.6 各工具处理器的复用方式

- `search_knowledge`/`grep_chunks`：内部直接复用 agent 工具层的检索能力（SearchTargets 带每个库的 owner TenantID），只查建了向量/关键词索引的库（`retrievableKnowledgeBases`）；
- `read_document`：分页 offset/limit + 可选关键词；
- `ask`：调用方不能指定任意 agent（`resolveAskAgent` 只认端点配置），session 按端点+调用隔离，续问传 `session_id`；
- 写工具：经 scoped 上下文落到 knowledge 服务，与 REST API 共用同一套写入校验。

---

## 5. fqx 二开：按用户校验机制（逐层拆解）

二开没有改官方的任何信任假设，而是在其后**串了一层独立的用户信任链**。改动共 6 个新文件 + 4 处插入点，全部中文注释。

### 5.1 票据层：`X-User-Credential`（L2 中间件）

[internal/middleware/mcp_user_identity.go](../../../internal/middleware/mcp_user_identity.go) + [service/mcp_user_ticket.go](../../../internal/application/service/mcp_user_ticket.go)。

**票据形态**：HS256 JWT，密钥复用 `JWT_SECRET`，claims：

| claim | 含义 |
|---|---|
| `type` | 固定 `mcp_user_identity`，Parse 时拒绝与登录 token / sandbox 票据等混用 |
| `sub` | 千机外部用户 id |
| `tid` | weknora 工作空间（租户）id |
| `aud` | **绑定的 MCP 端点 id** |
| `jti` | 随机 id |
| `iat/exp` | 默认 TTL **5 分钟** |

**验签纪律**（`ParseMCPUserTicket`，mcp_user_ticket.go:60-92）：强制 HMAC 算法（拒 alg 混淆）、验过期、验 type、三个必填字段非空，任一不满足即错。

**谁签发？** 关键：`IssueMCPUserTicket` 在 Go 代码内**没有任何业务调用方**（只有测试）。它是给**进程外网关（千机/OpenCode）**准备的契约——网关与 weknora 配置同一个 `JWT_SECRET`，在转发每个 MCP 请求前按本机时钟即时签发。短 TTL 的依据：鉴权发生在 HTTP 请求建立时刻，不影响其后 SSE 长连，同时把泄露窗口压到最小。

**中间件裁决**（mcp_user_identity.go:29-77）：

1. 无头：默认 401；灰度开关 `MCP_REQUIRE_USER_IDENTITY=false` 时仅告警放行（发布过渡用，.env.example 明确"稳定后必须移除"）；
2. 票据非法/过期：401；
3. **交叉校验**：`claims.tid == 端点.tenantID && claims.aud == 端点.id`——A 工作空间/端点签的票不能在 B 面重放；
4. 通过后把 user_id **同时写入 gin keys 和 request context**（与 applyAuthSession 同纪律，两种读取风格的下游拿到同一值）。

user_id 的唯一可信来源就是这张已验签票据；常量注释明确禁止从请求体/URL/工具参数取值（[types/const.go:103-106](../../../internal/types/const.go)）。

### 5.2 授权层：`mcp_user_permission` 查询服务

[service/mcp_user_permission.go](../../../internal/application/service/mcp_user_permission.go)：

- 接口仅一法：`GetUserPermission(ctx, tenantID, userID) ([]string, error)`；
- **进程内短 TTL 缓存 45s**（落在方案建议的 30–60s 区间）：千机侧建库/共享/收权最迟 45s 在 MCP 面生效；命中返回切片副本防污染；
- **无负缓存**：空结果（新用户首次提问是正常路径）不缓存，并顺手删掉旧条目——**撤销授权/删行在下次请求立即生效**，不用等 TTL；
- 后台 sweep 协程定时清过期项，防外部用户无限增长撑爆 map（生命周期同 mcpserver 限流清理器）；
- DB 错误向上返回 → 调用方 fail-closed，绝不"查不到就放行"。

### 5.3 裁决层：三处强制点

| 位置 | 无身份/服务未接线 | 授权为空 | 代码 |
|---|---|---|---|
| tools/list `filterTools` | warn + `[]` | `[]` | server.go:162-186 |
| tools/call `guardTool` | `tool not permitted for this user` + 审计 | 同左 | server.go:192-224（裁决体 198-215） |
| 每个具体工具的 KB 解析 `userKnowledgeBaseIDs` | error 中断 | 交集为空 → "no knowledge base is available" | scope.go:82-97 |

三层的意义：list 层让无权限用户看到的就是个干净的零工具服务（UI 不报错）；call 层防绕过 list 按名直调；scope 层保证即便工具本身不涉及 KB 选择（如 ask 不带 kb 参数），可见域仍被锁死。两个哨兵错误 `errMCPUserIdentityRequired` / `errMCPUserPermissionUnavailable` 把"没装这层"也定义成拒绝而非放开。

### 5.4 审计层：`auditToolCall`

server.go:230-261，每次 tools/call 一条结构化日志：

```
[mcp-audit] tool call allowed|denied
  user_id / endpoint_id / tool / verdict
  knowledge_base_id / knowledge_id   （从工具参数提取）
  request_id
  deny_reason = tool_disabled | identity_or_permission_unavailable
              | no_user_permission | rate_limited
```

allow 用 Info、deny 用 Warn，可按千机外部 user_id 完整追溯。

### 5.5 二开的接线点（DI 装配）

[container.go](../../../internal/container/container.go) 四处：

- 171 行：`repository.NewMCPUserPermissionRepository`；
- 252 行：`service.NewMCPUserPermissionService`；
- `mcpserver.NewServer` 参数表新增 `userPermissionSvc`（server.go:91,110）；
- 路由链插入 `identity` 中间件（routes_mcp_endpoint.go:47-49）。

因为全部通过 `interfaces.MCPUserPermission*` 注入（[interfaces/mcp_user_permission.go](../../../internal/types/interfaces/mcp_user_permission.go)），官方单测可传 nil/替身；生产装配缺服务时按 5.3 的哨兵 fail-closed。

### 5.6 二开边界外的职责划分（千机侧必须做的事）

1. 建库/共享/收权流程中 **upsert `mcp_user_permission`**（唯一键 `(tenant_id,user_id)`）；
2. 网关持有 `JWT_SECRET`，逐请求签 `X-User-Credential`（Go 可直接调 weknora 暴露的同款函数逻辑；其他语言按上表 claims 实现 HS256）；
3. 端点 token 仍是 weknora 管理面创建的 `mcp_` Bearer，网关代表集群持有。

---

## 6. 形态③ Python 独立 MCP Server

[mcp-server/weknora_mcp_server.py](../../../mcp-server/weknora_mcp_server.py)（1151 行；`mcp.py` 是其英文同源版，`weknora_mcp_server_1016.py` 是 10-16 备份，非机制差异）：

- **定位**：独立容器（compose `mcp` 服务，`profiles: full`，宿主 8082→容器 8000），用 `WEKNORA_API_KEY`（租户 API Key）调 WeKnora REST，把**租户管理 + 知识库 + 文档 + 混合检索 + 聊天**包成 MCP 工具，工具集比内置 Go 服务端大（含 create_tenant、建库、文件/URL/文本入库等）。
- **认证**：stdio 无认证；SSE/HTTP 启动时强制要求 `MCP_SERVER_AUTH_TOKEN`（共享密钥，`Authorization: Bearer` 或 `X-MCP-Auth-Token`），缺失即拒绝启动；另有上传目录白名单 `MCP_ALLOWED_UPLOAD_DIRS` 与文件路径安全校验（upload_paths.py、test_file_path_security.py）。
- 与形态②的关系：**两条独立产品线**。Python 版是"REST 全量包装器"，粗粒度、以租户 Key 为身份；Go 内置版是"精选知识库工具 + 端点细粒度授权 + 多用户可区分"。二开的用户校验只覆盖 Go 内置版，Python 版仍是一 Key 一租户，多用户场景不应使用。

---

## 7. v0.8.3 基线 vs pg-fqx 二开：文件级差异清单

### 7.1 新增文件（二开）

| 文件 | 职责 |
|---|---|
| [types/mcp_user_permission.go](../../../internal/types/mcp_user_permission.go) | 授权实体 + ctx 取 user_id 的 fail-closed helper |
| [types/interfaces/mcp_user_permission.go](../../../internal/types/interfaces/mcp_user_permission.go) | 仓储/服务两个接口 |
| [application/repository/mcp_user_permission.go](../../../internal/application/repository/mcp_user_permission.go) | 唯一读方法 GetByUser |
| [application/service/mcp_user_permission.go](../../../internal/application/service/mcp_user_permission.go) | 45s 缓存、无负缓存、撤销即时失效、sweep |
| [application/service/mcp_user_ticket.go](../../../internal/application/service/mcp_user_ticket.go) | HS256 用户票据签发/验签（type 隔离） |
| [middleware/mcp_user_identity.go](../../../internal/middleware/mcp_user_identity.go) | L2 中间件 + 灰度开关 |
| [migrations/versioned/000216_mcp_user_permission.up/down.sql](../../../migrations/versioned/) | PG 建表（jsonb、唯一索引、COMMENT） |
| [migrations/sqlite/000016_mcp_user_permission.up/down.sql](../../../migrations/sqlite/) | sqlite 对应 |
| mcpserver/user_gate_test.go | 8 个裁决场景（无身份全隐藏、交集、按名直调拒绝等） |
| mcp_user_identity_test.go / mcp_user_permission_test.go / mcp_user_ticket_test.go | 中间件/服务/票据单测 |

### 7.2 修改文件（插入式，无删除官方逻辑）

| 文件 | 改动 |
|---|---|
| [router/routes_mcp_endpoint.go](../../../internal/router/routes_mcp_endpoint.go) | 服务面链路上插入 `identity`（基线只有 `auth`） |
| [mcpserver/server.go](../../../internal/mcpserver/server.go) | 注入 userPermissionSvc；filterTools/guardTool 增加用户裁决与审计；空切片纪律 |
| [mcpserver/scope.go](../../../internal/mcpserver/scope.go) | allowedKnowledgeBases 改为「端点集 ∩ 用户集」，新增 intersect/userKnowledgeBaseIDs |
| [container/container.go](../../../internal/container/container.go) | 4 处 DI 接线 |
| [types/const.go](../../../internal/types/const.go)、[types/context_clone.go](../../../internal/types/context_clone.go) | 新 ContextKey（日志克隆白名单同步登记） |
| [.env.example](../../../.env.example) | `MCP_REQUIRE_USER_IDENTITY` + JWT_SECRET 用途注释 |

### 7.3 基线版本差提示（非二开有意改动，升级时注意）

当前分支的入站服务端相对 00008.3 正式版**少了若干后期修正**（基线代码在分叉点之后的演进），升级合并时建议补回：

- `tools_retrieve.go`：0.8.3 用 `readOnlyEndpointAnnotations()` 同时把 destructiveHint/openWorldHint 置对（二开版本只设了 read-only，客户端可能仍把工具标成"破坏性"）；query 语义 0.8.3 已升级为"空白分词、每词必含、乱序、分页 next_offset"，二开版还是"整短语匹配"；
- 0.8.3 mcpserver 下 8 个 `*_test.go`（annotations、ask_image、read_document、wiki、resource_urls、scope、server、grep_pattern）在二开分支被移除，仅留 user_gate_test.go；
- 出站侧 0.8.3 的 mcp_call_retry / structured_result / resource / timeout / oauth / panic / resume_mcp 等测试文件同样未落二开分支。

---

## 8. 典型时序

### 8.1 千机用户经网关调用 `ask`（二开后的完整链路）

```
千机用户 ──► 千机网关
  ① 网关确认用户身份与已共享 KB（读千机自己的库；建库/共享时已 upsert
     weknora.mcp_user_permission）
  ② 用共享 JWT_SECRET 签 5 分钟票据 {sub:user, tid:租户, aud:端点id}
  ③ 转发：Authorization: Bearer mcp_<端点token>
          X-User-Credential: <jwt>
        POST http://weknora/mcp/<端点id>
───────► weknora
  L1 MCPEndpointAuth：token 哈希比对 → 机器用户+scope(retrieve,chat,read_agents)
  L2 MCPUserIdentity：验票 + tid/aud 交叉校验 → ctx 写 user_id
  mcp-go：JSON-RPC tools/call(ask, {...})
  guardTool：白名单✓ → 查 mcp_user_permission(45s 缓存) 非空✓ →
             滑窗限流✓ → 审计 allow → touch last_used
  scope：端点KB ∩ 用户KB ∩ ResolveKB ⇒ 实际可见库
  tools_ask：解析端点默认 agent → 取/建 session → QA 检索（owner 租户上下文）
  返回：答案 + 引用 + session_id；结果 URL 经 rewriteResourceURLs
```

### 8.2 Agent 调用外部 MCP 工具（出站）

```
LLM 决定用外部工具
  → call_mcp_tool(service, tool, args)
  → MCPCatalog 校验该工具确实在 discover 过的清单里
  → MCPTool.Execute：enabled 复查 → 需审批? 发 EventBus 等用户 →
     MCPManager.GetOrCreateClient（OAuth 按用户取连接；ensureFresh 必要时刷新/租约）
  → CallTool → (401/OAuthRequired?) 会话内授权卡片 → 用户授权后重放
  → 文本/图片结果消毒 → ToolResult 回灌 LLM
```

---

## 9. 安全设计要点汇总

1. **双层信任，方向正交**：L1 证明"请求来自我方注册的集群/端点"，L2 证明"端点背后是哪个被授权的人"；任何一层失败即拒绝。
2. **fail-closed 贯穿**：无头 401、无授权行=零工具、授权服务未接线=错误、缓存只对成功正响应生效；代码中找不到"身份缺失时默认全开"的分支。
3. **凭证最小暴露**：端点 token 只存哈希只显示一次；外部服务密钥 AES-GCM 落库、DTO 编译期不带密；Bearer 不走 query；跨端点比较常量时间。
4. **范围最小化**：端点白名单（工具+KB+agent+限流）∘ 用户授权交集 ∘ 下游既有 access/scope 三道独立计算，任一表配置失效都导致静默收窄而非放大。
5. **票据不可重放**：绑 tid+aud、5 分钟 TTL、type 用途隔离、强制 HMAC 算法。
6. **注入缓解**：外部工具描述加 `(external)` 前缀；stdio 传输禁用；外部返回图片数据消毒；工具名注册表 first-wins。
7. **可审计**：tools/call 全量 allow/deny 结构化日志带外部用户 id。
8. **无状态可扩展**：协议层 stateless，限流/票据 state/OAuth state 全在 Redis，多副本部署天然成立；唯一的进程内状态（权限缓存、last_used 节流）都是可丢失的性能优化。

---

## 10. 配置与运维速查

| 项 | 值/位置 |
|---|---|
| 服务面路径 | `POST/GET/DELETE /mcp/:endpoint_id`（全局鉴权之前挂载） |
| 管理面 | `/api/v1/mcp-endpoints`（Admin 写、manage_channels 能力） |
| 出站管理 | `/api/v1/mcp-services` 全套 |
| 必配密钥 | `JWT_SECRET`：登录刷新 + **用户票据验签（必须与网关一致，否则 MCP 恒 401）**；`SYSTEM_AES_KEY`：外部 MCP 密钥落盘加密 |
| 灰度开关 | `MCP_REQUIRE_USER_IDENTITY=true`（默认；false 仅过渡排障） |
| 权限生效延迟 | ≤45s（服务缓存 TTL）；撤销/删行因无负缓存，下次请求立即生效 |
| 票据 TTL | 5 分钟（网关逐请求签发） |
| 默认限流 | 60 次/分钟/端点（Redis 滑窗，可配 1–6000） |
| Python MCP | compose profile `full` 的 `mcp` 服务，8082 端口，需 `MCP_SERVER_AUTH_TOKEN` |
| 关键日志 | `[mcp-audit] tool call allowed/denied`、`[mcp-user-identity] ...`、`[mcpserver] endpoint ... references unavailable knowledge base` |

---

## 11. 后续改进建议（非本次范围）

1. 合并 0.8.3 对入站工具 annotations 与 grep/query 语义的后期修正，并回补被移除的官方测试；
2. 票据目前由网关共享 `JWT_SECRET` 对称签发，若未来要解密钥共享，可演进为网关专用密钥（JWK kid）或 weknora 提供一次性 token 兑换接口；
3. `mcp_user_permission` 无 weknora 侧管理/对账接口，建议补一个仅超管可用的只读排查 API（不暴露给普通用户）；
4. 用户权限缓存是每进程 45s，多副本下收权最坏延迟仍为 45s；若出现秒级收权要求，改 Redis 缓存或表变更通知；
5. OceanBase 迁移注意：本表与 mcp_endpoints 均使用 jsonb + `uuid_generate_v4()` 默认值，迁入 MySQL 方言时需改 JSON 列与应用层 UUID（与迁移总方案 A 线一致）。
