# Codex2API 部署文档

本文档以 **源码编译部署（Linux + PostgreSQL）** 为默认方案，Docker 仅作为可选补充。

## 目录

- [部署模式概览](#部署模式概览)
- [源码编译部署（推荐）](#源码编译部署推荐)
- [systemd 托管](#systemd-托管)
- [本地开发](#本地开发)
- [Docker（可选）](#docker可选)
- [升级指南](#升级指南)
- [备份与恢复](#备份与恢复)

---

## 部署模式概览

| 模式 | 适用场景 | 数据库 | 缓存 |
|------|----------|--------|------|
| **源码编译（推荐）** | 生产长期运行 | PostgreSQL | Memory / Redis |
| 本地开发 | 功能开发、联调 | PostgreSQL / SQLite | Memory / Redis |
| Docker（可选） | 容器化验证或现有容器环境 | PostgreSQL / SQLite | Redis / Memory |

---

## 源码编译部署（推荐）

### 1. 环境要求

- Linux（Ubuntu 22.04+ 推荐）
- Go 1.21+
- Node.js 18+
- PostgreSQL 14+

### 2. 拉取代码

```bash
git clone https://github.com/Cong0707/codex2api.git
cd codex2api
```

### 3. 初始化 PostgreSQL

```bash
sudo -u postgres psql <<'SQL'
CREATE USER codex2api WITH PASSWORD 'your_db_password';
CREATE DATABASE codex2api OWNER codex2api;
GRANT ALL PRIVILEGES ON DATABASE codex2api TO codex2api;
SQL
```

### 4. 配置 `.env`

```bash
cp .env.example .env
```

最小示例（按你的环境修改）：

```bash
CODEX_PORT=7317
ADMIN_SECRET=your_admin_secret

DATABASE_DRIVER=postgres
DATABASE_HOST=127.0.0.1
DATABASE_PORT=5432
DATABASE_USER=codex2api
DATABASE_PASSWORD=your_db_password
DATABASE_NAME=codex2api
DATABASE_SSLMODE=disable

CACHE_DRIVER=memory
TZ=Asia/Shanghai
```

### 5. 构建前端并编译后端

```bash
cd frontend
npm ci
npm run build
cd ..

go build -o codex2api .
```

### 6. 启动验证

```bash
./codex2api
```

验证：

- 管理台：`http://127.0.0.1:7317/admin/`
- 健康检查：`http://127.0.0.1:7317/health`

---

## systemd 托管

创建 `/etc/systemd/system/codex2api.service`：

```ini
[Unit]
Description=Codex2API
After=network.target postgresql.service

[Service]
Type=simple
WorkingDirectory=/opt/codex2api
ExecStart=/opt/codex2api/codex2api
EnvironmentFile=/opt/codex2api/.env
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

启动与查看：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now codex2api
sudo systemctl status codex2api --no-pager
sudo journalctl -u codex2api -f
```

---

## 本地开发

### 后端

```bash
cp .env.example .env
cd frontend && npm ci && npm run build && cd ..
go run .
```

### 前端联调

```bash
cd frontend
npm ci
npm run dev
```

访问：`http://127.0.0.1:5173/admin/`

---

## Docker（可选）

Docker 不是默认推荐路径，仅在你明确需要容器化时使用。

### 本地源码容器构建（推荐于 Docker 路线）

```bash
cp .env.example .env
docker compose -f docker-compose.local.yml up -d --build
docker compose -f docker-compose.local.yml logs -f codex2api
```

如果你在 Docker 路线里启用 Redis，可参考下面这组环境变量：

```env
# 服务端口
CODEX_PORT=8080

# 管理后台密码（强密码推荐）
ADMIN_SECRET=your-strong-password-here

# 数据库配置（PostgreSQL 模式）
DATABASE_DRIVER=postgres
DATABASE_HOST=postgres
DATABASE_PORT=5432
DATABASE_USER=codex2api
DATABASE_PASSWORD=your-db-password
DATABASE_NAME=codex2api

# Redis 配置
CACHE_DRIVER=redis
REDIS_ADDR=redis:6379
REDIS_USERNAME=
REDIS_PASSWORD=your-redis-password
REDIS_DB=0
REDIS_TLS=false
REDIS_INSECURE_SKIP_VERIFY=false

# 时区
TZ=Asia/Shanghai
```

云 Redis（如 Aiven、Upstash）通常需要 TLS，可直接使用平台提供的 `rediss://` 连接串：

```env
CACHE_DRIVER=redis
REDIS_ADDR=rediss://default:your-redis-password@your-redis-host:6379/0
```

**可选配置：**

### SQLite 容器构建
```bash
cp .env.sqlite.example .env
docker compose -f docker-compose.sqlite.local.yml up -d --build
```

如果你的 Redis 来自 Aiven、Upstash 等云服务，通常需要 TLS，可直接使用平台提供的 `rediss://...` 连接串；如果仍使用 `host:port`，请额外设置 `REDIS_TLS=true`。

如果你要使用镜像拉取模式，请改成你自己的镜像仓库地址，不要依赖历史上游镜像地址。

---

## 升级指南

### 源码部署升级（推荐）

```bash
git pull
cd frontend && npm ci && npm run build && cd ..
go build -o codex2api .
sudo systemctl daemon-reload
sudo systemctl restart codex2api
sudo systemctl status codex2api --no-pager
```

### Docker 路线升级（可选）

```bash
git pull
docker compose -f docker-compose.local.yml up -d --build
docker compose -f docker-compose.local.yml logs -f codex2api
```

---

## 备份与恢复

### PostgreSQL 备份

```bash
pg_dump -h 127.0.0.1 -U codex2api -d codex2api > backup_$(date +%Y%m%d_%H%M%S).sql
```

### PostgreSQL 恢复

```bash
psql -h 127.0.0.1 -U codex2api -d codex2api < backup_xxx.sql
```

### SQLite 备份

```bash
cp /data/codex2api.db /backup/codex2api_$(date +%Y%m%d_%H%M%S).db
```

### SQLite 恢复

```bash
cp /backup/codex2api_xxx.db /data/codex2api.db
```
