"""侧车配置：全部来自环境变量，集中在一处声明与校验。

阶段 0 只强制绑定地址；其余项（REST 地址、数据库、Redis、JWT 等）阶段 1
接线时才会成为必填，这里先给默认值/空值并写中文注释，避免骨架阶段因缺
配置无法启动。
"""

from __future__ import annotations

import os
from dataclasses import dataclass


def _env(name: str, default: str = "") -> str:
    """读取环境变量；未设置时返回默认值。"""
    return os.environ.get(name, default)


@dataclass(frozen=True)
class Settings:
    # 监听地址与端口（容器内固定 8000，宿主映射由 compose 决定）
    host: str = "0.0.0.0"
    port: int = 8000

    # WeKnora 后端 REST 基址（容器内网指向 app 服务）
    weknora_base_url: str = "http://app:8080/api/v1"
    # 调用 REST 用的工作空间 API Key（部署时只授 retrieve 只读能力）
    weknora_api_key: str = ""
    # REST 调用是否校验 TLS（自签证书可关）
    verify_ssl: bool = True

    # PostgreSQL 只读账号 DSN（仅两表 SELECT 权限，见 deploy/01_readonly_role.sql）
    db_dsn: str = ""

    # Redis 滑窗限流（复用现有实例；db 号/前缀与其他服务错开）
    redis_addr: str = ""
    redis_password: str = ""
    redis_db: int = 0

    # 与千机网关一致的票据签名密钥（HS256）
    jwt_secret: str = ""
    # 侧车自身的会话票据密钥（ask 多轮不透明票据，阶段 2 用）
    session_secret: str = ""

    # ask 工具整体超时（问答含 SSE 聚合，默认 240 秒）
    ask_timeout_seconds: int = 240

    @classmethod
    def from_env(cls) -> "Settings":
        """从环境变量装配配置；布尔/整数做显式转换。"""
        return cls(
            host=_env("SIDECAR_HOST", "0.0.0.0"),
            port=int(_env("SIDECAR_PORT", "8000")),
            weknora_base_url=_env("WEKNORA_BASE_URL", "http://app:8080/api/v1"),
            weknora_api_key=_env("WEKNORA_API_KEY"),
            verify_ssl=_env("WEKNORA_VERIFY_SSL", "true").lower() == "true",
            db_dsn=_env("SIDECAR_DB_DSN"),
            redis_addr=_env("REDIS_ADDR"),
            redis_password=_env("REDIS_PASSWORD"),
            redis_db=int(_env("REDIS_DB", "0") or "0"),
            jwt_secret=_env("JWT_SECRET"),
            session_secret=_env("SIDECAR_SESSION_SECRET"),
            ask_timeout_seconds=int(_env("ASK_TIMEOUT", "240") or "240"),
        )
