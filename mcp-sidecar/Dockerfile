# WeKnora MCP 侧车镜像（阶段 2：鉴权链 + 9 工具 = 8 只读 + ask）
FROM python:3.12-slim

WORKDIR /app

# 先装依赖，利用构建缓存
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# 再拷业务代码
COPY app ./app

# 非 root 运行
RUN useradd --create-home --uid 10001 sidecar \
    && chown -R sidecar:sidecar /app
USER sidecar

ENV SIDECAR_HOST=0.0.0.0 \
    SIDECAR_PORT=8000 \
    WEKNORA_BASE_URL=http://app:8080/api/v1

EXPOSE 8000

# 直连 ASGI 应用；凭证形态在请求时自识别（无票=机器，带合法票=用户）
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000"]
