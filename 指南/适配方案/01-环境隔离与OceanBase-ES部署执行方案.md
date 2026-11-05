# 01 · qj-weknora 环境隔离与 OceanBase 4.2 / ES 8.18 部署执行方案

> 基线：WeKnora `pg-fqx` 分支，VERSION 0.8.3，ParadeDB v0.22.2-pg17（PostgreSQL 17 内核）+ Redis + Neo4j。
> 项目目录：`/root/home/workspace_cribc/fqx_weknora/qj-weknora`（以下简称 **qj-weknora**）。
> 目标：将 qj-weknora 从旧 pg-fqx 调试版的共享基础设施中彻底隔离，并最终迁移至 OceanBase 4.2（MySQL 兼容租户）+ Elasticsearch 8.18。
> 前置文档：[00 · 迁移调研总方案](./00-迁移调研总方案-OceanBase4.2-ES8.18-Nebula3.5.3.md)（本方案是其落地执行篇，不重复调研结论）。

---

## 1. 服务器现状盘点（已核实，2026-11-05）

### 1.1 三套 WeKnora 实例对照

| 实例 | 目录 | 后端 | 前端 | 基础设施 |
|---|---|---|---|---|
| 常规模式 | `zzh_weknora/WeKnora` | 8080 (docker) | 1180 (nginx) | docker 全栈，内部网络 |
| 开发正式版 (pg-zzh) | `my_weknora/WeKnora` | **8082** | **5173** | 共享 dev 基建 |
| 开发调试版 (旧 pg-fqx) | `fqx_weknora/WeKnora` | **8083** | **5174** | 共用 pg-zzh 基建 |
| **本项目 qj-weknora** | `fqx_weknora/qj-weknora` | 未启动 | 未启动 | 拷贝自旧 pg-fqx，待隔离 |

### 1.2 共享基建容器（compose project `weknora-dev`）

| 容器 | 宿主机端口 | 用途 |
|---|---|---|
| `WeKnora-postgres-dev` | **65432** → 5432 | ParadeDB pg17 |
| `WeKnora-redis-dev` | **16379** → 6379 | Redis 7 |
| `WeKnora-docreader-dev` | **15051** → 50051 | 文档解析 gRPC |
| `WeKnora-neo4j-dev` | 17474 / 17687 | 图谱（可选） |

### 1.3 关键暗坑：启动脚本硬编码了共享端口

`scripts/dev.sh` 与 `scripts/dev-app-nohup.sh` 在加载 `.env` / `.env.local` **之后**，无条件执行：

```bash
export DB_HOST=127.0.0.1
export DB_PORT=65432              # 硬编码，覆盖 .env.local
export REDIS_ADDR=127.0.0.1:16379 # 硬编码
export DOCREADER_ADDR=127.0.0.1:15051
```

> 后果：即使 `.env.local` 写了 `DB_PORT=65433`，也会被启动脚本改回 65432，qj-weknora 一旦启动就直连共享 PG。**这是隔离必须先断裂的耦合点。**

### 1.4 OB / ES 现状

服务器上**无任何 OceanBase 或 Elasticsearch 容器**（2881 / 9200 均无监听），需从零部署。

---

## 2. 隔离决策矩阵

| 组件 | 决策 | 理由 |
|---|---|---|
| **主数据库** | 🔴 完全独立（先过渡 PG，后切 OB） | 用户明确要求隔离；本就计划换 OB。绝不共享 65432 |
| **Redis** | 🟡 独立容器 | 共享 Redis 会导致 asynq 解析任务被旧 pg-fqx 抢走、同账号互踢 session |
| **DocReader** | 🟢 复用共享 15051 | 无状态 gRPC，只做格式解析；Redis 独立后队列不交叉，共享零风险 |
| **文件存储** | 🔴 独立目录 | 删除文件互不影响；`.env.local` 中指向 my_weknora 的行删除 |
| **Neo4j/图谱** | ⚪ 暂不启用 | 图谱属 C 线（Nebula），本期只做 OB+ES |
| **MinIO** | ⚪ 不用 | 开发模式 `STORAGE_TYPE=local` |

---

## 3. qj-weknora 目标端口规划

| 组件 | 端口 | 说明 |
|---|---|---|
| qj 后端 | **8085** | 8080/8082/8083/8084 全被占 |
| qj 前端 Vite | **5175** | 5173/5174 被占 |
| 过渡独立 PG | **65433** | 与共享 65432 错开，同镜像 paradedb-pg17 |
| 独立 Redis | **16380** | 与共享 16379 错开 |
| OceanBase 4.2 | **2881** (SQL) | 迁移目标主库 |
| Elasticsearch 8.18 | **9200** (HTTP) | 向量 + 全文检索 |
| DocReader（复用） | 15051 | 无状态共享 |

---

## 4. 分阶段执行计划

### 阶段 1：环境隔离（零外部依赖，1-2 天）

**目标**：qj-weknora 跑在自己的过渡 PG + 独立 Redis 上，与共享 dev 基建彻底断开。

| 步骤 | 操作 | 涉及文件 |
|---|---|---|
| 1.1 | 后端端口 8083→8085 | `config/config.yaml:3` |
| 1.2 | 前端端口 5174→5175，代理目标 8083→8085 | `frontend/vite.config.ts:53,131` |
| 1.3 | 新建独立基建 compose（project `weknora-qj`）：PG 65433 + Redis 16380 | 新建 `docker-compose.qj.yml` |
| 1.4 | 改启动脚本硬编码为"尊重 .env.local" | `scripts/dev.sh`、`scripts/dev-app-nohup.sh` |
| 1.5 | 重写 `.env.local`（独立 PG/Redis/存储，关图谱） | `.env.local` |
| 1.6 | 启动独立基建 + 后端 + 前端，验证数据落在 65433 | — |

**验收标准**：旧 pg-fqx（8083/5174）与 qj（8085/5175）同时运行，互相删数据互不影响；`psql -p 65433` 能看到自建表，`psql -p 65432` 看不到 qj 的数据。

---

### 阶段 2：部署 ES 8.18，检索后端切 ES（低风险）

**为什么先 ES**：OB MySQL 模式无 pgvector/pgsearch 扩展，不先把检索流量从 PG 挪走，主库就不能换 OB。而 ES v8 适配器代码已完整实现（`internal/application/repository/retriever/elasticsearch/v8/repository.go`）。

| 步骤 | 操作 |
|---|---|
| 2.1 | 部署 ES 8.18 单节点（加入 `docker-compose.qj.yml`），4C8G / 堆 4G，开 HTTPS+鉴权，**装 IK 分词插件** |
| 2.2 | `.env` / `.env.local` 切 `RETRIEVE_DRIVER=elasticsearch_v8`，配置 `ELASTICSEARCH_*` |
| 2.3 | POC：中文分词对比（ik_max_word vs standard），建库→解析→向量 kNN→BM25→混合+rerank→删除全链路冒烟 |
| 2.4 | 验证 `_cat/indices` 看到自动建索引，`_search` 返回向量命中 |

**验收标准**：知识库问答走 ES，过渡 PG 不再承担向量/BM25 检索。

---

### 阶段 3：部署 OceanBase 4.2，代码适配主库方言（核心改造）

| 步骤 | 操作 |
|---|---|
| 3.1 | 拿到 OB 4.2 企业版介质 + 授权（**外部依赖，立即申请**）；POC 用 docker 单节点 all-in-one |
| 3.2 | 引入 `gorm.io/driver/mysql`，`container.go` 注册第三条 dialect |
| 3.3 | 迁移器 `migration.go` 增加 mysql driver 路径 |
| 3.4 | 新建 `migrations/oceanbase/`：以当前最新 PG schema 为基线快照生成全量初始化 SQL |
| 3.5 | 改造 42 处 `.Raw/.Exec` PG 方言：jsonb→JSON、ILIKE→LOWER() LIKE 或走 ES、ON CONFLICT→ON DUPLICATE KEY UPDATE、uuid_generate_v4()→应用层、BIGSERIAL→BIGINT AUTO_INCREMENT |
| 3.6 | 配置切 `DB_DRIVER=mysql`，指向 OB 2881 |

> 本阶段代码改动最大，建议独立 git 分支开发，不影响阶段 1/2 已跑通的隔离环境。

---

### 阶段 4：数据迁移

| 数据 | 路径 |
|---|---|
| 业务表 | 过渡 PG（65433）→ obloader/DataX → OB（2881） |
| embeddings | **不进 OB**，从过渡 PG 批量回填 ES（cli 加 `reindex` 子命令） |
| 校验 | 三层：行数对账 → checksum 抽样 → 登录/知识库列表/对话历史/任务队列/Wiki 接口回归 |

---

### 阶段 5：联调验证 + 部署固化

- `/health` 健康检查扩展 OB/ES 连通性
- compose / helm / `.env.example` 同步、文档登记到 `指南/变更文档/`
- 保留过渡 PG 一周作回滚兜底

---

## 5. 风险与待决策

| # | 事项 | 建议 |
|---|---|---|
| 🔴 | OB 企业版介质/授权到位时间不可控 | 立即申请；阶段 1/2 不依赖 OB，先行 |
| 🔴 | 启动脚本硬编码端口 | 阶段 1.4 必须改 |
| 🟡 | OB MySQL 模式 ≠ 完全 MySQL 兼容 | 一切以 OB 实测为准 |
| 🟡 | ES 中文分词召回质量 | 阶段 2.3 真实语料对比后再切流量 |
| ❓ | 是否需要过渡独立 PG | 要：OB 代码未改完前，qj 仍需可跑的库做开发 |
| ❓ | 图谱（Nebula）是否本期做 | 不做，本期只交付 OB+ES，Nebula 另立二期 |

---

## 6. 关联文档

- [00 · 迁移调研总方案](./00-迁移调研总方案-OceanBase4.2-ES8.18-Nebula3.5.3.md)
- [开发模式部署指南2.md](../开发模式部署指南2.md) —— qj-weknora 独立环境的日常运维手册
