#!/usr/bin/env bash
# ============================================================
# WeKnora MCP 侧车（mcp-sidecar）x86_64 构建 / 验证 / 启动命令手册（全字面值版）
# 镜像: weknora-mcp-sidecar:0.1.0   基础镜像: python:3.12-slim (x86_64)
#
# 本文件与 readme_0.1.0_x86_64.sh 内容一致，区别是不使用任何 shell 变量，
# 每段都可直接逐行复制执行。
# 若你本地只有 stage0 标签的镜像，把本文件里的 weknora-mcp-sidecar:0.1.0
# 全部替换为 weknora-mcp-sidecar:stage0 即可。
# 设计依据见 指南/变更文档/MCP/需求.md
# ============================================================


### 1. 构建镜像（在仓库根目录执行，上下文是 ./mcp-sidecar）
cd /root/home/workspace_cribc/fqx_weknora/WeKnora
docker build -t weknora-mcp-sidecar:0.1.0 ./mcp-sidecar


### 2. 验证镜像内容完整性（--rm 容器跑完即删，不起服务）
docker run --rm --entrypoint sh weknora-mcp-sidecar:0.1.0 -c '
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
docker run -d --name sidecar-smoke -p 127.0.0.1:8084:8000 weknora-mcp-sidecar:0.1.0
sleep 3

# 3.1 健康检查（应返回 {"status":"ok","service":"weknora-mcp-sidecar","version":"0.1.0"}）
curl -s http://127.0.0.1:8084/healthz; echo

# 3.2 /mcp 无凭证必须被拒（所有 JSON-RPC 方法统一鉴权；不连 DB 也先判 401）
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8084/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'mcp-protocol-version: 2025-06-18' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}'
# 期望: 401（无 Bearer）

# 3.3 无凭证的 tools/list 同样被拒（阶段 2 工具共 9 个：8 个只读 + ask，须联机带票验证）
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8084/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'mcp-protocol-version: 2025-06-18' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'

# 3.4 容器内进程身份（应为 uid=10001(sidecar)）
docker exec sidecar-smoke id

# 冒烟结束，删除临时容器
docker rm -f sidecar-smoke


### 4. （可选）离线导出 / 目标机加载
# 导出:
docker save weknora-mcp-sidecar:0.1.0 | gzip > weknora-mcp-sidecar-0.1.0-x86_64.tar.gz
# 目标机加载:
# gunzip -c weknora-mcp-sidecar-0.1.0-x86_64.tar.gz | docker load


### 5. 正式启动前准备（只需一次）
# 5.1 创建 PG 只读账号（由 DBA/用户本人执行，脚本不会自动跑；需传入强密码）
#   psql "postgresql://postgres@<pg_host>:5432/WeKnora" \
#     -v sidecar_password='强密码' \
#     -f /root/home/workspace_cribc/fqx_weknora/WeKnora/mcp-sidecar/deploy/01_readonly_role.sql

# 5.2 公共书架（不需要任何名单文件：模式由请求凭证现场自识别）
#   - 建一个公共测试知识库，把它的 id 写入端点的 knowledge_base_ids，
#     机器与任何持票用户都能检索它；留空则机器主体零库（用户仍可取个人授权并集）。
#   - 千机用户个人可见库来自 mcp_user_permission 表，与公共书架取并集、自动去重。


### 6. 正式启动（常驻；加入 WeKnora compose 网络，用服务名 app/postgres/redis 互访）
# 网络名先执行确认：docker network ls | grep WeKnora
# 本机常见值 weknora-main_WeKnora-network，如不同请替换下方 --network 参数。
docker rm -f WeKnora-mcp-sidecar 2>/dev/null

docker run -d \
    --name WeKnora-mcp-sidecar \
    --restart unless-stopped \
    --network weknora-main_WeKnora-network \
    -p 8084:8000 \
    -e WEKNORA_BASE_URL=http://app:8080/api/v1 \
    -e WEKNORA_API_KEY='请填写：APIKey需含retrieve；端点开放ask时再含chat，永不授full access' \
    -e WEKNORA_VERIFY_SSL=true \
    -e SIDECAR_DB_DSN='postgresql://mcp_sidecar_ro:请填写密码@postgres:5432/WeKnora' \
    -e REDIS_ADDR=redis:6379 \
    -e REDIS_PASSWORD='请填写：同WeKnora现有Redis密码，无则留空' \
    -e REDIS_DB='请填写：未占用的db号' \
    -e JWT_SECRET='请填写：与千机网关一致的HS256密钥' \
    -e SIDECAR_SESSION_SECRET='请填写：新生成的随机长密钥；端点开放ask时必填' \
    -e ASK_TIMEOUT=240 \
    weknora-mcp-sidecar:0.1.0

# 注: 第 3 节冒烟不连 DB/Redis，仅验证启动时这些 -e 可暂不填；
#     正式启动鉴权/限流必须全部填齐；SIDECAR_SESSION_SECRET 缺失时 ask fail-closed
#     （只读工具不受影响）。


### 7. 启动后验证
# 7.1 容器状态与日志
docker ps --filter name=WeKnora-mcp-sidecar
docker logs --tail 50 WeKnora-mcp-sidecar

# 7.2 健康检查（在宿主本机）
curl -s http://127.0.0.1:8084/healthz; echo

# 7.3 从侧车容器内验证到后端/数据库网络可达
docker exec WeKnora-mcp-sidecar python -c "import socket; socket.gethostbyname('app'); print('OK app 可达')"
docker exec WeKnora-mcp-sidecar python -c "import socket; socket.gethostbyname('postgres'); print('OK postgres 可达')"
docker exec WeKnora-mcp-sidecar python -c "import socket; socket.gethostbyname('redis'); print('OK redis 可达')"

# 7.4 灰度验证（同一个端点、同一把钥匙；详见需求.md §4.3）
#   a) 裸 Bearer mcp_xxx -> 机器模式通过，只看得到端点 knowledge_base_ids（公共书架）
#   b) Bearer mcp_xxx.<坏票/过期票/签给别的端点的票> -> 一律 401，不会降级成机器模式
#   c) 网关发布合法复合票 mcp_xxx.<JWT> -> 用户模式通过，可见库=公共书架∪mcp_user_permission 个人授权


### 8. 更新 / 停止 / 回滚
# 更新镜像: 重新执行第 1 节 build，然后:
#   docker rm -f WeKnora-mcp-sidecar && 重新执行第 6 节 docker run
# 停止:
#   docker stop WeKnora-mcp-sidecar
# 回滚（指客户端回退到内置 Go MCP）: 客户端把 MCP 地址改回原 /mcp/<endpoint_id> 即可，
#   侧车无任何写库动作，停掉侧车容器不影响 WeKnora 本体。
