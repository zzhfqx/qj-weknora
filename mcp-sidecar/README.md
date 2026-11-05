# WeKnora MCP 侧车（mcp-sidecar）

独立的 Python MCP 服务：对外是唯一的 MCP 入口，对内只用 WeKnora **公开 REST**
办事，不 import、不修改 Go 主程序任何代码。目标是让 MCP 客户端用**一个网址、
一个密钥**就能接入，同一个端点同时兼容两类客户端（现场按凭证形态自识别）：

- **机器客户端**（Claude Desktop、脚本）：`Authorization: Bearer mcp_xxx`，
  不带用户票 → 机器模式，只能看到端点配置的"公共书架"；
- **千机用户客户端**（千机网关 → OpenCode）：`Authorization: Bearer mcp_xxx.<JWT>`，
  票验签通过 → 用户模式，可见库 = 公共书架 ∪ 该用户的个人授权。

> 带了票但票非法（签名错/过期/签给别的端点或工作空间）一律 **401**，
> 绝不会悄悄降级成机器模式。

完整设计与决策记录见 [../../指南/变更文档/MCP/需求.md](../../指南/变更文档/MCP/需求.md)。

## 当前进度

- [x] Python 工程骨架（pyproject / requirements / Dockerfile，非 root 运行）
- [x] Streamable HTTP（无状态）应用挂载在 `/mcp`，健康检查 `/healthz`
- [x] PG 只读账号脚本（`deploy/01_readonly_role.sql`，人工执行）
- [x] compose 片段（`deploy/compose.fragment.yml`，暂不并入主 compose）
- [x] 阶段 1-A：鉴权链——`auth/credentials.py`（复合 Bearer 切分）、
      `auth/middleware.py`（JWT 验签/凭证自识别/contextvars/ASGI 中间件）、
      `stores/endpoints.py`（连接池+端点反查）、`stores/permissions.py`（45s/无负缓存）；
      30 个单测全过，真实进程 401/403/503 冒烟通过
- [x] 阶段 1-B：`ratelimit.py`（Redis ZSET Lua 滑窗 + 内存兜底，对齐 Go）、
      `guards.py`（工具白名单/公共书架并集裁决/限流 + JSON 审计到 stdout）；48 个单测全过
- [x] 阶段 1-C：`backend/`（client/kb/documents/chunks REST 适配）+
      `tools/retrieve.py` 只读工具 1–5：库列表（含共享库详情补查）、多库并发
      hybrid-search 合并去重、knowledge_ids 文档级检索（≤100 + 越权/跨租户复
      检）、grep 关键词候选+正则后置过滤、文档分页/query 阅读、FAQ 行契约；
      89 个单测全过，tools/list 与 ASGI lifespan 冒烟通过
- [~] 阶段 1-D：respx REST 契约测试已完成（26 例：信封/错误映射/能力位回落/
      共享库补查/hybrid-search 请求体/chunks 重复参数/端到端越权，共 116 例全过）；
      镜像重建通过（非 root 身份、依赖齐全），离线容器冒烟通过（healthz 200、
      无凭证 /mcp 401、lifespan 干净）；联机冒烟（3b 节，tools/list 出 5 工具）
      待只读账号/retrieve 钥匙/端点令牌齐备后执行
- [x] 阶段 2：`backend/wiki.py` + `tools/wiki.py` wiki 三工具（search/read_page/
      index，裸响应适配、24000 rune 输出预算、正则三态、页面/索引渲染逐字对齐
      Go MCP）；`session_token.py`（HMAC 不透明会话票据，绑定 sid+端点+用户）、
      `backend/chat.py` + `tools/chat.py` 的 ask（智能体白名单/租户裁决、
      knowledge-chat 与 agent-chat 双路由、SSE 答案/引用聚合、240s 整体超时、
      请求体永不带 web_search_enabled）；新增 51 个单测（含 respx 真 HTTP/SSE
      契约）
- [x] 阶段 3：错误文案/annotations 打磨 + 9 工具冻结契约回归——对照 Go 源码逐字
      对齐模式校验回显、速率限制、空选择器/空白文档 ID、wiki 索引失败、ask 超时等
      文案；ask annotations 修正为 readOnly=false + SDK 默认（destructive=true/
      openWorld=true）；检索类工具补齐 Go 同款 2 空格缩进 JSON text + structured_content
      双输出；新增 annotations 冻结测试（3）、JSON-RPC e2e 四链路测试（5：机器公共
      书架/用户并集/坏票 401 不降级/范围外选择器不发详情探测）、wiki_index 契约
      测试（3）、冻结校验文案回归（3）
- [ ] 阶段 4：部署与网关灰度切换

> **2026-11-03 模型修订（已实施）**：① 取消 `identity.yaml` 端点模式名单，
> 改为单端点凭证自识别（机器/用户同一把钥匙，模式由"有没有合法 JWT"决定）；
> ② 库范围从"端点白名单 ∩ 用户授权"改为"公共书架 ∪ 个人授权"（frozenset
> 并集天然去重）；③ 机器端点公共书架留空 = 零库（取消旧"空=工作空间全部库"
> 语义，与待下线的内置 Go MCP 刻意分叉）；④ ask 的库范围一律钉死在 guard
> 闭集内，智能体自带 kb 配置不再生效。当前共 **227** 个单测全过。

> `/mcp` 已有完整鉴权链与 9 个工具，但 `SIDECAR_DB_DSN` 缺失等 fail-closed
> 场景下仍会拒服务；端点令牌就绪前请勿把端口暴露到内网之外。
> 开放 `ask` 的部署务必配齐 `SIDECAR_SESSION_SECRET`，否则该工具拒绝服务。

## 本地开发

```bash
cd mcp-sidecar
uv venv && uv pip install -r requirements.txt
# 或安装含开发依赖的完整工程：uv sync

# 跑测试
python -m pytest -q

# 本地启动（无需任何身份文件；裸令牌=机器模式，复合票=用户模式）
uvicorn app.main:app --host 127.0.0.1 --port 8084
curl http://127.0.0.1:8084/healthz
```

## 配置（环境变量）

| 变量 | 说明 |
|---|---|
| `SIDECAR_HOST` / `SIDECAR_PORT` | 监听地址，默认 0.0.0.0:8000 |
| `WEKNORA_BASE_URL` | WeKnora REST 基址，默认 `http://app:8080/api/v1` |
| `WEKNORA_API_KEY` | 调 REST 的工作空间 API Key：只读工具只需 **retrieve** 能力；端点要开放 `ask` 时钥匙还需 **chat** 能力（用于建会话与 QA 流）。**永不授予 full access**——侧车不会、也无法替客户端打开联网（ask 请求体从不携带 web_search_enabled） |
| `WEKNORA_VERIFY_SSL` | 是否校验后端 TLS，默认 true |
| `SIDECAR_DB_DSN` | PG 只读账号 DSN（见 `deploy/01_readonly_role.sql`） |
| `REDIS_ADDR` / `REDIS_PASSWORD` / `REDIS_DB` | 限流用 Redis（复用现有实例） |
| `JWT_SECRET` | 与千机网关一致的 HS256 票据密钥；**不配置时所有带用户票的请求 401**（机器模式不受影响） |
| `SIDECAR_SESSION_SECRET` | ask 多轮会话票据 HMAC 签名密钥；**不配置则 ask 整体 fail-closed**，只读工具不受影响 |
| `ASK_TIMEOUT` | ask 整体超时秒数，默认 240 |

## 凭证自识别与知识库范围（公共书架并集模型）

- **端点配置只认两个清单**（`mcp_endpoints` 行）：
  - `knowledge_base_ids`：公共书架。任何持有该端点钥匙的机器或用户都能检索的库；
    留空表示"不开放任何公共库"，此时机器主体零库——
    如需一个开箱可测的公共库，请建库后把它的 id 填进这里；
  - `tools`：该端点允许调用的工具白名单。
- **机器模式**（裸 `mcp_xxx`）：生效范围 = 公共书架。
- **用户模式**（`mcp_xxx.<JWT>`）：生效范围 = 公共书架 ∪ 该用户在
  `mcp_user_permission` 表中的个人授权（并集自动去重）；个人授权为空仍可看
  公共书架；授权表读取失败 fail-closed，不降级。
- 机器范围恒为用户范围的子集：网关漏发/错发票据只会让看到的库变少，不会越权。
- 票也可走过渡期旧头 `X-User-Credential`，校验规则与复合票完全相同。

## 目录

```
app/          侧车代码
tests/        pytest 单测
deploy/       只读账号 SQL、compose 片段、构建/部署命令手册（人工审阅/执行，不自动生效）
```
