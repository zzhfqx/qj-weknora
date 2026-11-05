"""pytest 共享夹具：当前请求主体（机器端点，空公共书架；工具层 scope 由 FakeGuard 给定）。"""

from __future__ import annotations

import pytest

from fakes import principal_context as _principal_context


@pytest.fixture
def principal_context():
    with _principal_context():
        yield
