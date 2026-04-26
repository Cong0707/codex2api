# Codex2API API 文档

本文档详细描述 Codex2API 的所有 API 端点、请求/响应格式以及错误码说明。

## 目录

- [概述](#概述)
- [认证](#认证)
- [公共 API](#公共-api)
- [管理 API](#管理-api)
- [错误码](#错误码)
- [限流说明](#限流说明)

---

## 概述

Codex2API 提供兼容 OpenAI 风格的 API 接口，同时包含完整的管理后台 API。

**推荐 Base URL:** `http://localhost:8080/v1`

**兼容 Base URL:** `http://localhost:8080`（当客户端不方便携带 `/v1` 前缀时，可直接使用根路径兼容路由）

**请求格式:**
- 请求头: `Content-Type: application/json`
- 认证头: `Authorization: Bearer <api_key>`

---

## 认证

### API Key 认证

公共 API (`/v1/*`) 需要 API Key 进行认证。

**请求头:**
```http
Authorization: Bearer sk-xxxxxxxxxxxxxxxxxxxxxxxx
```

**配置方式:**
1. 通过管理后台 `/admin/settings` 页面配置
2. 如果没有配置任何 API Key，则 `/v1/*` 接口跳过鉴权（开发模式）

### Admin Secret 认证

管理 API (`/api/admin/*`) 需要 Admin Secret 进行认证。

**请求头:**
```http
X-Admin-Key: your-admin-secret
```

或

```http
Authorization: Bearer your-admin-secret
```

**配置方式:**
- 环境变量: `ADMIN_SECRET`
- 数据库: 通过管理后台设置

---

## 公共 API

### 1. Chat Completions

**端点:** `POST /v1/chat/completions`

**说明:** OpenAI 风格的 Chat Completions 接口，支持流式和非流式响应。

**兼容端点:** `POST /chat/completions`

**请求示例:**
```json
{
  "model": "gpt-5.4",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": false,
  "reasoning_effort": "medium",
  "service_tier": "fast"
}
```

**参数说明:**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| model | string | 是 | 模型名称，见 [支持模型](#支持模型) |
| messages | array | 是 | 消息列表 |
| stream | boolean | 否 | 是否启用流式响应，默认 false |
| reasoning_effort | string | 否 | 推理强度: low/medium/high |
| service_tier | string | 否 | 服务等级: fast/auto |
| max_tokens | integer | 否 | 最大输出 token 数（Codex 不支持，会被过滤） |
| temperature | float | 否 | 温度参数（Codex 不支持，会被过滤） |

**非流式响应示例:**
```json
{
  "id": "chatcmpl-xxxxxxxx",
  "object": "chat.completion",
  "created": 1712345678,
  "model": "gpt-5.4",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 25,
    "completion_tokens": 15,
    "total_tokens": 40
  }
}
```

**流式响应示例:**
```
data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.4","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}

data: {"id":"chatcmpl-xxx","object":"chat.completion.chunk","created":1712345678,"model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

### 2. Responses

**端点:** `POST /v1/responses`

**说明:** Codex 原生 Responses 接口，直接透传，无需协议翻译。

**兼容端点:** `POST /responses`、`POST /v1/responses/compact`、`POST /responses/compact`（`compact` 路径等同于 `/v1/responses`，用于兼容部分客户端 compact 请求）

**请求示例:**
```json
{
  "model": "gpt-5.4",
  "input": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Hello!"}
  ],
  "stream": false,
  "reasoning": {
    "effort": "medium"
  },
  "service_tier": "fast",
  "include": ["reasoning.encrypted_content"]
}
```

**参数说明:**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| model | string | 是 | 模型名称 |
| input | array/string | 是 | 输入内容（支持数组或字符串） |
| stream | boolean | 否 | 是否启用流式响应，默认 false。仅当显式传 `stream=true` 时返回 SSE（流式响应），否则返回普通 JSON。 |
| reasoning.effort | string | 否 | 推理强度: low/medium/high |
| service_tier | string | 否 | 服务等级: fast/auto |
| include | array | 否 | 包含的额外字段 |
| previous_response_id | string | 否 | 上一响应 ID，用于上下文连续 |

**响应示例:**
```json
{
  "id": "resp_xxxxxxxx",
  "object": "response",
  "created": 1712345678,
  "model": "gpt-5.4",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {
          "type": "output_text",
          "text": "Hello! How can I help you today?"
        }
      ]
    }
  ],
  "usage": {
    "input_tokens": 25,
    "output_tokens": 15,
    "total_tokens": 40
  }
}
```

### 3. List Models

### 3. Images

#### 生成图片

**端点:** `POST /v1/images/generations`

**说明:** OpenAI Images 兼容入口。外部请求使用 `gpt-image-2`，内部转为 Codex `/responses` 图像工具链路。

**兼容端点:** `POST /images/generations`

**请求示例:**
```json
{
  "model": "gpt-image-2",
  "prompt": "Draw a small orange cat",
  "size": "1024x1024",
  "quality": "high",
  "response_format": "b64_json"
}
```

#### 编辑图片

**端点:** `POST /v1/images/edits`

**说明:** 支持 JSON `images[].image_url` 和 multipart `image` / `image[]` 上传；`mask.image_url` 或 multipart `mask` 可用于遮罩编辑。

**兼容端点:** `POST /images/edits`

**JSON 请求示例:**
```json
{
  "model": "gpt-image-2",
  "prompt": "Replace the background with aurora lights",
  "images": [
    {"image_url": "https://example.com/source.png"}
  ],
  "output_format": "png"
}
```

**响应示例:**
```json
{
  "created": 1710000000,
  "model": "gpt-image-2",
  "data": [
    {
      "b64_json": "iVBORw0KGgoAAAANSUhEUgAA..."
    }
  ]
}
```

### 4. List Models

**端点:** `GET /v1/models`

**说明:** 获取支持的模型列表。

**兼容端点:** `GET /models`

**响应示例:**
```json
{
  "object": "list",
  "data": [
    {"id": "gpt-5.4", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5.4-mini", "object": "model", "owned_by": "openai"},
    {"id": "gpt-5", "object": "model", "owned_by": "openai"}
  ]
}
```

### 4. Health Check

**端点:** `GET /health`

**说明:** 健康检查端点，返回服务状态。

**响应示例:**
```json
{
  "status": "ok",
  "available": 5,
  "total": 8
}
```

---

## 管理 API

所有管理 API 需要 `X-Admin-Key` 请求头进行认证。

### 统计接口

#### GET /api/admin/stats

获取仪表盘统计数据。

**响应:**
```json
{
  "total": 10,
  "available": 8,
  "error": 2,
  "today_requests": 1234
}
```

#### GET /api/admin/health

系统健康检查（扩展版）。

**响应:**
```json
{
  "status": "ok",
  "available": 8,
  "total": 10
}
```

### 账号管理

#### GET /api/admin/accounts

获取账号列表。

**响应:**
```json
{
  "accounts": [
    {
      "id": 1,
      "name": "account-1",
      "email": "user@example.com",
      "plan_type": "pro",
      "status": "ready",
      "health_tier": "healthy",
      "scheduler_score": 100,
      "dynamic_concurrency_limit": 2,
      "proxy_url": "http://proxy.example.com:8080",
      "created_at": "2024-01-01T00:00:00Z",
      "updated_at": "2024-01-01T12:00:00Z",
      "active_requests": 0,
      "total_requests": 100,
      "last_used_at": "2024-01-01T11:00:00Z",
      "success_requests": 95,
      "error_requests": 5,
      "usage_percent_7d": 45.2,
      "usage_percent_5h": 12.5,
      "reset_5h_at": "2024-01-01T17:00:00Z",
      "reset_7d_at": "2024-01-08T00:00:00Z",
      "scheduler_breakdown": {
        "unauthorized_penalty": 0,
        "rate_limit_penalty": 0,
        "timeout_penalty": 0,
        "server_penalty": 0,
        "failure_penalty": 0,
        "success_bonus": 12,
        "usage_penalty_7d": -5,
        "latency_penalty": 0,
        "success_rate_penalty": 0
      }
    }
  ]
}
```

#### POST /api/admin/accounts

添加新账号（支持批量）。

**请求:**
```json
{
  "name": "my-account",
  "refresh_token": "rt_xxxxxxxxxxxx",
  "proxy_url": "http://proxy.example.com:8080"
}
```

批量添加（使用换行分隔）:
```json
{
  "name": "batch",
  "refresh_token": "rt_xxx1\nrt_xxx2\nrt_xxx3",
  "proxy_url": ""
}
```

**响应:**
```json
{
  "message": "成功添加 3 个账号",
  "success": 3,
  "failed": 0
}
```

#### DELETE /api/admin/accounts/:id

删除账号（软删除，标记为 deleted）。

**响应:**
```json
{
  "message": "账号已删除"
}
```

#### POST /api/admin/accounts/:id/refresh

手动刷新账号 Access Token。

**响应:**
```json
{
  "message": "账号刷新成功"
}
```

#### GET /api/admin/accounts/:id/test

测试账号连接。

**响应:**
```json
{
  "success": true,
  "latency_ms": 523,
  "message": "连接正常"
}
```

#### GET /api/admin/accounts/:id/usage

获取单个账号用量统计。

**响应:**
```json
{
  "id": 1,
  "name": "account-1",
  "total_requests": 100,
  "total_tokens": 5000,
  "last_7d_requests": 500,
  "last_7d_tokens": 25000
}
```

#### POST /api/admin/accounts/import

批量导入账号（支持 TXT/JSON 格式）。

**请求:**
- Method: POST
- Content-Type: multipart/form-data
- Form 字段: `file`, `format` (txt|json), `proxy_url`

**响应:** SSE 流式进度
```
data: {"type":"progress","current":5,"total":10,"success":3,"duplicate":1,"failed":1}

data: {"type":"complete","current":10,"total":10,"success":8,"duplicate":1,"failed":1}
```

#### POST /api/admin/accounts/batch-test

批量测试账号连接。

**请求:**
```json
{
  "ids": [1, 2, 3],
  "concurrency": 5
}
```

**响应:** SSE 流式进度
```
data: {"type":"progress","current":3,"total":3,"success":2,"failed":1}

data: {"type":"complete","current":3,"total":3,"success":2,"failed":1}
```

#### POST /api/admin/accounts/clean-banned

清理 Unauthorized（401）账号。

**响应:**
```json
{
  "message": "已清理 5 个账号",
  "cleaned": 5
}
```

#### POST /api/admin/accounts/clean-rate-limited

清理 Rate Limited（429）账号。

#### POST /api/admin/accounts/clean-error

清理 Error 状态账号。

#### GET /api/admin/accounts/export

导出账号（标准 JSON 格式）。

**查询参数:**
- `filter`: healthy (只导出健康账号)
- `ids`: 1,2,3 (指定 ID 列表)
- `remote`: true (远程迁移模式)

**响应:**
```json
[
  {
    "type": "codex",
    "email": "user@example.com",
    "expired": "2024-12-31T23:59:59Z",
    "id_token": "id_xxx",
    "account_id": "acc_xxx",
    "access_token": "at_xxx",
    "last_refresh": "2024-01-01T12:00:00Z",
    "refresh_token": "rt_xxx"
  }
]
```

#### POST /api/admin/accounts/migrate

从远程 codex2api 实例迁移账号。

**请求:**
```json
{
  "url": "http://remote-instance:8080",
  "admin_key": "remote-admin-secret"
}
```

**响应:** SSE 流式进度

#### GET /api/admin/accounts/event-trend

获取账号增删趋势。

**查询参数:**
- `start`: RFC3339 格式开始时间
- `end`: RFC3339 格式结束时间
- `bucket_minutes`: 聚合桶大小（默认 60）

**响应:**
```json
{
  "trend": [
    {"timestamp": "2024-01-01T00:00:00Z", "added": 5, "deleted": 0}
  ]
}
```

### 用量统计

#### GET /api/admin/usage/stats

获取使用统计。

**响应:**
```json
{
  "total_requests": 10000,
  "total_tokens": 500000,
  "today_requests": 500,
  "today_tokens": 25000,
  "rpm": 50,
  "tpm": 2500,
  "error_rate": 0.02
}
```

#### GET /api/admin/usage/logs

获取使用日志。

**查询参数:**
- `start`: RFC3339 开始时间
- `end`: RFC3339 结束时间
- `page`: 页码
- `page_size`: 每页条数 (最大 200)
- `email`: 按账号邮箱过滤
- `model`: 按模型过滤
- `endpoint`: 按端点过滤
- `api_key_id`: 按 API 密钥 ID 过滤
- `fast`: true/false (是否 fast 服务)
- `stream`: true/false (是否流式)

**响应:**
```json
{
  "logs": [
    {
      "id": 1,
      "account_id": 1,
      "account_email": "user@example.com",
      "api_key_id": 3,
      "api_key_name": "Team A",
      "api_key_masked": "sk-t****...****1234",
      "endpoint": "/v1/chat/completions",
      "model": "gpt-5.4",
      "status_code": 200,
      "duration_ms": 523,
      "first_token_ms": 150,
      "prompt_tokens": 25,
      "completion_tokens": 15,
      "total_tokens": 40,
      "created_at": "2024-01-01T12:00:00Z"
    }
  ],
  "total": 1000
}
```

#### GET /api/admin/usage/chart-data

获取图表聚合数据。

**查询参数:**
- `start`: RFC3339 开始时间
- `end`: RFC3339 结束时间
- `bucket_minutes`: 聚合桶大小（默认 5）

**响应:**
```json
{
  "buckets": [
    {
      "time": "2024-01-01T12:00:00Z",
      "requests": 50,
      "tokens": 2500,
      "latency_ms": 500
    }
  ],
  "total_requests": 1000,
  "total_tokens": 50000
}
```

#### DELETE /api/admin/usage/logs

清空使用日志。

**响应:**
```json
{
  "message": "日志已清空"
}
```

### API Key 管理

#### GET /api/admin/keys

获取所有 API 密钥。

**响应:**
```json
{
  "keys": [
    {
      "id": 1,
      "name": "default",
      "key": "sk-xxxxxxxxxxxxxxxxxxxxxxxx",
      "created_at": "2024-01-01T00:00:00Z"
    }
  ]
}
```

#### POST /api/admin/keys

创建新 API 密钥。

**请求:**
```json
{
  "name": "production",
  "key": "sk-custom-key"  // 可选，不填则自动生成
}
```

**响应:**
```json
{
  "id": 2,
  "key": "sk-xxxxxxxxxxxxxxxxxxxxxxxx",
  "name": "production"
}
```

#### DELETE /api/admin/keys/:id

删除 API 密钥。

**响应:**
```json
{
  "message": "已删除"
}
```

### 系统设置

#### GET /api/admin/settings

获取系统设置。

**响应:**
```json
{
  "max_concurrency": 2,
  "global_rpm": 0,
  "test_model": "gpt-5.4",
  "test_concurrency": 50,
  "proxy_url": "",
  "pg_max_conns": 50,
  "redis_pool_size": 30,
  "auto_clean_unauthorized": false,
  "auto_clean_rate_limited": false,
  "auto_clean_full_usage": false,
  "auto_clean_error": false,
  "proxy_pool_enabled": false,
  "fast_scheduler_enabled": false,
  "max_retries": 3,
  "allow_remote_migration": false,
  "database_driver": "postgres",
  "database_label": "PostgreSQL",
  "cache_driver": "redis",
  "cache_label": "Redis",
  "admin_secret": "",
  "admin_auth_source": "env"
}
```

#### PUT /api/admin/settings

更新系统设置。

**请求:**
```json
{
  "max_concurrency": 4,
  "global_rpm": 100,
  "test_model": "gpt-5.4",
  "test_concurrency": 50,
  "proxy_url": "http://proxy.example.com:8080",
  "auto_clean_unauthorized": true,
  "auto_clean_rate_limited": false,
  "fast_scheduler_enabled": true
}
```

**响应:** 更新后的完整设置对象

### 代理池管理

#### GET /api/admin/proxies

获取代理列表。

**响应:**
```json
{
  "proxies": [
    {
      "id": 1,
      "url": "http://proxy1.example.com:8080",
      "label": "US Proxy",
      "enabled": true,
      "last_tested_at": "2024-01-01T12:00:00Z",
      "last_test_result": "ok",
      "latency_ms": 150
    }
  ]
}
```

#### POST /api/admin/proxies

添加代理（支持批量）。

**请求:**
```json
{
  "urls": ["http://proxy1.example.com:8080", "http://proxy2.example.com:8080"],
  "label": "Batch Add"
}
```

或单条:
```json
{
  "url": "http://proxy.example.com:8080",
  "label": "US Proxy"
}
```

#### DELETE /api/admin/proxies/:id

删除代理。

#### PATCH /api/admin/proxies/:id

更新代理。

**请求:**
```json
{
  "label": "New Label",
  "enabled": false
}
```

#### POST /api/admin/proxies/batch-delete

批量删除代理。

**请求:**
```json
{
  "ids": [1, 2, 3]
}
```

#### POST /api/admin/proxies/test

测试代理连通性。

**请求:**
```json
{
  "url": "http://proxy.example.com:8080",
  "id": 1,  // 可选，用于持久化测试结果
  "lang": "zh-CN"
}
```

**响应:**
```json
{
  "success": true,
  "ip": "1.2.3.4",
  "country": "United States",
  "region": "California",
  "city": "Los Angeles",
  "isp": "Example ISP",
  "latency_ms": 150,
  "location": "United States·California·Los Angeles"
}
```

### 运维监控

#### GET /api/admin/ops/overview

获取系统运维概览。

**响应:**
```json
{
  "updated_at": "2024-01-01T12:00:00Z",
  "uptime_seconds": 86400,
  "database_driver": "postgres",
  "database_label": "PostgreSQL",
  "cache_driver": "redis",
  "cache_label": "Redis",
  "cpu": {
    "percent": 25.5,
    "cores": 8
  },
  "memory": {
    "percent": 60.2,
    "used_bytes": 6442450944,
    "total_bytes": 10737418240
  },
  "runtime": {
    "goroutines": 50,
    "available_accounts": 8,
    "total_accounts": 10
  },
  "requests": {
    "active": 5,
    "total": 10000
  },
  "postgres": {
    "healthy": true,
    "open": 10,
    "in_use": 5,
    "idle": 5,
    "max_open": 50,
    "wait_count": 0,
    "usage_percent": 20
  },
  "redis": {
    "healthy": true,
    "total_conns": 10,
    "idle_conns": 5,
    "stale_conns": 0,
    "pool_size": 30,
    "usage_percent": 33.3
  },
  "traffic": {
    "qps": 10.5,
    "qps_peak": 50.0,
    "tps": 500.0,
    "tps_peak": 2000.0,
    "rpm": 600,
    "tpm": 30000,
    "error_rate": 0.02,
    "today_requests": 5000,
    "today_tokens": 250000,
    "rpm_limit": 0
  }
}
```

### 模型管理

#### GET /api/admin/models

获取当前启用的模型列表，并返回模型注册表元数据。

**响应:**
```json
{
  "models": [
    "gpt-5.5",
    "gpt-5.4",
    "gpt-5.4-mini",
    "gpt-5.3-codex",
    "gpt-image-2"
  ],
  "items": [
    {
      "id": "gpt-image-2",
      "enabled": true,
      "category": "image",
      "source": "builtin",
      "pro_only": false,
      "api_key_auth_available": true
    }
  ],
  "source_url": "https://developers.openai.com/codex/models",
  "last_synced_at": "2026-04-24T00:00:00Z"
}
```

#### POST /api/admin/models/sync

从 OpenAI 官方 Codex 模型页同步模型注册表。同步只新增或更新模型元数据，不会自动删除本地模型；`gpt-image-2` 会始终作为内置图像模型保留。

**响应:**
```json
{
  "added": 0,
  "updated": 2,
  "unchanged": 5,
  "skipped": ["gpt-5.2-codex"],
  "models": [
    "gpt-5.5",
    "gpt-5.4",
    "gpt-5.4-mini",
    "gpt-5.3-codex",
    "gpt-image-2"
  ],
  "last_synced_at": "2026-04-24T00:00:00Z",
  "source_url": "https://developers.openai.com/codex/models"
}
```

### OAuth 授权

#### POST /api/admin/oauth/generate-auth-url

生成 OAuth 授权 URL。

**请求:**
```json
{
  "provider": "google",
  "redirect_uri": "http://localhost:8080/admin/oauth/callback"
}
```

#### POST /api/admin/oauth/exchange-code

交换授权码获取 Token。

**请求:**
```json
{
  "provider": "google",
  "code": "auth_code_from_callback",
  "redirect_uri": "http://localhost:8080/admin/oauth/callback"
}
```

---

## 支持模型

| 模型 | 说明 |
|------|------|
| gpt-5.4 | 旗舰模型 |
| gpt-5.4-mini | 轻量版 |
| gpt-5 | 标准版 |
| gpt-5-codex | Codex 专用 |
| gpt-5-codex-mini | Codex 轻量 |
| gpt-5.1 | 旧版本 |
| gpt-5.1-codex | 旧版本 Codex |
| gpt-5.1-codex-mini | 旧版本 Codex 轻量 |
| gpt-5.1-codex-max | 旧版本 Codex 最大版 |
| gpt-5.2 | 中间版本 |
| gpt-5.2-codex | 中间版本 Codex |
| gpt-5.3-codex | 较新版本 |

> 提示：实际支持的模型以 `/v1/models` 接口返回为准，文档可能未及时更新。
---

## 错误码

### HTTP 状态码

| 状态码 | 说明 |
|--------|------|
| 200 | 请求成功 |
| 400 | 请求参数错误 |
| 401 | 认证失败 |
| 403 | 权限不足 |
| 404 | 资源不存在 |
| 429 | 请求过于频繁（限流） |
| 499 | 客户端断开连接 |
| 500 | 服务器内部错误 |
| 502 | 网关错误（上游服务异常） |
| 503 | 服务不可用（账号池耗尽） |
| 598 | 上游流中断 |

### 错误响应格式

```json
{
  "error": {
    "message": "错误描述",
    "type": "错误类型",
    "code": "错误代码"
  }
}
```

### 常见错误代码

| 代码 | 说明 | 处理建议 |
|------|------|----------|
| missing_api_key | 缺少 API Key | 添加 Authorization 请求头 |
| invalid_api_key | API Key 无效 | 检查密钥是否正确 |
| authentication_error | 认证错误 | 检查 Admin Secret 或 API Key |
| invalid_request_error | 请求参数错误 | 检查请求体格式 |
| server_error | 服务器错误 | 查看日志排查问题 |
| upstream_error | 上游服务错误 | 检查 Codex 服务状态 |
| account_pool_usage_limit_reached | 账号池额度耗尽 | 等待冷却或添加新账号 |
| rate_limit_exceeded | 限流触发 | 降低请求频率 |

---

## 限流说明

### 全局 RPM 限流

通过 `global_rpm` 设置限制全局每分钟请求数。

- `global_rpm = 0`: 无限流
- `global_rpm > 0`: 启用 RPM 限流

### 账号级别限流

系统会自动根据账号状态进行限流：

- **Healthy**: 正常并发
- **Warm**: 并发减半
- **Risky**: 固定 1 并发
- **Banned**: 0 并发，不参与调度

### 429 限流响应

当账号触发限流时，响应包含 `Retry-After` 头：

```http
HTTP/1.1 503 Service Unavailable
Retry-After: 3600

{
  "error": {
    "message": "账号池额度已耗尽，请稍后重试",
    "type": "server_error",
    "code": "account_pool_usage_limit_reached",
    "plan_type": "free",
    "resets_at": 1712345678,
    "resets_in_seconds": 3600
  }
}
```

### 建议

1. 监控 `X-RateLimit-*` 响应头（如有）
2. 实现指数退避重试策略
3. 处理 429/503 状态码，根据 `Retry-After` 等待后重试
4. 避免在短时内发送大量请求
