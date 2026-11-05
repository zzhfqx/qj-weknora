# Git 独立仓库 init 全流程

> 适用场景：把已经在本地跑起来的独立项目（不是 fork 上游仓库）第一次接入 Git 版本控制并推到自己的 GitHub。
>
> 与《Git操作指南.md》的区别：那份讲的是 **fork 上游开源项目**（有 origin + upstream 双远程）；本文讲的是**从零起一个全新独立仓库**（只有 origin，没有 upstream）。
>
> 实际案例：2026-11-04 把 MCP sidecar 从 WeKnora 主项目剥离到 `/root/home/workspace_cribc/fqx_weknora/mcp-sidecar/`，独立建仓。

---

## 第 0 步：先在 GitHub 网页上空建仓库

浏览器打开 → New repository：

- 名字建议：`WeKnora-mcp-sidecar`（或 `mcp-sidecar`）
- **不要**勾 "Add README" / `.gitignore` / "license"（保持空仓库，否则首次 push 会冲突）
- 建好后复制给你的 SSH 地址，形如：`git@github.com:zzhfqx/WeKnora-mcp-sidecar.git`

> 空仓库是为了让本地 `git init` 出来的内容能直接 push 上去，不会跟 GitHub 自动生成的 README 打架。

---

## 第 1 步：进入目录并初始化

```bash
cd /root/home/workspace_cribc/fqx_weknora/mcp-sidecar
git init
```

> 输出 `Initialized empty Git repository in /root/home/workspace_cribc/fqx_weknora/mcp-sidecar/.git/`

---

## 第 2 步：设定主分支名

```bash
git branch -M main
```

> 新仓库默认用 `main`（跟 WeKnora 主项目的 `pg-fqx` 分支无关，这是独立项目）。

---

## 第 3 步：绑定远程 origin

```bash
git remote add origin git@github.com:zzhfqx/WeKnora-mcp-sidecar.git
```

> 把上面地址换成第 0 步实际建的仓库 SSH 地址。

验证：

```bash
git remote -v
# 应看到两行 origin（fetch / push），都是 git@github.com:zzhfqx/...
```

> 独立项目**不要**加 `upstream`。fork 上游才需要 upstream 用来 `git pull upstream main` 拉官方更新；独立项目没有上游血缘，加了反而迷惑。

---

## 第 4 步：确认 `.gitignore` 会挡住敏感文件

```bash
cat .gitignore
```

Python 项目至少要包含这几行：

```
__pycache__/
*.pyc
.venv/
.pytest_cache/
.env
```

> ⚠️ **关键**：`.env` 里有真实密钥（API Key、JWT secret、DB 密码），必须保证它在忽略列表里。这一步漏了，下一步 `git add .` 就会把密钥塞进 Git 历史，即使后面删文件也在历史里。

---

## 第 5 步：首次检查将被提交的文件

```bash
git status
```

**重点核对**：

- `.env` **不能**出现在列表里（出现就是出事了，停在这一步查 `.gitignore`，不要继续 add）
- `__pycache__/`、`*.pyc`、`.pytest_cache/` **不能**出现
- 应该看到的是代码、模板、文档、Dockerfile、compose 这些

以 mcp-sidecar 为例，预期列表：

```
.env.example      # 模板，别人照着配
.gitignore        # 规则
Dockerfile        # 构建
README.md         # 项目说明
app/              # 代码
deploy/           # 部署 SQL、冒烟手册、历史 compose 片段
docker-compose.yml
pyproject.toml
requirements.txt
tests/
```

如果看到 `.env`，**先停下来**，不要继续。

---

## 第 6 步：添加并首次提交

```bash
git add .
git status
```

**先看一眼** `git status` 的输出，确认：

- 每个文件名前面有 `A`（added）
- **`.env` 依然没出现**（哪怕这一步也不该出现）
- 没有 `__pycache__/`、`*.pyc`、`.pytest_cache/`

确认无误再提交：

```bash
git commit -m "chore: 从 WeKnora 主项目剥离为独立 MCP sidecar 服务

- 独立目录、独立 docker-compose（external 接入 weknora-dev 网络）
- 自带 .env.example 模板与 .gitignore（.env 已排除）
- 端到端冒烟通过：healthz / tools/list / list_knowledge_bases / ask

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

> 提交信息末尾那行 `Co-Authored-By` 按本项目约定加上。

---

## 第 7 步：首次推送（绑定上游）

```bash
git push -u origin main
```

> `-u` 绑定后，以后在这个目录里直接 `git push` / `git pull` 就行。

---

## 第 8 步：验证

```bash
git log --oneline
# 应看到刚才那一条 commit

git branch -vv
# main 应该显示 [origin/main] 跟踪关系
```

浏览器刷新 GitHub 仓库页面，代码已经上去了。

---

## 日常开发速查

```bash
cd /root/home/workspace_cribc/fqx_weknora/mcp-sidecar

# 新功能开分支
git checkout -b feat/xxx

# ...改代码...

git add .
git commit -m "feat: xxx"

# 首次推这个新分支（绑定 upstream）
git push -u origin feat/xxx

# 之后这个分支再推
git push
```

常用对照：

| 需求 | 命令 |
|---|---|
| 查看当前分支 | `git branch` |
| 查看所有分支（含远程） | `git branch -a` |
| 查看分支跟踪关系 | `git branch -vv` |
| 查看提交历史 | `git log --oneline` |
| 切换分支 | `git checkout 分支名` |
| 拉远程更新 | `git fetch origin` |
| 拉并合并当前分支 | `git pull` |
| 撤销工作区修改 | `git checkout -- 文件名` |
| 撤销最近一次提交（保留改动） | `git reset --soft HEAD~1` |
| 删本地分支 | `git branch -d 分支名` |
| 删远程分支 | `git push origin --delete 分支名` |

---

## ⚠️ 常见坑

| 坑 | 后果 | 处理 |
|---|---|---|
| 第 0 步勾了 "Add README" | push 报 `fetch first` | 先 `git pull --rebase origin main` 合并，再 push |
| 没确认 `.env` 被 ignore 就 add | **密钥直接推到 GitHub 公开历史里**，删都删不干净 | 立即 `git reset --soft HEAD~1`，把 `.env` 加进 `.gitignore`，再重新 commit |
| 用 HTTPS 而不是 SSH | 443 端口超时 | 用 `git remote set-url origin git@github.com:...` 换成 SSH |
| 把 fork 项目的 `upstream` 也加过来 | 没必要，还可能误 `git pull upstream` 拉进无关代码 | 独立项目只保留 origin |
| `git remote add origin` 报 `remote origin already exists` | origin 已存在 | 用 `git remote set-url origin <新地址>` 而不是 `add` |

---

## 本次实际执行记录（2026-11-04）

项目：`/root/home/workspace_cribc/fqx_weknora/mcp-sidecar/`

- 第 0 步：GitHub 空仓库（用户自己建）
- 第 1–3 步：`git init` → `git branch -M main` → `git remote add origin git@github.com:zzhfqx/<仓库名>.git`
- 第 4 步：`.gitignore` 已含 `.env`
- 第 5 步：`git status` 输出 10 项待跟踪，**不含** `.env` ✅
- 第 6–7 步：`git add .` → `git commit` → `git push -u origin main`
