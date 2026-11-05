# 内置 MCP 通用性适配方案：端点密钥与用户凭证合一

> 状态：**设计方案（尚未实施）**
> 适用基线：pg-fqx 分支（0.8.3 + fqx 用户校验二开）
> 范围：仅形态②入站内置 MCP 服务端（`internal/mcpserver/` + MCP 鉴权中间件），不涉及形态①出站 MCP 客户端与形态③独立 Python mcp-server。
> 关联文档：《内置MCP机制原理详解-0.8.3基线与二开用户校验.md》《内置MCP服务端12工具设计详解.md》。

---

## 1. 背景与问题

当前入站 MCP 面使用**两层独立凭证**：

| 层 | 凭证 | 头 | 验证内容 | 来源 |
|----|------|----|---------|------|
| L1 | 端点令牌 `mcp_xxx`（静态，服务端仅存 SHA-256 哈希） | `Authorization: Bearer` | 哪个端点、是否启用、工具白名单 | 上游 0.8.3 原有 |
| L2 | 用户身份票据（HS256 JWT，TTL 5 分钟，claims `type/sub/tid/aud/jti`） | `X-User-Credential`（自定义头） | 千机真实用户是谁、能看哪些库 | fqx 二开新增 |

二开这一层是专门为「千机网关 → OpenCode」这一条链路做的：网关持有 `JWT_SECRET`，转发时逐请求签发短期票据，服务端用 `MCP_REQUIRE_USER_IDENTITY` 环境变量全局强制无头即 401。

暴露出的通用性问题：

1. **自定义头不通用**：OpenCode、Claude Desktop、Cursor 以及大量第三方智能体客户端只暴露一个「API Key / Bearer Token」输入框，无法配置 `X-User-Credential`，二开能力只有千机网关这类可改请求头的转发方才用得上。
2. **全局开关过于粗暴**：`MCP_REQUIRE_USER_IDENTITY=false` 一关全关、一开全要，无法做到「这个端点给千机用必须带用户、那个端点给桌面客户端用不带用户」。
3. **两种客户端无法共存于同一端点**：需要区分用户的智能体与不需要区分用户的客户端必须分别建端点、分别发令牌。
4. **票据耦合 MCP**：`type="mcp_user_identity"`、`aud=端点ID` 的票据设计无法给将来其它需要用户身份的智能体入口复用。

目标：**合成一套凭证与一个统一中间件**，同时满足三类调用方——

- 千机网关/OpenCode：必须区分真实用户，按 `mcp_user_permission` 授权库过滤；
- 其它有签发能力、需要区分用户的智能体：同上；
- Claude Desktop 等普通客户端：不区分用户，端点共用，行为与现状机器模式完全一致。

## 2. 现状事实（实现依据）

- 端点令牌生成：`mcp_` 前缀 + `base64.RawURLEncoding` 随机串，字符集仅 `A-Za-z0-9-_`，**不含 `.`**；库存 `sha256hex`；鉴权时 `GetByTokenHash` + 与路径 endpoint_id 的 `subtle.ConstantTimeCompare`（[mcp_endpoint.go:61-73](../../../internal/application/service/mcp_endpoint.go#L61-L73)、[mcp_endpoint.go:269-284](../../../internal/application/service/mcp_endpoint.go#L269-L284)，前缀常量 [mcp_endpoint.go:58-60](../../../internal/types/mcp_endpoint.go#L58-L60)）。
- 用户票据：[mcp_user_ticket.go](../../../internal/application/service/mcp_user_ticket.go)，HS256 JWT（恰好含两个 `.`），字段 `type=mcp_user_identity / sub=外部用户ID / tid=工作空间ID / aud=端点ID / jti / iat / exp`；Parse 强制 HMAC 算法、验签、过期、type 隔离、tid/aud/sub 非空。
- L1 中间件 [mcp_endpoint_auth.go](../../../internal/middleware/mcp_endpoint_auth.go)：验过后合成机器用户 `mcp-<epid>` + `MCPEndpointPrincipal` + `APIKeyScope`，写入 ctx。
- L2 中间件 [mcp_user_identity.go](../../../internal/middleware/mcp_user_identity.go)：验票 + `tid==ep.TenantID && aud==ep.ID` 交叉校验，外部 user_id 双写 gin keys 与 request context；无头时由环境变量决定 401 还是告警放行。
- 路由挂载顺序为 `auth, identity, h`（[routes_mcp_endpoint.go:44-49](../../../internal/router/routes_mcp_endpoint.go#L44-L49)）。
- 下游对用户身份的全部消费点只有两处语义：`filterTools`/`guardTool` 的 fail-closed 闸门与 `allowedKnowledgeBases` 中「端点允许集 ∩ 用户授权集」（[server.go:162-224](../../../internal/mcpserver/server.go#L162-L224)、[scope.go:24-97](../../../internal/mcpserver/scope.go#L24-L97)）。**只要 ctx 里有无 user_id 这一事实不变，下游零改动。**

## 3. 方案总览

一句话：**一个标准 Bearer 头、两段拼接（无点的端点密钥 + 带点的用户 JWT）、端点上用身份模式裁决接受哪种身份。**

```
机器模式（老客户端零改动）：
  Authorization: Bearer mcp_xxxxxxxxxxxxxxxx

用户模式（网关/OpenCode 等）：
  Authorization: Bearer mcp_xxxxxxxx.eyJhbGciOiJIUzI1NiJ9.<payload>.<sig>
                                   └──── L1 ────┘ └────────── L2 JWT ──────────┘
```

- 分隔符取**第一个 `.`**：左段端点令牌字符集保证无点；右段 JWT 自身两个点原样保留，切分无歧义。
- 双因子强度不降级：用户模式仍要求调用方同时持有①端点静态密钥②`JWT_SECRET` 签发能力，两个独立秘密缺一不可。不能退化成「只有 JWT 就能调」的单因子。
- 全部走标准 `Authorization` 头，任何只支持填单个 API Key 的 MCP 客户端都能用。

## 4. 详细设计

### 4.1 端点身份模式 `identity_mode`

`mcp_endpoints` 新增一列（DDL 见 §7）：

| 取值 | 机器凭证 | 用户凭证 | 适用 |
|------|---------|---------|------|
| `machine`（新端点默认） | ✅ | ❌ 携带即拒绝 | 桌面客户端、脚本等不区分用户的场景 |
| `user_required` | ❌ | ✅ | 千机/OpenCode 专用端点，替代全局环境变量 |
| `user_optional` | ✅（按机器主体，仅端点白名单） | ✅（端点白名单 ∩ 用户授权表） | 同一端点两类客户端混用 |

`user_optional` 必须钉死的语义：**机器调用的权限 = 端点机器主体自身权限（端点 KB 白名单），绝不是「该端点所有用户授权的并集」**；只有携带用户身份时才做 `mcp_user_permission` 交集。否则会出现「不带用户反而看得更多」的提权路径。

服务端裁决，客户端无法自选身份级别：模式不匹配一律 401/403，不靠客户端自觉。

### 4.2 凭证切分与判定

新增无外部依赖的小工具（建议放 `internal/application/service/` 或 middleware 内）：

```go
// SplitBearerCredential 在第一个 "." 处切分复合凭证。
// 端点令牌字符集为 base64url（无点），JWT 恒含两个点，故切分无歧义。
//   - 无点：machineToken=raw, userTicket=""
//   - 有点：machineToken=点前段, userTicket=点后全部（含 JWT 自身的点）
func SplitBearerCredential(raw string) (machineToken, userTicket string)
```

判定规则：右段为空 → 机器凭证；右段非空 → 用户凭证（右段必须能通过 JWT 解析，不引入第三种格式；PAT 扩展见 §8）。

左段恒为端点令牌，**两种模式都必须通过 L1 的哈希鉴权**，机器主体（`mcp-<epid>` + Principal + APIKeyScope）在两种模式下都照常合成——用户身份是「附加在机器主体之上」的一层，不替换主体。这样审计、限流、会话影子主体（ask 工具的 `MCPEndpointPrincipal.StorageID()`）等行为保持一致。

### 4.3 统一中间件（合并 L1+L2）

用一个 `UnifiedMCPEndpointAuth(endpointSvc, tenantSvc, identityModeSvc)` 取代现挂的两个中间件；也可保留两个函数、在路由层改成单个入口，内部按顺序执行，减少 diff。流程：

```
1. 取路径 :endpoint_id 与 Bearer；任一缺失 → 401（WWW-Authenticate: Bearer realm="weknora-mcp"）
2. SplitBearerCredential：
   ├─ 机器凭证（无用户票据）
   │     L1 Authenticate(endpointID, machineToken) 哈希常量时间比对
   │       ├ 失败/禁用/不存在 → 401/403（沿用现有错误分支与文案）
   │     identity_mode 必须 ∈ {machine, user_optional}
   │       └ user_required 却收到机器凭证 → 401 "endpoint requires user identity"
   │     合成机器主体 ctx（同现状）
   └─ 用户凭证（有用户票据）
         L1 Authenticate(...)  ← 端点密钥这一因子照常校验，失败 → 401
         ParseMCPUserTicket(userTicket)：HMAC/签名/type/exp
         交叉校验 tid==ep.TenantID 且 aud==ep.ID
         identity_mode 必须 ∈ {user_required, user_optional}
           └ machine 模式却收到用户凭证 → 401 "endpoint does not accept user identity"
         任一环节失败 → 401，错误文案统一为 "invalid credential"，
           不区分是端点密钥错还是票据错（防分段探测）
         合成机器主体 ctx + 写入外部 user_id（沿用 MCPUserIDContextKey，双写 gin keys）
3. 租户状态校验（inactive/disabled → 403，同现状）
4. c.Next()
```

要点：

- 常量时间、查询令牌哈希等防探测措施只作用于 L1 段，保持现状；用户票据段错误不回吐具体原因。
- 中间件只负责「身份有没有、是什么模式」；**是否有库权限仍由 `filterTools/guardTool/allowedKnowledgeBases` 在业务层裁决**（用户存在但授权列表为空 → 看到零工具，与现状一致）。
- 灰度期兼容旧头：请求没有复合段时，再回退读一次 `X-User-Credential`（受同一 identity_mode 约束）。过渡期结束删除（见 §6）。

### 4.4 下游零改动清单

ctx 中「机器主体 + 可选 user_id」的结构不变，因此以下模块无需修改：

- [mcpserver/server.go](../../../internal/mcpserver/server.go)：`filterTools`、`guardTool`（白名单→用户授权非空→限流→审计→执行→URL 改写）；
- [mcpserver/scope.go](../../../internal/mcpserver/scope.go)：`userKnowledgeBaseIDs`、`allowedKnowledgeBases` 三层交集；
- 12 个工具 handler、审计字段（user_id 缺失时为空，机器调用照旧）；
- 会话归属（ask 仍挂端点影子主体，与外部 user_id 无关）。

改动面收敛在：凭证解析（新工具）、鉴权中间件（合并）、端点模型/迁移（新列）、端点管理 API 与前端（模式选择）、网关契约文档。

### 4.5 票据泛化（为其它智能体入口复用）

两步走，避免一次动太多：

1. 本期：票据 `type` 与 `aud` 语义文档化为「身份票据 / 资源标识」，MCP 面 aud 仍是端点 id；签发函数抽出通用签名：

   ```go
   func IssueUserIdentityTicket(userID string, tenantID uint64, resourceID string, ttl time.Duration) (string, error)
   // IssueMCPUserTicket 保留为薄封装：resourceID=endpointID, type 固定 mcp_user_identity
   ```

   `type` 本周期维持 `mcp_user_identity` 不变（已在网关联调，避免 Parse 端 type 隔离造成中断）。
2. 后续若新增非 MCP 的智能体入口：引入 `type=user_identity` + `aud` 前缀体系（如 `mcp:<epid>` / `agent:<id>`），各入口中间件只认自己的 aud 前缀，同一签发/验签设施共享。

### 4.6 千机网关改造

网关现状已同时持有端点令牌与 `JWT_SECRET`，改造仅一行拼接：

```
Authorization: Bearer mcp_xxx + "." + IssueMCPUserTicket(uid, tid, endpointID, 5min)
```

- 不再需要注入自定义头；网关侧 OpenCode/任意 MCP 客户端只需支持 Bearer；
- 票据 5 分钟 TTL、按请求即时签发的策略不变（鉴权发生在 HTTP 建连时刻，短 TTL 不影响 SSE/流式长连）；
- 网关对 WeKnora 的对接说明中，`X-User-Credential` 标为 deprecated。

## 5. 端到端时序

**用户模式（千机 → OpenCode → WeKnora）**

```
千机用户请求
  │ 网关：IssueMCPUserTicket(uid,tid,epid) → JWT
  ▼
OpenCode（Authorization: Bearer mcp_xxx.<JWT>）
  │
  ▼ /mcp/:endpoint_id （tools/list 或 tools/call）
UnifiedMCPEndpointAuth
  ├─ L1 端点令牌哈希校验通过 → 机器主体
  ├─ JWT 验签/exp/type 通过；tid/aud 交叉校验通过
  ├─ identity_mode=user_required/user_optional 校验通过
  └─ ctx 写入 user_id
  ▼
filterTools/guardTool：端点白名单 ∩ user_id 非空（否则零工具/拒绝）
  ▼
工具 handler → allowedKnowledgeBases：端点集 ∩ mcp_user_permission ∩ ResolveKB
```

**机器模式（Claude Desktop）**：同一入口，Bearer 只有 `mcp_xxx` → 机器主体，无 user_id → 不查用户授权表，行为等同 0.8.3 原生。

## 6. 迁移与发布计划

1. **DB**：`mcp_endpoints` 加 `identity_mode VARCHAR(20) NOT NULL DEFAULT 'machine'`（PG 版本化迁移 + sqlite 迁移，编号接续当前 000216/000016 之后；含 down）。
2. **数据回填**：普通端点保持 `machine`；当前实际给千机使用的端点（上线时靠 `MCP_REQUIRE_USER_IDENTITY=true` 强制）显式置 `user_required`。建议提供一次性配置清单人工确认，不做自动猜测。
3. **代码**：实现切分工具 + 统一中间件；过渡期双读（复合 Bearer 优先，旧 `X-User-Credential` 兜底，两者都受 identity_mode 约束）。
4. **管理面**：端点创建/编辑接口与前端增加身份模式选择，文案说明三种模式；端点详情展示当前模式。
5. **网关切换**：千机改发复合 Bearer；灰度期两种凭证并行验证。
6. **清理（下一个版本）**：删除 `MCP_REQUIRE_USER_IDENTITY` 环境变量、`X-User-Credential` 解析与灰度分支；路由上移除旧 `MCPUserIdentity()` 挂载点。
7. 回滚：每步独立可回退；数据库列保留至清理版本之后再评估删除。

## 7. 数据结构与错误码

### 7.1 DDL（示意）

```sql
-- up（versioned PostgreSQL）
ALTER TABLE mcp_endpoints
  ADD COLUMN identity_mode VARCHAR(20) NOT NULL DEFAULT 'machine';
-- 已知千机端点由发布清单回填为 'user_required'

-- down
ALTER TABLE mcp_endpoints DROP COLUMN identity_mode;
```

Go 模型：`IdentityMode string`，常量 `MCPEndpointIdentityMachine / UserRequired / UserOptional`，默认值在 `BeforeCreate` 兜底；非法值入库拒绝。

### 7.2 对外错误码（HTTP）

| 场景 | 状态码 | error 文案要点 |
|------|--------|---------------|
| 无头/无 Bearer | 401 | missing bearer token（带 WWW-Authenticate） |
| 端点令牌非法/端点不存在 | 401 | invalid token（不区分两者） |
| 端点禁用 | 403 | mcp endpoint disabled |
| 工作空间停用 | 403 | workspace inactive |
| user_required 收到机器凭证 | 401 | endpoint requires user identity |
| machine 收到用户凭证 | 401 | endpoint does not accept user identity |
| 票据验签/过期/交叉校验/任一段失败 | 401 | invalid credential（不指明失败段） |
| 用户有效但无任何授权库 | 200 | tools/list 返回 `[]`；tools/call 工具拒绝（现状语义不变） |

### 7.3 兼容性矩阵

| 客户端发送方式 | machine | user_optional | user_required |
|----------------|---------|---------------|---------------|
| 仅 `mcp_xxx` | ✅ 机器主体 | ✅ 机器主体 | ❌ 401 |
| `mcp_xxx.<JWT>` 合法 | ❌ 401 | ✅ 用户主体 | ✅ 用户主体 |
| 旧 `X-User-Credential`（过渡期） | ❌ 401 | ✅ 用户主体 | ✅ 用户主体 |
| 票据非法/过期/tid/aud 不符 | ❌ 401 | ❌ 401 | ❌ 401 |

## 8. 可选二期：用户个人令牌（PAT）

面向「没有网关代签 JWT、但又需要区分用户」的智能体：用户在 WeKnora 控制台自助生成 PAT，服务端只存哈希，支持随时吊销。

```
Authorization: Bearer mcp_xxx.pat_yyyyyyyy
```

- 切分规则扩展：右段以 `pat_` 开头（JWT 恒为 `eyJ` 开头 base64url 段）→ 查 PAT 表反查 `(tenant_id, user_id)`，等价于用户身份，其余裁决流程不变；
- 新增表 `mcp_user_pats(id, tenant_id, user_id, token_hash, name, last_used_at, expires_at, revoked, created_at)`；
- PAT 与端点令牌仍是双因子（左段端点密钥 + 右段个人密钥），泄露任一个都不能独立调用；
- 代价：有状态存储、过期/吊销管理、控制台页面。一期不做，仅在切分工具与文档中预留判定位。

## 9. 安全分析

1. **双因子保持**：用户模式同时校验静态端点密钥（哈希入库）与 JWT（共享密钥签名），相比现状不减弱；相比「纯 JWT 单因子」方案，JWT_SECRET 单点泄露不足以调用（还需对应端点的有效令牌）。
2. **防重放**：JWT 的 `tid/aud` 与路径端点强绑定交叉校验沿用，A 工作空间/端点的票据不能在 B 处使用；短 TTL + `jti`（预留审计/将来拒重放）。
3. **防探测**：L1 段保持常量时间比对；复合凭证错误统一文案，不回显是哪一段失败；机器/用户模式不匹配也返回 401 而非 400，不暴露端点模式细节给匿名调用方（模式本身不是秘密，但不主动帮助枚举）。
4. **防提权**：`user_optional` 下机器调用不获得任何用户授权并集；用户调用不得超过端点白名单（交集两个方向都存在）。
5. **日志脱敏**：凭证集中在一个头，沿用现有 Authorization 脱敏即可；反而消除了 `X-User-Credential` 可能被自定义日志/代理完整记录的隐患。注意 jti/user_id 可入审计，票据原文绝不入日志。
6. **fail-closed 不变**：身份缺失（在要求用户的模式下）、授权服务不可用、授权为空三类仍全部拒绝，与二开现状一致。

## 10. 被否决的备选方案

| 方案 | 否决理由 |
|------|---------|
| 纯 JWT 单因子（废除 `mcp_xxx`，端点标识放进 claims） | 丢失双因子；JWT_SECRET 泄露即全盘失守；老客户端全部要改 |
| 保留两个头，仅放宽客户端支持 | 通用客户端无法配自定义头，核心问题没解决 |
| 用 query 参数传用户票据 | 会进访问日志/代理日志/浏览器历史，现状 L1 明确拒绝 query token，不能开倒车 |
| 每用户发独立端点令牌（把用户维度编码进令牌行） | 令牌数量随用户×端点膨胀，千机用户在 WeKnora 无本地账号、无法自助管理；轮换/吊销成本高；仍需外部授权表同步 |
| 端点级全局开关维持现状（不改） | 无法同端点混用、无法按端点差异化，通用性问题原样保留 |

## 11. 改动清单（实施时对照）

- 新增：凭证切分工具（`SplitBearerCredential`）及单测；
- 改造：`internal/middleware/` 统一 MCP 鉴权中间件（合并 L1/L2 逻辑，保留交叉校验与错误分支）；
- 改造：[routes_mcp_endpoint.go](../../../internal/router/routes_mcp_endpoint.go) 挂载点；
- 新增：`mcp_endpoints.identity_mode` 模型字段、常量、两套迁移（versioned PG + sqlite）与 down；
- 改造：端点 service/handler 的创建更新校验、管理前端；
- 泛化：`IssueUserIdentityTicket` 通用签发函数，`IssueMCPUserTicket` 降级为封装；
- 文档：千机网关对接文档更新（复合 Bearer）、本方案落地后在变更记录中按「二开」章节登记实际完成项；
- 测试：机器/用户/可选三种模式 × 合法/非法/缺段/跨端点票据/过期 的矩阵测试，迁移回填测试，旧头灰度测试。
