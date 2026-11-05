"""侧车入口：组装 MCP 应用并以 uvicorn 承载。

阶段 1：鉴权链已接通——复合 Bearer 切分、端点令牌反查、用户票验签在
ASGI 中间件完成；凭证形态自识别（无票=机器，带合法票=用户，坏票 401），
无有效凭证时 /mcp 不可用。
阶段 1-C：只读工具 1-5（检索类）已注册，经 ToolGuard 裁决后调后端 REST。
"""

from __future__ import annotations

import asyncio
import contextlib
import logging

import redis.asyncio as redis_asyncio
import uvicorn
from mcp.server import MCPServer
from starlette.requests import Request
from starlette.responses import JSONResponse, Response

from app import __version__
from app.auth.middleware import MCPAuthMiddleware
from app.backend.client import RESTClient
from app.config import Settings
from app.guards import ToolGuard
from app.ratelimit import SlidingWindowLimiter
from app.stores.endpoints import Database, EndpointStore
from app.stores.permissions import PermissionStore
from app.tools.chat import AskToolSet
from app.tools.retrieve import RetrieveToolSet
from app.tools.wiki import WikiToolSet

logger = logging.getLogger("sidecar")

# MCP 服务端实例（无状态 HTTP，与内置 Go MCP 同档）
mcp = MCPServer("weknora-sidecar", version=__version__)

# 模块级 mcp 只能注册一次；build_app 在测试里可能被多次调用，用标记防重复注册
_tools_registered = False


@mcp.custom_route("/healthz", methods=["GET"])
async def healthz(_request: Request) -> Response:
    """存活探针：无需鉴权，不泄露任何配置内容。"""
    return JSONResponse({"status": "ok", "service": "weknora-mcp-sidecar", "version": __version__})


def build_app(settings: Settings):
    """组装 ASGI 应用：MCP 应用在外层包鉴权中间件。"""
    db = Database(settings.db_dsn)
    endpoint_store = EndpointStore(db)
    permission_store = PermissionStore(db)

    # Redis 滑窗限流：未配置 REDIS_ADDR 时走进程内内存兜底
    redis_client = build_redis_client(settings)
    limiter = SlidingWindowLimiter(redis_client)
    # 工具裁决器（1-C 工具统一调用；tools/list 的端点级过滤待定方案）
    tool_guard = ToolGuard(permission_store, limiter)

    # 后端 REST 客户端 + 检索工具集（只读 1-5），REST 基址/凭证来自环境配置
    rest_client = RESTClient(
        settings.weknora_base_url,
        settings.weknora_api_key,
        verify_ssl=settings.verify_ssl,
    )
    toolset = RetrieveToolSet(rest_client, tool_guard)
    wiki_toolset = WikiToolSet(rest_client, tool_guard)
    # ask 非只读；SIDECAR_SESSION_SECRET 未配置时工具内部 fail-closed
    ask_toolset = AskToolSet(
        rest_client, tool_guard,
        session_secret=settings.session_secret,
        ask_timeout=settings.ask_timeout_seconds,
    )
    register_tools(toolset, wiki_toolset, ask_toolset)

    mcp_app = mcp.streamable_http_app(
        streamable_http_path="/mcp",
        stateless_http=True,
    )
    # 纯 ASGI 包装：/healthz 在中间件内放行，/mcp 必须过完整裁决；
    # lifespan 桥在 MCP 应用关停后顺带关闭后端 httpx 连接池
    return MCPAuthMiddleware(
        _LifespanShutdownBridge(mcp_app, rest_client.aclose),
        endpoint_store=endpoint_store,
        jwt_secret=settings.jwt_secret,
    ), {
        "db": db,
        "permission_store": permission_store,
        "limiter": limiter,
        "tool_guard": tool_guard,
        "redis_client": redis_client,
        "rest_client": rest_client,
    }


class _LifespanShutdownBridge:
    """lifespan 透传桥：HTTP 作用域原样下放；lifespan 事件转发给内层 MCP 应用。

    Starlette 1.x 已移除 add_event_handler，而 MCP 应用自身需要 lifespan 来管理
    会话清理任务，不能在外层吞掉。这里用两个队列把 startup/shutdown 消息转发给
    内层，等内层关停完成后再执行外部关停回调（关闭 httpx 连接池）。
    """

    def __init__(self, inner, on_shutdown) -> None:
        self._inner = inner
        self._on_shutdown = on_shutdown

    async def __call__(self, scope, receive, send) -> None:
        if scope["type"] != "lifespan":
            await self._inner(scope, receive, send)
            return

        to_inner: asyncio.Queue = asyncio.Queue()
        from_inner: asyncio.Queue = asyncio.Queue()
        inner_task = asyncio.create_task(
            self._inner(scope, to_inner.get, from_inner.put)
        )
        while True:
            message = await receive()
            await to_inner.put(message)
            reply = await from_inner.get()
            if message["type"] == "lifespan.shutdown":
                # 内层先停，再关连接池；回调失败不影响关停应答
                with contextlib.suppress(Exception):
                    await self._on_shutdown()
                await send(reply)
                await inner_task
                return
            await send(reply)


def register_tools(*toolsets) -> None:
    """把各工具集注册到模块级 mcp；进程内只注册一次（测试重复 build_app 时跳过）。"""
    global _tools_registered
    if _tools_registered:
        return
    for toolset in toolsets:
        toolset.register(mcp)
    _tools_registered = True


def build_redis_client(settings: Settings):
    """按配置构造 redis.asyncio 客户端；未配置地址返回 None（限流走内存兜底）。"""
    if not settings.redis_addr:
        return None
    password = settings.redis_password or None
    return redis_asyncio.Redis(
        host=settings.redis_addr.split(":")[0],
        port=int(settings.redis_addr.split(":")[1]) if ":" in settings.redis_addr else 6379,
        db=settings.redis_db,
        password=password,
        socket_timeout=2,
        socket_connect_timeout=2,
        decode_responses=True,
    )


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")
    settings = Settings.from_env()
    app, _resources = build_app(settings)
    if not settings.db_dsn:
        logger.warning("SIDECAR_DB_DSN 未配置：/mcp 将全部返回 503（fail-closed），仅 /healthz 可用")
    if not settings.jwt_secret:
        logger.warning("JWT_SECRET 未配置：携带用户票的请求将全部 401（机器模式不受影响）")
    if not settings.session_secret:
        logger.warning("SIDECAR_SESSION_SECRET 未配置：ask 工具将 fail-closed 拒绝服务")
    logger.info("weknora-mcp-sidecar %s 启动：%s:%d", __version__, settings.host, settings.port)
    uvicorn.run(app, host=settings.host, port=settings.port, log_level="info")


# uvicorn app.main:app 使用的模块级实例
app, _app_resources = build_app(Settings.from_env())


if __name__ == "__main__":
    main()
