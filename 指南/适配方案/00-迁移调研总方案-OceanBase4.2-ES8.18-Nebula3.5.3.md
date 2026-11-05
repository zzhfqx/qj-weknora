# 基础设施迁移调研总方案

> 目标栈：**OceanBase 4.2（企业版）+ Elasticsearch 8.18 + NebulaGraph 3.5.3（企业版）**
> 当前基线：WeKnora `pg-fqx` 分支，VERSION 0.8.3，ParadeDB v0.22.2-pg17（PostgreSQL 17 内核）+ Redis + Neo4j 2025.10.1（可选），向量/检索后端可插拔。
> 文档性质：**调研阶段（Phase 0）总方案**，用于圈定验证范围、排期和资源申请；不是最终实施方案。每条结论后标注证据位置，便于复核。

---

## 1. 背景、目标与范围

### 1.1 迁移目标

| 组件 | 当前实现 | 目标版本 | 替换性质 |
|---|---|---|---|
| 关系型数据库 | ParadeDB/PostgreSQL 17（[docker-compose.yml:521](../../docker-compose.yml)） | OceanBase 4.2 企业版（**建议 MySQL 兼容租户**） | 方言迁移（PG → MySQL） |
| 全文/向量检索 | PG 扩展三件套：`pg_search`(BM25) + `vector`(halfvec/HNSW) + `pg_trgm` | Elasticsearch 8.18 | 能力迁移（后端已内置适配器，重点在验证与切换） |
| 知识图谱 | Neo4j 2025.10.1 + APOC（[docker-compose.yml:625](../../docker-compose.yml)） | NebulaGraph 3.5.3 企业版 | 驱动/方言/建模三重改造 |

### 1.2 范围内 / 范围外

**范围内**

- 应用代码的方言适配、配置开关、部署物（compose / helm / 文档）
- 存量数据（业务表、embeddings、图谱）的迁移与校验方案
- 三条线之间的依赖排序与灰度/回滚策略

**范围外（本期不做）**

- Redis、MinIO、Qdrant/Milvus/Weaviate/Doris 等其他可选后端的下线评估（保持可插拔现状）
- 上游官方版本同步（遵循二开约定：Neo4j 相关代码本就搁置、不随官方升级，Nebula 适配作为本分支独立二开能力维护）
- 前端交互改版（图谱可视化 API 契约尽量保持不变）

### 1.3 关键前置判断

**三条线不是平行的：B 线（ES）是 A 线（OceanBase）的前置条件。**

OceanBase MySQL 模式不具备 PostgreSQL 的 `vector`/`pg_search`/`pg_trgm` 扩展（[000002_embeddings.up.sql:16-18](../../migrations/versioned/000002_embeddings.up.sql)），而当前默认检索后端 `RETRIEVE_DRIVER=postgres` 把向量、BM25 都落在主库里（[.env.example:269](../../.env.example)）。不先把检索流量切到 ES，主库就无法换成 OceanBase。建议顺序：**B（ES 接管检索）→ A（主库切 OB）→ C（图谱切 Nebula）**，C 线与 A/B 无数据依赖，可并行启动、最后切换。

---

## 2. 现状摸排摘要

### 2.1 关系库现状（A 线）

| 项 | 现状 | 证据 |
|---|---|---|
| ORM | GORM，仅注册 postgres、sqlite 两个 Dialect | [container.go](../../internal/container/container.go)、[go.mod:90-91](../../go.mod) |
| MySQL 驱动 | `go-sql-driver/mysql` 已在依赖中，但**只用于 Doris 检索后端**，主库未接入；也没有 `gorm.io/driver/mysql` | [go.mod:25](../../go.mod)、[retriever/doris](../../internal/application/repository/retriever/doris) |
| Schema 管理 | golang-migrate，运行器只支持 postgres/sqlite3 两个 driver | [migration.go:13-14](../../internal/database/migration.go)、[:106-107](../../internal/database/migration.go) |
| 版本化迁移 | `migrations/versioned/` 共 **104 对** up/down（PG 方言）；sqlite 16 对 | `migrations/versioned/` |
| 遗留 MySQL 脚本 | `migrations/mysql/00-init-db.sql` 只有上游时代 7 张表的快照，**未被运行器引用，不可直接用于 OB** | [00-init-db.sql](../../migrations/mysql/00-init-db.sql) |
| PG 专有语法密度 | 104 个 up 文件中 31 个含方言专有物：`jsonb` 59 处、`uuid_generate_*` 26 处、`BIGSERIAL/SERIAL` 15 处、`pg_trgm` 11 处、`tsvector/GIN` 2 处、`USING bm25` 1 处 | `migrations/versioned/*.up.sql` |
| Go 侧裸 SQL | `internal/` 下 `.Raw(`/`.Exec(` 共 **42 处**；含 `ILIKE/jsonb/::强转/paradedb/tsvector/ON CONFLICT` 等 PG 特征的命中 **82 处**，集中在 wiki_page、message、chunk、organization 等 repository | 见附录 A |
| Model 层 | `internal/types/` 154 个 Go 文件；`serializer:json` 等 GORM 标签约 48 处（依赖 GORM 跨方言序列化，风险较低但需逐个回归） | `internal/types/` |
| 自动建表 | 基本不用 AutoMigrate，schema 以 SQL 文件为准（仅 sqlite 检索后端自建 1 张表） | [retriever/sqlite/repository.go:46](../../internal/application/repository/retriever/sqlite/repository.go) |

### 2.2 检索后端现状（B 线）——好消息：适配器已存在

| 项 | 现状 | 证据 |
|---|---|---|
| 后端抽象 | 9 种后端：postgres / elasticsearch_v7 / elasticsearch_v8 / opensearch / qdrant / milvus / weaviate / doris / tencent_vectordb，经工厂按 `RETRIEVE_DRIVER` 装配，支持逗号分隔多驱动 | [engine_factory.go:67-83](../../internal/container/engine_factory.go)、[.env.example:269-278](../../.env.example) |
| ES 客户端 | go-elasticsearch **v7.17.10 与 v8.19.6 双版本并存**；目标服务端 8.18 走 v8 客户端（8.19 客户端对 8.18 服务端向前兼容，需 POC 确认） | [go.mod:20-21](../../go.mod) |
| ES v8 适配器 | 已实现 Save/BatchSave/Delete*/Retrieve/**VectorRetrieve(kNN)**/**KeywordsRetrieve(BM25)**/CopyIndices/批量启停/标签更新等完整方法；索引不存在时自动建索引并探测字段类型 | [v8/repository.go:344](../../internal/application/repository/retriever/elasticsearch/v8/repository.go)（820 行）；v7 1452 行 |
| 当前默认 | postgres：向量走 halfvec+HNSW，全文走 `paradedb.score(id)` BM25 | [retriever/postgres/repository.go:215](../../internal/application/repository/retriever/postgres/repository.go)、[:432](../../internal/application/repository/retriever/postgres/repository.go) |
| 混合编排 | 关键词+向量混合检索、rerank 已有统一编排层 | [keywords_vector_hybrid_indexer.go](../../internal/application/service/retriever/keywords_vector_hybrid_indexer.go)、`NewKVHybridRetrieveEngine` |
| 部署 | compose **不含 ES 服务**，定位为外部可选后端；helm 同样无 ES 模板 | [docker-compose.yml](../../docker-compose.yml) |
| PG 侧附加全文 | wiki_pages 上还有 2 处 `to_tsvector + GIN` 索引与若干 `pg_trgm` 模糊查询，**不在检索适配器抽象内**，切库后必须另行安置 | [000037_wiki_and_indexing.up.sql:74](../../migrations/versioned/000037_wiki_and_indexing.up.sql)、[000041:143](../../migrations/versioned/000041_task_queue_and_wiki_indexes.up.sql) |

### 2.3 图谱现状（C 线）——好消息：Cypher 高度集中

| 项 | 现状 | 证据 |
|---|---|---|
| 驱动 | neo4j-go-driver v6，**全仓只有 3 个 Go 文件 import** | [go.mod:42](../../go.mod) |
| Cypher 集中度 | **所有 Cypher 语句集中在同一个文件**：[retriever/neo4j/repository.go](../../internal/application/repository/retriever/neo4j/repository.go)（967 行），handler/service 层不直写 Cypher | 全仓 grep `MATCH (`/`MERGE (` 仅命中该文件 |
| 抽象接口 | `RetrieveGraphRepository` 仅 **9 个方法**：AddGraph / DelGraph / SearchNode + 6 个可视化方法（概览/ego 图/搜索/统计/关系类型/节点详情） | [retriever_graph.go](../../internal/types/interfaces/retriever_graph.go) |
| 服务层 | `Neo4jGraphService` 135 行薄封装；抽取链路 `extract.go` 917 行与图驱动无关（产出 GraphData） | [graph_neo4j.go](../../internal/application/service/graph_neo4j.go)、[extract.go](../../internal/application/service/extract.go) |
| 消费方 | 图谱 RAG：chat_pipeline 的 `search_entity`/`search_parallel`；可视化：`handler/graph_neo4j.go` + system 健康检查 | [search_entity.go:75](../../internal/application/service/chat_pipeline/search_entity.go)、[handler/system.go:611-615](../../internal/handler/system.go) |
| 开关 | `NEO4J_ENABLE` 非 true 时 driver 返回 nil，功能整体降级，已有空驱动容忍逻辑 | [container.go:1566-1604](../../internal/container/container.go) |
| **重度 APOC 依赖** | 写入：`apoc.merge.node`、`apoc.merge.relationship`、`apoc.coll.union`、`apoc.periodic.iterate`；查询：`apoc.path.subgraphAll`（已有变长路径 fallback） | [repository.go:66-77](../../internal/application/repository/retriever/neo4j/repository.go)、[:98-101](../../internal/application/repository/retriever/neo4j/repository.go)、[:140-152](../../internal/application/repository/retriever/neo4j/repository.go)、[:427-466](../../internal/application/repository/retriever/neo4j/repository.go)、[:668-669](../../internal/application/repository/retriever/neo4j/repository.go) |
| 图模型 | **每个知识库一个动态 Label**（`kbLabel(kbID)`），节点以 `name` 为合并键，属性 `attributes[]`、`chunks[]`、`kg`(文档 id)；关系类型由抽取 prompt 动态产生 | [repository.go:40](../../internal/application/repository/retriever/neo4j/repository.go)、[:248](../../internal/application/repository/retriever/neo4j/repository.go) |
| 部署 | compose neo4j 服务（profile 门控）+ helm `neo4j:` 段 | [docker-compose.yml:625](../../docker-compose.yml)、[helm/values.yaml:437](../../helm/values.yaml) |

> 结论：C 线代码改造面**不大但很深**。难点不在换驱动，而在 ① 强 schema 与"每 KB 一个动态 Label / 动态关系类型"的冲突；② APOC 四个能力在 nGQL 中的等价写法与性能；③ openCypher 兼容子集的实际覆盖度（必须拿真实 9 个方法的查询逐一 POC）。

---

## 3. 调研任务分解

### A 线：OceanBase 4.2 企业版

#### A1. 环境与租户模式确认（0.5 人天）

- 向供货/运维方确认：4.2.x 具体小版本（建议 4.2.1 LTS 线）、企业版安装介质与授权、集群拓扑（至少 1-1-1 三节点用于功能验证）。
- **待决策（D1）**：租户兼容模式选 **MySQL（推荐）** 还是 Oracle。推荐 MySQL 模式的依据：Doris 后端已在用 MySQL 驱动与方言、`migrations/mysql/` 有历史脚本可参考、GORM mysql driver 成熟、团队/运维熟悉度高。下文任务均按 MySQL 模式展开。

#### A2. 方言差异全量核对（核心，3 人天）

以 104 对迁移 SQL + 82 处 Go 裸 SQL 为输入，产出《PG→OceanBase 方言差异清单》，逐项给出"OB 写法/规避方案/影响文件"。必验项：

| PG 用法（出现处） | OceanBase MySQL 模式方向 | 备注 |
|---|---|---|
| `jsonb`（59 处） | `JSON` 类型；`->>`/`@>`/`?` 等操作符改 `JSON_EXTRACT/JSON_CONTAINS` | OB 4.2 JSON 函数覆盖度需实测 |
| `uuid_generate_v4()`（26 处） |应用层生成 UUID（Go 侧已多为字符串 ID，确认是否仅默认值用到）或 `UUID()` | 注意去掉连字符差异 |
| `BIGSERIAL/SERIAL`（15 处） | `BIGINT AUTO_INCREMENT`；自增步长、分库分表语义 | OB 自动增量全局唯一不保证连续 |
| `ILIKE` / `~` / `pg_trgm`（多处） | **不做 LIKE 平移**：模糊检索改走 ES；其余用 `LOWER(...) LIKE` 或 OB 全文索引（ngram） | 与 B 线强相关，见 B5 |
| `to_tsvector + GIN`（wiki 2 处） | 并入 ES 索引或 OB 倒排/全文索引 POC | 需定 Wiki 检索归属 |
| `USING bm25` / `paradedb.score` | 整体废弃，由 ES BM25 接管 | B 线完成后删除 |
| `vector(halfvec) + HNSW` | 不迁移，embeddings 表数据整体迁入 ES kNN | 见 B3 |
| `ON CONFLICT ... DO UPDATE/RETURNING` | `ON DUPLICATE KEY UPDATE`；GORM 层面多数已用 Upsert 钩子 | 检查 clauses.OnConflict 的方言兼容 |
| `::regclass`、`DO $$ ... $$` 存储过程、条件迁移 | 改写为 OB 兼容过程体或由 Go 迁移器承担条件判断 | 如 000002 依赖 `current_setting('app.skip_embedding')` |
| schema（`public.`、search_path） | MySQL database 概念，确认多租户是否用库隔离 | 当前未见强 schema 依赖，需确认 |
| 时区 `TIMESTAMP WITH TIME ZONE` | `TIMESTAMP` + 会话 `time_zone`，核对 Asia/Shanghai 行为 | compose 已统一 TZ |
| 保留字/大小写 | MySQL 保留字（如 `rank`、`condition`、`groups`）撞名检查 | 全量导入时让 OB 报错清单驱动 |
| 事务/锁 | 确认 `SELECT ... FOR UPDATE`、隔离级别（OB 默认 READ COMMITTED，PG 是 RC 也一致）、唯一约束语义 | 任务队列表重点验 |

- 方法：先把 104 个 up 文件在空 OB 库**顺序裸跑一遍**，以报错驱动清单；再对 42 处 `.Raw/.Exec` 逐个审计登记。

#### A3. 迁移体系改造调研（1.5 人天）

- 引入 `gorm.io/driver/mysql` 与 golang-migrate 的 `mysql` driver，在 [migration.go](../../internal/database/migration.go) 增加第三条路径，保持"按 driver 选择迁移目录"的既有结构。
- **待决策（D2）**：OceanBase 的版本化迁移 SQL 怎么产出？备选：
  - 方案一（推荐）：新建 `migrations/oceanbase/`，以当前 PG 最新 schema 为**基线快照**生成一份全量初始化 SQL，之后的增量迁移双份维护（PG/OB），OB 侧版本号从 1 开始。
  - 方案二：把 104 对逐个翻译——工作量大且无历史包袱价值，不推荐。
- GORM 配置差异：DSN（parseTime/loc/sql_mode）、索引前缀长度（utf8mb4 下 varchar 索引）、`ORDER BY` 忽略大小写行为差异、慢 SQL 日志。
- 新增/修改代码注释一律中文（遵循分支约定）。

#### A4. 数据迁移与双跑校验调研（2 人天）

- 工具选型 POC：obloader（官方，推荐）/ DataX / 自研 Go ETL。重点：JSON 字段、时间格式、NULL、大字段（chunk 文本、wiki content）。
- embeddings 表**不进 OB**（随 B 线进 ES）；迁移前需要冻结写入或做增量同步窗口设计。
- 校验三层：行数对账、checksum 抽样对账、应用层关键接口回归（登录、知识库列表、对话历史、任务队列、Wiki 页面）。
- 全量切换 vs 双写灰度的取舍：建议**一次性切换 + 可回滚保留 PG 一周**（应用无分库中间件，双写改造成本高于停机窗口成本；最终由 D3 拍板）。

#### A5. 工作量预估（A 线，含 POC 不含实施）

调研+POC 约 **7 人天**；后续实施（翻译基线 SQL、改 42 处裸 SQL、迁移演练 2 轮）粗估 **15–20 人天**，实施阶段估算在调研报告评审后细化。

---

### B 线：Elasticsearch 8.18

#### B1. 现有 v8 适配器对 8.18 的兼容性验证（1 人天）

- 搭单节点 ES 8.18（开启 HTTPS/鉴权的生产形态也要测，不能只测裸 http），用现有 `ELASTICSEARCH_*` 配置直连。
- 跑通：建库（自动建索引 [v8/repository.go:344](../../internal/application/repository/retriever/elasticsearch/v8/repository.go)）→ 文档解析入库 → 向量检索 → 关键词检索 → 混合+rerank → 删除/启停/移动（CopyIndices）全链路。
- 重点核对自动 mapping 探测逻辑在 8.18 上是否产生预期 mapping（dense_vector 维度、`index: true`、相似度参数；8.x 默认 kNN 用 HNSW，与现有代码写法的兼容）。

#### B2. 中文分词（必须 POC，1 人天）

- 默认 standard 分词对中文按字切，BM25 召回质量与 ParadeDB 中文分词行为不同。POC 对比：**IK 分词器（ik_max_word）** vs 8.18 内置 `analysis-ik`?（注意：ES 官方无内置 IK，需装 medcl/elasticsearch-analysis-ik 插件，先确认 8.18 对应插件版本可得性）vs ngram。
- 输出：索引 settings/mapping 模板（含 analyzer）、对现有自动建索引逻辑的改造点（可能需要显式 mapping 而非自动探测）。
- 用真实语料（抽 100 篇知识库文档）做召回对比：PG BM25 vs ES，统计 Top-10 命中率差异。

#### B3. 向量数据从 PG halfvec 迁入 ES（1.5 人天）

- 盘点：embeddings 表维度（不同模型多维度并存，dimension 字段区分）、source_type 种类、量级（行数、占用 GB）。
- 回填路径调研：现有 `CopyIndices`（租户/KB 间移动）能否改造成"PG→ES 批量灌库"的一次性 backfill 命令；还是在 cli 下新增 `reindex` 子命令更干净（推荐后者）。
- 切换期一致性：边写边迁的增量窗口（按 updated_at 追平）→ 切 `RETRIEVE_DRIVER=elasticsearch_v8` → 回归。

#### B4. 混合检索打分与配置（0.5 人天）

- ES kNN 分数与 BM25 分数量纲不同，确认 `NewKVHybridRetrieveEngine` 的归一化/RRF 逻辑对 ES 返回分的处理（postgres 后端同样走过该层，理论透明，需实测验证）。
- 多驱动并存语义：是否短暂保留 `RETRIEVE_DRIVER=postgres,elasticsearch_v8` 作灰度。

#### B5. Wiki/通用模糊查询安置（1 人天）

- wiki_pages 的 `to_tsvector` 全文索引、`pg_trgm` 模糊匹配、以及业务代码里 ILIKE 查询（约 20 个 repository 文件命中）逐类登记：
  - 走向 A：进 ES（需要把 wiki 页面也建成可检索索引，复用文档索引管线）；
  - 走向 B：保留在 OB，用全文索引/`LIKE` 覆盖低频管理类模糊查询。
- 产出《模糊查询处置矩阵》：查询场景 × 性能要求 × 去向。

#### B6. 部署与资源（0.5 人天）

- compose 增加 ES 8.18 服务（配置安全选项、数据卷、JVM 堆），helm 增加模板；许可证：Elastic License v2 / SSPL 双许可，内部使用无问题，分发镜像需法务确认（D4）。
- 容量规划：按 embeddings 行数 × 维度 × 1.4（HNSW 开销）+ 原文文本估算。

#### B7. 工作量预估（B 线）

调研+POC 约 **5.5 人天**；实施（mapping 中文化、backfill 命令、部署物）粗估 **6–10 人天**。

---

### C 线：NebulaGraph 3.5.3 企业版

#### C1. 环境与授权（0.5 人天）

- 向供货方取得企业版安装介质、授权文件与版本说明（企业版相对社区版的增量能力，如运维平台/权限/备份，确认哪些被本方案依赖）。
- 部署 graphd / metad / storaged 三件套（POC 可 1 台一体机 all-in-one，压测用 3 节点）；打通 nebula-console。

#### C2. 数据模型重设计（核心，2 人天）——最大风险点

现状是**无 schema 约束**：每 KB 动态建 Label，关系类型由 LLM 动态产出。Nebula 是强 schema（TAG/EDGE TYPE 必须预定义），必须二选一：

- **方案甲（推荐调研）：单标签 + 属性分区**
  - `TAG entity(name string, kb_id string, attributes string /*JSON*/, kg string, created_at timestamp)`，以 `kb_id + name` 做复合业务键；
  - `EDGE relate(rel_type string, kg string, attributes string)` 单一边类型，原关系名放属性；
  - 优点：schema 稳定、DDL 零动态；缺点：图原生按边类型过滤退化为属性过滤，需测性能。
- 方案乙：按 KB 动态 `CREATE TAG/EDGE TYPE`——DDL 随业务写、元数据膨胀、企业版多租户治理困难，仅作对照。
- 属性数组 `attributes[]/chunks[]`：Nebula 支持 FIXED_STRING/string，**3.x 对 list 属性的索引/更新支持弱**，倾向存 JSON 字符串或拆关联顶点，POC 中实测 `UPDATE VERTEX ... SET` 的合并语义（对应 APOC 的 `apoc.coll.union` 追加去重）。
- 必须设计**幂等写**：Nebula 3.x 的 `INSERT ... IF NOT EXISTS`/`WHEN NOT EXISTS`、`UPDATE` 语义在同顶点并发下的行为要测（当前写链路靠 apoc.merge 保证重复抽取不产生重复点/边）。

#### C3. 9 个接口方法的 nGQL/openCypher 等价 POC（核心，3 人天）

| Neo4j 现状 | Nebula 待验写法 | POC 要点 |
|---|---|---|
| `apoc.merge.node` 批量幂等写点（UNWIND $data） | `INSERT VERTEX IF NOT EXISTS` + `UPDATE VERTEX` 两步，或 openCypher `MERGE`（3.5 支持度以实测为准） | 吞吐：万级点批量写入耗时 |
| `apoc.merge.relationship` | `INSERT EDGE IF NOT EXISTS` + rank 处理重复边 | 同名多点之间重复关系的去重语义 |
| `apoc.coll.union` 属性追加去重 | 读出-合并-写回，或 list 函数（支持度实测） | 并发抽取同一热点实体时的丢失更新 |
| `apoc.periodic.iterate` 批量删除 | `GO ... | YIELD ... | DELETE` 分片提交 | 删整个 KB 的图（DelGraph）耗时 |
| `apoc.path.subgraphAll`（ego 子图，带 label/关系白名单） | `GET SUBGRAPH`/`GO N STEPS`/openCypher 变长路径 `-[e:relate*1..N]-` | **重点**：方案甲下边类型收敛为一个后，rel_type 白名单过滤要在结果层做；深度 2-3、limit 下的性能 |
| 变长路径 fallback `*1..N` | nGQL `GO 1..N STEPS` | 同上，互为对照 |
| GetGraphOverview（按度数 top-N + 点间关系） | `MATCH ... RETURN count()` 或 `GO` + 度统计 | 大图（千万边级）top-N 是否需要预聚合 |
| SearchNodes 名称模糊搜索 | 前缀/正则（`STARTS WITH`/`=~`）与 ES 外挂索引的取舍 | Nebula 自身无全文索引时模糊搜性能；企业版是否带全文能力 |
| SearchNode（GraphRAG 实体定向，[search_entity.go:75](../../internal/application/service/chat_pipeline/search_entity.go)） | 精确 name 命中 + 邻居展开 | 这是对话链路，延迟预算需明确（建议 P95 < 300ms） |

- 每个方法产出：可执行 nGQL 语句、与 Neo4j 返回结构的字段映射（保持 `types.Neo4jGraphData` 等 DTO 不变，前端零改动）、性能数据。

#### C4. Go 驱动与工程接入（1 人天）

- 驱动选型：`github.com/vesoft-inc/nebula-go/v3`（v3 对应 3.x 服务端），连接池、会话模型（graphd session 非线程安全约束）、认证、超时/重试。
- 工程形态：新增 `internal/application/repository/retriever/nebula/` 实现 9 方法接口；container 按 `GRAPH_DRIVER=nebula|neo4j|none` 装配（**不沿用 NEO4J_ENABLE 单开关，新增通用开关但向后兼容**）。
- resultSet 解析为强类型结构样板、错误码处理（Tag 不存在/边不存在等）、单元测试夹具。
- 注释全部中文；不删除 neo4j 包（搁置策略延续，但也不随上游升级它）。

#### C5. 存量图谱迁移（1 人天）

- 两条路径对比：
  - 路径甲（推荐）：**不迁数据，重建**。图谱本就是 LLM 抽取的派生产物，按 KB 重跑抽取任务即可（抽取链路 [extract.go](../../internal/application/service/extract.go) 与驱动无关）；成本是 LLM 调用费和时间，收益是天然适配新模型、无脏数据。
  - 路径乙：Neo4j 导出 CSV（apoc.export）→ nebula-importer。仅在重建成本不可接受时备选，需处理动态 label/type → 方案甲单标签的折算。
- 统计当前启用图谱的 KB 数与文档量，给重建成本算账（token 估算），作为 D5 决策输入。

#### C6. 部署物（0.5 人天）

- compose 增加 nebula 服务组（graphd/metad/storaged + 数据卷；企业版按供货方部署文档，可能是独立集群不在 compose 内）；helm 增加模板或指向外部集群的连接配置。
- 备份策略（企业版 BR）、监控、磁盘规划。

#### C7. 工作量预估（C 线）

调研+POC 约 **8 人天**（C2/C3 是硬骨头）；实施（新适配器约 1.5–2k 行 Go、部署物、切换演练）粗估 **18–25 人天**。

---

### X 线：交叉事项

| # | 事项 | 说明 |
|---|---|---|
| X1 | 配置体系统一 | DB_DRIVER 增 oceanbase 取值；RETRIEVE_DRIVER 切 elasticsearch_v8；新增 GRAPH_DRIVER；`.env.example`/`config.yaml`/compose/helm 四处同步，中文注释 |
| X2 | 健康检查/就绪探针 | `/health` 当前含 Neo4j 引擎名判断（[system.go:611](../../internal/handler/system.go)）；需扩展为三种新组件的连通性检查与初始化等待（OB/ES/Nebula 启动顺序） |
| X3 | CI 与测试 | 现有 sqlite 路径可承担部分 DB 方言测试；新增 OB/ES/Nebula 的测试容器（testcontainers 或 docker compose profile），至少覆盖迁移 SQL 可干净跑通、检索 golden case、9 个图方法 |
| X4 | 灰度顺序 | B 先（检索可双驱动灰度）→ A（主库停窗切换，保留 PG 回滚）→ C（按 KB 灰度：新抽取写 Nebula、老 KB 保持 Neo4j 或整体重建后切换） |
| X5 | 文档与二开记录 | 部署文档、`指南/变更文档` 中按既定「二开/版本升级」结构在**功能实际完成后**登记；调研阶段不登记 |
| X6 | 备份与回滚 | 切换前 PG 全量备份 + 镜像保留；ES reindex 别名策略（alias 切换秒级回滚）；Nebula 企业版 BR 演练 |

---

## 4. 调研环境与资源需求

| 资源 | 规格建议（POC） | 用途 | 提供方 |
|---|---|---|---|
| OceanBase 4.2 企业版集群 | 1-1-1 三节点（或单节点 all-in-one），8C16G×3 + SSD | A2/A3 | 运维/供货方，含授权 |
| Elasticsearch 8.18 | 单节点 4C8G（堆 4G），可联网装 IK 插件 | B1/B2 | 自行 docker 部署即可 |
| NebulaGraph 3.5.3 企业版 | all-in-one 起；另需 3 节点（graphd/metad/storaged）做 C3 性能验证 | C1–C3 | 供货方介质+授权 |
| 脱敏数据副本 | 一份生产库脱敏快照 + 对应 embeddings；启用图谱的 KB 原始文档 | 全链路 POC | 业务方 |
| LLM 额度 | 图谱重建成本试算与小批量实跑 | C5 | 业务方 |
| 人力 | 1 名 Go 后端主力全程；另需 0.3 人力运维支持环境 | — | — |

---

## 5. 排期建议（调研期约 4 周）

| 周次 | A 线 OB | B 线 ES | C 线 Nebula |
|---|---|---|---|
| 第 1 周 | 环境到位；104 个 SQL 裸跑报错清单（A1/A2） | ES 8.18 环境 + v8 适配器冒烟（B1） | 企业版环境；nebula-go 连通（C1/C4 预研） |
| 第 2 周 | 裸 SQL 审计 + 迁移器第三路径 demo（A2/A3） | 中文分词 POC + 混合打分（B2/B4） | **模型重设计评审（C2）**，出模型设计稿 |
| 第 3 周 | obloader/DataX 灌库演练（A4） | PG→ES backfill 原型 + Wiki 模糊查询处置（B3/B5） | **9 方法 nGQL 逐个 POC（C3）**，出性能数据 |
| 第 4 周 | 应用连 OB 联调（绕开检索依赖，用 sqlite/ES 替身） | 语料召回对比报告 + 部署物（B6） | 重建 vs 导入试算（C5）；三线调研报告汇总评审 |

调研评审通过的准入条件：① 方言清单无未决高风险项；② ES 在真实语料上的召回/延迟达标；③ Nebula 9 方法全部有可跑通等价语句且 ego 图延迟在预算内；④ 三条线实施排期与人天估算签字确认。

---

## 6. 风险登记册

| 级别 | 风险 | 影响 | 缓解 |
|---|---|---|---|
| 🔴 高 | Nebula 强 schema 与动态 Label/关系冲突，方案甲属性过滤导致大图 ego 查询性能不达标 | C 线可能需要额外建索引/预聚合，甚至调整前端交互（限制深度/边类型选择） | C2/C3 前置 POC，用百万边级仿真数据压测，第 2 周末设 go/no-go 检查点 |
| 🔴 高 | 82 处 PG 方言裸 SQL 审计后出现遗漏，切 OB 后隐蔽功能报错 | 业务故障 | 裸跑报错 + 代码审计双轨；testcontainers 回归；保留 PG 一周回滚 |
| 🔴 高 | ES 中文分词导致召回质量下降 | 问答效果回退 | B2 真实语料对比，分词方案评审后再切；切后用线上 query 抽样回放 |
| 🟡 中 | openCypher 兼容子集不覆盖 MERGE/变长路径，退回纯 nGQL 后开发量上升 | C 线工期 +30% | C3 第一周就专门验证 MERGE 与变长路径，不把假设留到实施期 |
| 🟡 中 | embeddings 回填期间新旧写入不一致 | 检索缺条 | backfill 支持增量追平 + 切换窗口双读校验 |
| 🟡 中 | OceanBase 企业版 / Nebula 企业版授权与介质到位时间不可控 | 全线阻塞 | 第 0 周立即启动申请；ES 先行不等 |
| 🟡 中 | OB MySQL 模式 JSON 函数/全文索引行为与 MySQL 官方存在差异 | A2 清单返工 | 一切以 OB 实测为准，不采信"MySQL 兼容=完全兼容"文档表述 |
| 🟢 低 | ES 8.19 客户端连 8.18 服务端的边角兼容 | 偶发告警 | B1 冒烟覆盖；必要时降客户端版本 |
| 🟢 低 | 前端 DTO 字段名仍叫 `Neo4jGraphData` | 仅命名不雅 | 实施期保留类型名避免大 diff，注释说明为通用图 DTO |

---

## 7. 待决策问题（请拍板）

| # | 决策点 | 选项 | 建议 |
|---|---|---|---|
| D1 | OB 租户兼容模式 | MySQL / Oracle | **MySQL** |
| D2 | OB 迁移 SQL 产出方式 | 基线快照起版本 / 翻译全部 104 对 | **基线快照** |
| D3 | 主库切换方式 | 停窗一次性切+保留回滚 / 应用层双写灰度 | **停窗切换**（数据量不大时），需提供维护窗口 |
| D4 | ES 镜像分发 | 官方镜像自带 / 内部仓库自建 | 内部仓库；许可内部使用无限制 |
| D5 | 图谱存量数据 | LLM 抽取重建 / CSV 导入 | 待 C5 成本试算后定，倾向**重建** |
| D6 | GRAPH_DRIVER 是否长期保留 neo4j 选项 | 保留双后端 / 只留 Nebula | **保留开关**（代码已天然接口化，保留成本低） |
| D7 | Wiki 全文检索归属 | 并入 ES / 留 OB 全文索引 | 调研后按 B5 矩阵定，倾向**并入 ES** |

---

## 8. 调研交付物清单

1. 《A 线：PG→OceanBase 方言差异清单》（含每处文件行号、改写 SQL、验证结果）
2. 《A 线：数据迁移演练报告》（obloader/DataX 实测、耗时、对账结果）
3. 《B 线：ES 8.18 适配器验证报告》（功能矩阵、mapping 模板、中文分词对比、召回/延迟数据）
4. 《B 线：PG→ES 回填与切换手册》
5. 《C 线：Nebula 图模型设计》（TAG/EDGE/索引/幂等写方案）
6. 《C 线：9 接口 nGQL 对照与 POC 报告》（含 Cypher↔nGQL 语句对照、压测数据）
7. 《C 线：图谱数据重建/导入成本评估》
8. 《总体切换 Runbook 与回滚预案》（X4/X6）
9. 更新后的部署物：compose、helm、`.env.example`、部署文档（实施期产出，调研期只出草案）

---

## 附录 A：PG 方言裸 SQL 命中集中的文件（审计起点）

`internal/` 下含 ILIKE/jsonb/类型转换/tsvector 等特征的主要文件（排查顺序按命中密度）：

- [repository/wiki_page.go](../../internal/application/repository/wiki_page.go)
- [repository/message.go](../../internal/application/repository/message.go)、[service/message.go](../../internal/application/service/message.go)
- [repository/chunk.go](../../internal/application/repository/chunk.go)、[repository/chunk_image_assets.go](../../internal/application/repository/chunk_image_assets.go)
- [repository/session.go](../../internal/application/repository/session.go)、[types/session.go](../../internal/types/session.go)
- [repository/user.go](../../internal/application/repository/user.go)
- [repository/organization.go](../../internal/application/repository/organization.go)
- [repository/mcp_metadata.go](../../internal/application/repository/mcp_metadata.go)
- [service/wiki_ingest.go](../../internal/application/service/wiki_ingest.go)
- [utils/inject.go](../../internal/utils/inject.go)
- 各检索后端 repository（postgres/sqlite/weaviate/milvus/tencentvectordb，多数为各自后端方言，A 线只需确认 postgres 包在 OB 场景彻底摘除）

## 附录 B：调研期禁止事项（遵循分支约定）

- 不修改、不升级 Neo4j 相关代码（搁置策略不变；Nebula 为新增平行实现）
- 新增/修改代码注释一律中文
- 调研 POC 代码放在独立分支/目录，不进主干
- `指南/变更文档` 只在功能**实际完成**后登记，调研阶段不写入
