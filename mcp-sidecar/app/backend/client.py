"""httpx 异步 REST 客户端：X-API-Key 认证、统一超时、统一错误提取。

WeKnora 成功返回包：{"success": true, "data": ...}（部分列表接口把
total/page/page_size 放在顶层与 data 平级）；
失败返回包：{"success": false, "error": {"code", "message", "details"}}。
"""

from __future__ import annotations

import json
import logging
from collections.abc import AsyncIterator

import httpx

from app.auth.middleware import current_principal

logger = logging.getLogger("sidecar.backend")

# 单次 REST 调用默认超时（秒）；检索类调用可能较慢，各方法可按需覆盖
DEFAULT_TIMEOUT_SECONDS = 30.0


class BackendError(Exception):
    """上游 REST 可预期错误（4xx/5xx、坏包、超时等），携带英文文案。"""

    def __init__(self, message: str, *, status: int = 0, code: str = "") -> None:
        super().__init__(message)
        self.message = message
        self.status = status
        self.code = code


class BackendNotFound(BackendError):
    """上游 404；tools 层翻译成"文档/知识库不存在"。"""


def extract_error(body: object) -> tuple[str, str]:
    """从 WeKnora 错误包里取 (message, code)，缺字段时给兜底文案。

    标准失败包：{"success": false, "error": {"code", "message"}}；
    wiki 等直出 gin.H 的接口错误体是 {"error": "字符串"}，同样提取原文，
    否则冻结契约里的逐 KB 错误文案（如正则非法信息）会被兜底文案吞掉。
    """
    if isinstance(body, dict):
        error = body.get("error")
        if isinstance(error, dict):
            return str(error.get("message") or "upstream request failed"), str(error.get("code") or "")
        if isinstance(error, str) and error.strip():
            return error.strip(), ""
    return "upstream request failed", ""


class RESTClient:
    """全工程共用的一个 httpx.AsyncClient 实例（连接池复用）。"""

    def __init__(
        self,
        base_url: str,
        api_key: str,
        *,
        verify_ssl: bool = True,
        timeout: float = DEFAULT_TIMEOUT_SECONDS,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._api_key = (api_key or "").strip()
        headers = {"Accept": "application/json"}
        if self._api_key:
            # 与内置 Go MCP 一致：侧车是 X-API-Key 的唯一持有者
            headers["X-API-Key"] = self._api_key
        self._client = httpx.AsyncClient(
            base_url=self._base_url,
            headers=headers,
            timeout=timeout,
            verify=verify_ssl,
            # 只读短请求；连接数按侧车并发保守设置
            limits=httpx.Limits(max_connections=32, max_keepalive_connections=16),
        )

    async def aclose(self) -> None:
        await self._client.aclose()

    async def get_json(
        self, path: str, *, params: httpx._types.QueryParamTypes | None = None, timeout: float | None = None
    ) -> dict:
        return await self.request("GET", path, params=params, timeout=timeout)

    async def post_json(
        self,
        path: str,
        *,
        params: httpx._types.QueryParamTypes | None = None,
        json_body: object | None = None,
        timeout: float | None = None,
    ) -> dict:
        return await self.request("POST", path, params=params, json_body=json_body, timeout=timeout)

    async def request(
        self,
        method: str,
        path: str,
        *,
        params: httpx._types.QueryParamTypes | None = None,
        json_body: object | None = None,
        timeout: float | None = None,
    ) -> dict:
        """发一次 REST 调用并解析外层包；任何可预期失败抛 BackendError。

        返回完整 JSON 包（dict）；data / total 等字段由各 backend 适配函数自取，
        因为列表接口的分页字段与 data 平级。
        """
        if not self._api_key:
            # fail-closed：没配钥匙绝不裸调（裸调只会被后端 401，文案还更含糊）
            raise BackendError("upstream api key is not configured")

        headers = self._tenant_headers()
        try:
            response = await self._client.request(
                method, path, params=params, json=json_body, timeout=timeout, headers=headers
            )
        except httpx.TimeoutException as exc:
            raise BackendError("upstream request timed out") from exc
        except httpx.HTTPError as exc:
            raise BackendError("upstream request failed") from exc

        try:
            body = response.json()
        except ValueError as exc:
            raise BackendError(f"upstream returned a non-JSON response (HTTP {response.status_code})") from exc

        if response.status_code >= 400:
            message, code = extract_error(body)
            logger.info("[rest] %s %s -> %s: %s", method, path, response.status_code, message)
            if response.status_code == 404:
                raise BackendNotFound(message, status=404, code=code)
            raise BackendError(message, status=response.status_code, code=code)

        if not isinstance(body, dict):
            raise BackendError("upstream response envelope is not an object")
        # 2xx 但 success=false（后端个别异常路径），同样按错误处理
        if body.get("success") is False:
            message, code = extract_error(body)
            raise BackendError(message, code=code)
        return body

    async def stream_sse_events(
        self,
        method: str,
        path: str,
        *,
        json_body: object | None = None,
        timeout: float | None = None,
    ) -> AsyncIterator[dict]:
        """打开 SSE 响应并逐条 yield data 里的 JSON 对象。

        ask 的 /knowledge-chat 与 /agent-chat 在消息落库后直接以
        text/event-stream 返回（gin SSEvent）；400/403/404 等流式建立前的
        错误仍是普通 JSON 错误包，这里统一读出来转成 BackendError。
        超时参数是"相邻事件的最大间隔"；ask 的整体 240s 期限由工具层
        asyncio.wait_for 兜底，避免长答案被读空闲误杀。
        """
        if not self._api_key:
            raise BackendError("upstream api key is not configured")

        request_kwargs: dict = {"json": json_body, "headers": self._tenant_headers()}
        if timeout is not None:
            request_kwargs["timeout"] = timeout
        try:
            stream_cm = self._client.stream(method, path, **request_kwargs)
            response = await stream_cm.__aenter__()
        except httpx.TimeoutException as exc:
            raise BackendError("upstream request timed out") from exc
        except httpx.HTTPError as exc:
            raise BackendError("upstream request failed") from exc

        try:
            if response.status_code >= 400:
                # 流式建立前的错误：响应体是普通 JSON 错误包
                try:
                    body = await response.aread()
                    payload = json.loads(body.decode("utf-8"))
                except (ValueError, UnicodeDecodeError):
                    payload = None
                message, code = extract_error(payload if isinstance(payload, dict) else {})
                logger.info("[rest:sse] %s %s -> %s: %s", method, path, response.status_code, message)
                if response.status_code == 404:
                    raise BackendNotFound(message, status=404, code=code)
                raise BackendError(message, status=response.status_code, code=code)

            content_type = response.headers.get("Content-Type", "")
            if "text/event-stream" not in content_type.lower():
                raise BackendError(
                    f"upstream returned a non-SSE response (Content-Type: {content_type or 'unknown'})"
                )

            data_lines: list[str] = []
            async for raw_line in response.aiter_lines():
                if raw_line == "":
                    # 空行分发一个事件
                    if data_lines:
                        event = self._parse_sse_data("\n".join(data_lines), method, path)
                        data_lines = []
                        if event is not None:
                            yield event
                    continue
                line = raw_line
                if line.startswith(":"):
                    # SSE 注释/心跳
                    continue
                field, sep, value = line.partition(":")
                if not sep:
                    # 畸形行（无冒号）按整行字段名处理，data 场景不会出现
                    continue
                if value.startswith(" "):
                    value = value[1:]
                if field == "data":
                    data_lines.append(value)
                # event/id/retry 字段侧车不需要（后端只发 event: message）
            if data_lines:
                # 连接关闭前缺少收尾空行时补发最后一条
                event = self._parse_sse_data("\n".join(data_lines), method, path)
                if event is not None:
                    yield event
        finally:
            await stream_cm.__aexit__(None, None, None)

    @staticmethod
    def _tenant_headers() -> dict[str, str]:
        """平台 API Key 不绑定空间，调空间接口必须显式带 X-Tenant-ID。

        从当前请求的 Principal（中间件注入的 contextvar）取端点行的 tenant_id。
        非请求上下文（如启动时健康检查、后台任务）没有 principal，返回空 dict。
        """
        principal = current_principal()
        if principal is None:
            return {}
        return {"X-Tenant-ID": str(principal.endpoint.tenant_id)}

    @staticmethod
    def _parse_sse_data(raw: str, method: str, path: str) -> dict | None:
        """解析单条 SSE data；坏 JSON 记日志跳过，不拖垮整条答案。"""
        try:
            payload = json.loads(raw)
        except ValueError:
            logger.info("[rest:sse] %s %s 返回了无法解析的事件：%.200s", method, path, raw)
            return None
        if not isinstance(payload, dict):
            return None
        return payload
