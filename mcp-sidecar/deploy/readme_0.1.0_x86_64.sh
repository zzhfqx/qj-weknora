#!/usr/bin/env bash
# ============================================================
# WeKnora MCP 侧车（mcp-sidecar）x86_64 构建 / 验证 / 启动命令手册
# 镜像: weknora-mcp-sidecar:0.1.0   基础镜像: python:3.12-slim (x86_64)
# 流程: docker build -> 镜像校验 -> 临时容器冒烟 -> (可选)导出离线包 -> 正式 docker run
#
# 说明: 本文件是"命令记录"，每节可单独复制执行；带占位符的密钥/密码需替换。
#       设计依据见 指南/变更文档/MCP/需求.md
# ============================================================


### 0. 变量（按实际环境修改）
# ⚠️ 逐段复制执行时：必须先在"同一个终端"里跑完本节变量，后面的 $IMAGE/$HOST_PORT
#    才会生效；否则变量为空，例如 curl http://127.0.0.1:$HOST_PORT 会变成访问 80 端口，
#    打到本机别的网页服务。不想先跑本节，就把命令里的变量直接替换成字面值（如 8084）。
REPO_DIR=/root/home/workspace_cribc/fqx_weknora/WeKnora
# 侧车部署目录（01_readonly_role.sql 等部署文件放这里；2026-11-03 起无需挂载任何名单文件）
SIDECAR_HOME=/root/home/workspace_cribc/fqx_weknora/WeKnora/mcp-sidecar
IMAGE=weknora-mcp-sidecar:0.1.0
NAME=WeKnora-mcp-sidecar
# WeKnora compose 网络名：执行 `docker network ls | grep WeKnora` 按实际栈选
#   常见值: weknora-main_WeKnora-network / weknora_WeKnora-network
NETWORK=weknora-main_WeKnora-network
# 宿主端口（对外/对本机访问的端口；容器内固定 8000）
HOST_PORT=8084
echo "变量已加载：IMAGE=$IMAGE HOST_PORT=$HOST_PORT NETWORK=$NETWORK"


### 1. 构建镜像（在仓库根目录执行，上下文是 ./mcp-sidecar）
cd $REPO_DIR
docker build -t $IMAGE ./mcp-sidecar


### 2. 验证镜像内容完整性（--rm 容器跑完即删，不起服务）
docker run --rm --entrypoint sh $IMAGE -c '
    printf "=== 1. 架构确认（应为 x86_64）===\n"
    uname -m

    printf "\n=== 2. Python 版本 ===\n"
    python --version

    printf "\n=== 3. 关键 Python 依赖 ===\n"
    python -c "import mcp, httpx, asyncpg, redis, jwt, uvicorn; from importlib.metadata import version; print(\"OK deps, mcp\", version(\"mcp\"))"

    printf "\n=== 4. 侧车代码已拷入（无 identity.yaml，模式按凭证现场自识别）===\n"
    ls /app/app/main.py >/dev/null 2>&1 && echo "OK app files" || echo "MISS app files"
    test ! -e /app/config/identity.yaml && echo "OK no identity file" || echo "WARN stale identity file"

    printf "\n=== 5. 运行身份（应为 uid=10001 sidecar，非 root）===\n"
    id
'


### 3. 临时容器冒烟（只绑本机回环，验完即删；不连数据库、不加入业务网络）
docker rm -f sidecar-smoke 2>/dev/null
docker run -d --name sidecar-smoke -p 127.0.0.1:$HOST_PORT:8000 $IMAGE
sleep 3

# 3.1 健康检查（无需鉴权，不连数据库）
curl -s http://127.0.0.1:$HOST_PORT/healthz; echo
# 期望: {"status":"ok","service":"weknora-mcp-sidecar","version":"0.1.0"}

# 3.2 /mcp 无凭证必须被拒（阶段 1-A 起所有 JSON-RPC 方法统一鉴权）
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:$HOST_PORT/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'mcp-protocol-version: 2025-06-18' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
# 期望: 401（无 Bearer）；未配 SIDECAR_DB_DSN 时为 503（fail-closed）

# 3.3 容器内进程身份（必须是非 root 的 uid=10001 sidecar）
docker exec sidecar-smoke id

# 冒烟结束，删除临时容器
docker rm -f sidecar-smoke


### 3b. 联机冒烟（阶段 2：验 9 个工具 = 8 只读 + ask；需接 PG/Redis/后端，容器加入业务网络）
# 前置：01_readonly_role.sql 已执行、侧车端点令牌 mcp_xxx 已落库
#       （端点工具白名单含待验工具，如 list_knowledge_bases；knowledge_base_ids
#       即"公共书架"，留空时机器主体一个库都看不到，建议先挂一个公共测试库）。
#       上游 WeKnora API Key：只验只读/wiki 工具时 retrieve 即可；要验 ask 必须
#       再含 chat 能力（永不授 full access）。
#       下面变量按实际替换；机器主体直接 "Bearer mcp_xxx"，用户主体 "Bearer mcp_xxx.<JWT>"。
SMOKE_TOKEN=mcp_xxxxxxxx
docker rm -f sidecar-smoke 2>/dev/null
docker run -d --name sidecar-smoke -p 127.0.0.1:$HOST_PORT:8000 \
  --network $NETWORK \
  -e SIDECAR_DB_DSN='postgresql://mcp_sidecar_ro:<密码>@postgres:5432/WeKnora' \
  -e REDIS_ADDR=redis:6379 -e REDIS_PASSWORD='<Redis密码>' \
  -e WEKNORA_BASE_URL=http://app:8080/api/v1 \
  -e WEKNORA_API_KEY='<retrieve或retrieve+chat钥匙>' \
  -e JWT_SECRET='<网关JWT密钥>' \
  -e SIDECAR_SESSION_SECRET='<随机长密钥；不配则ask拒绝服务>' \
  $IMAGE
sleep 3

# 3b.1 initialize（带 Bearer；stateless 模式每请求独立）
curl -s -X POST http://127.0.0.1:$HOST_PORT/mcp \
  -H "Authorization: Bearer $SMOKE_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'mcp-protocol-version: 2025-06-18' \
  -H 'mcp-session-id: smoke' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'; echo
# 期望 SSE data: 内 serverInfo.name = weknora-sidecar

# 3b.2 tools/list（阶段 2 应为 9 个：list_knowledge_bases/search_knowledge/grep_chunks/
#       list_documents/read_document/wiki_search/wiki_read_page/wiki_index/ask）
curl -s -X POST http://127.0.0.1:$HOST_PORT/mcp \
  -H "Authorization: Bearer $SMOKE_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'mcp-protocol-version: 2025-06-18' \
  -H 'mcp-session-id: smoke' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' | grep '^data:'

# 3b.3 tools/call 真打后端 REST（应返回 success 信封里的知识库数组）
curl -s -X POST http://127.0.0.1:$HOST_PORT/mcp \
  -H "Authorization: Bearer $SMOKE_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'mcp-protocol-version: 2025-06-18' \
  -H 'mcp-session-id: smoke' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_knowledge_bases","arguments":{}}}' | grep '^data:'

# 3b.4（可选）ask 冒烟：端点白名单需含 ask、上游钥匙需含 chat 能力。
#       返回文本末尾带 session_id: t1.xxx（侧车 HMAC 票据，不是上游真实 sid）；
#       把该票据填回下一轮 session_id 可续问。SSE 聚合可能耗时数秒到数分钟。
curl -s -X POST http://127.0.0.1:$HOST_PORT/mcp \
  -H "Authorization: Bearer $SMOKE_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'mcp-protocol-version: 2025-06-18' \
  -H 'mcp-session-id: smoke' \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ask","arguments":{"question":"测试问题，请简单回答"}}}' | grep '^data:'

docker rm -f sidecar-smoke


### 4. （可选）离线导出 / 目标机加载
# 导出:
docker save $IMAGE | gzip > weknora-mcp-sidecar-0.1.0-x86_64.tar.gz
# 目标机加载:
# gunzip -c weknora-mcp-sidecar-0.1.0-x86_64.tar.gz | docker load


### 5. 正式启动前准备（只需一次）
# 5.1 创建 PG 只读账号（由 DBA/用户本人执行，脚本不会自动跑；需传入强密码）
#   psql "postgresql://postgres@<pg_host>:5432/WeKnora" \
#     -v sidecar_password='强密码' \
#     -f $SIDECAR_HOME/deploy/01_readonly_role.sql

# 5.2 公共书架（不需要任何名单文件：模式由请求凭证现场自识别）
#   - 建一个公共测试知识库，把它的 id 写入该端点的 knowledge_base_ids，
#     机器与任何持票用户都能检索它；留空则机器主体零库（用户仍可取个人授权并集）。
#   - 千机用户个人可见库来自 mcp_user_permission 表，与公共书架取并集、自动去重。


### 6. 正式启动（常驻；加入 WeKnora compose 网络，用服务名 app/postgres/redis 互访）
docker rm -f $NAME 2>/dev/null

docker run -d \
    --name $NAME \
    --restart unless-stopped \
    --network $NETWORK \
    -p $HOST_PORT:8000 \
    -e WEKNORA_BASE_URL=http://app:8080/api/v1 \
    -e WEKNORA_API_KEY='请填写：APIKey需含retrieve；端点开放ask时再含chat，永不授full access' \
    -e WEKNORA_VERIFY_SSL=true \
    -e SIDECAR_DB_DSN='postgresql://mcp_sidecar_ro:请填写密码@postgres:5432/WeKnora' \
    -e REDIS_ADDR=redis:6379 \
    -e REDIS_PASSWORD='请填写：同WeKnora现有Redis密码，无则留空' \
    -e REDIS_DB='请填写：未占用的db号' \
    -e JWT_SECRET='请填写：与千机网关一致的HS256密钥' \
    -e SIDECAR_SESSION_SECRET='请填写：新生成的随机长密钥' \
    -e ASK_TIMEOUT=240 \
    $IMAGE

# 注: 阶段 0 骨架不连 DB/Redis，仅验证启动时这些 -e 可暂不填；
#     阶段 1 起鉴权/限流生效后必须全部填齐；
#     阶段 2 起 SIDECAR_SESSION_SECRET 必须填，否则 ask fail-closed（只读工具不受影响）。


### 7. 启动后验证
# 7.1 容器状态与日志
docker ps --filter name=$NAME
docker logs --tail 50 $NAME

# 7.2 健康检查（在宿主本机）
curl -s http://127.0.0.1:$HOST_PORT/healthz; echo

# 7.3 从侧车容器内验证到后端/数据库网络可达
docker exec $NAME python -c "import socket; socket.gethostbyname('app'); print('OK app 可达')"
docker exec $NAME python -c "import socket; socket.gethostbyname('postgres'); print('OK postgres 可达')"
docker exec $NAME python -c "import socket; socket.gethostbyname('redis'); print('OK redis 可达')"

# 7.4 灰度验证（同一个端点、同一把钥匙；详见需求.md §4.3）
#   a) 裸 Bearer mcp_xxx -> 机器模式通过，只看得到端点 knowledge_base_ids（公共书架）
#   b) Bearer mcp_xxx.<坏票/过期票/签给别的端点的票> -> 一律 401，不会降级成机器模式
#   c) 网关发布合法复合票 mcp_xxx.<JWT> -> 用户模式通过，可见库=公共书架∪mcp_user_permission 个人授权


### 8. 更新 / 停止 / 回滚
# 更新镜像: 重新执行第 1 节 build，然后:
#   docker rm -f $NAME && 重新执行第 6 节 docker run
# 停止:
#   docker stop $NAME
# 回滚（指客户端回退到内置 Go MCP）: 客户端把 MCP 地址改回原 /mcp/<endpoint_id> 即可，
#   侧车无任何写库动作，停掉侧车容器不影响 WeKnora 本体。
