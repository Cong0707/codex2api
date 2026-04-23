# 增强代理池管理

本文档描述了 codex2api 的增强代理池管理功能。

## 功能概述

增强代理池管理提供了以下核心功能：

1. **代理健康检查**
   - 定期 HTTP HEAD 检查
   - 响应时间监控
   - HTTP/SOCKS5 代理支持

2. **代理轮换策略**
   - 轮询 (Round-robin)
   - 加权选择 (Weighted)
   - 最少连接 (Least Connections)

3. **代理池状态监控**
   - 健康代理列表
   - 代理统计信息
   - 实时状态更新

4. **账号级代理优先级**
   - 账号单独配置的 `proxy_url` 优先
   - Refresh Token 刷新、测试连接、用量探针会按账号 ID 粘性选择代理池出口
   - 未启用代理池时回退到全局 `ProxyURL`

5. **代理性能统计**
   - 成功率跟踪
   - 延迟监控
   - 活跃连接数
   - 总请求数

6. **故障代理自动隔离**
   - 连续失败阈值检测
   - 自动隔离故障代理
   - 隔离期满后自动恢复

## 核心数据结构

### ProxyPool

```go
type ProxyPool struct {
    proxies   []*ProxyEntry
    stats     map[string]*ProxyStats
    healthy   []*ProxyEntry
    // ... 配置和运行时状态
}
```

### ProxyEntry

```go
type ProxyEntry struct {
    URL                 string
    Healthy             bool
    LastCheck           time.Time
    Latency             time.Duration
    SuccessRate         float64
    Weight              int64
    ActiveConns         int64
    TotalRequests       int64
    FailedRequests      int64
    Status              ProxyHealthStatus
    IsolatedAt          time.Time
    ConsecutiveFailures int
}
```

### ProxyStats

```go
type ProxyStats struct {
    URL                 string
    Healthy             bool
    LastCheck           time.Time
    Latency             time.Duration
    LatencyMs           float64
    SuccessRate         float64
    Weight              int64
    ActiveConns         int64
    TotalRequests       int64
    FailedRequests      int64
    Status              string
    ConsecutiveFailures int
}
```

## API 使用示例

### 创建代理池

```go
config := &auth.ProxyPoolConfig{
    Strategy:           auth.StrategyRoundRobin,
    CheckInterval:      30 * time.Second,
    Timeout:            10 * time.Second,
    IsolationThreshold: 3,
    IsolationDuration:  5 * time.Minute,
    HealthCheckURL:     "http://www.google.com/generate_204",
}

pool := auth.NewProxyPool(config)
```

### 添加代理

```go
pool.AddProxy("http://proxy1:8080", 10)
pool.AddProxy("http://proxy2:8080", 5)
```

### 选择代理

```go
// 使用默认策略（轮询）
entry := pool.Select()
if entry != nil {
    fmt.Println("Selected proxy:", entry.URL)
}

// 使用加权策略
pool.SetStrategy(auth.StrategyWeighted)
entry := pool.Select()

// 使用最少连接策略
pool.SetStrategy(auth.StrategyLeastConnections)
entry := pool.Select()
```

### 标记成功/失败

```go
// 标记代理成功
pool.MarkSuccess("http://proxy1:8080")

// 标记代理失败
pool.MarkFailure("http://proxy1:8080")
```

### 获取统计信息

```go
stats := pool.GetStats()
for url, stat := range stats {
    fmt.Printf("Proxy: %s, Success Rate: %.2f, Latency: %.2fms\n",
        url, stat.SuccessRate, stat.LatencyMs)
}

status := pool.GetPoolStatus()
fmt.Printf("Total: %d, Healthy: %d, Isolated: %d\n",
    status.Total, status.Healthy, status.Isolated)
```

### 启动健康检查

```go
// 设置回调
pool.SetOnHealthCheck(func(result *auth.HealthCheckResult) {
    if !result.Healthy {
        log.Printf("Proxy %s health check failed: %v", result.URL, result.Error)
    }
})

pool.SetOnIsolation(func(entry *auth.ProxyEntry) {
    log.Printf("Proxy %s is isolated due to consecutive failures", entry.URL)
})

pool.SetOnRecovery(func(entry *auth.ProxyEntry) {
    log.Printf("Proxy %s has recovered", entry.URL)
})

// 启动定期健康检查
pool.StartHealthCheck()

// 停止
pool.Stop()
```

### 连接管理

```go
// 获取连接
if pool.AcquireConnection("http://proxy1:8080") {
    // 使用代理...

    // 释放连接
    pool.ReleaseConnection("http://proxy1:8080")
}
```

## 策略说明

### 轮询 (Round-robin)

按顺序轮流选择每个代理，适用于负载均匀分布的场景。

### 加权选择 (Weighted)

根据代理权重进行随机选择，权重高的代理被选中的概率更大。适用于代理性能不均衡的场景。

### 最少连接 (Least Connections)

选择当前活跃连接数最少的代理。适用于长连接场景，可以更好地均衡负载。

## 健康检查机制

1. **定期检查**：按配置的间隔时间定期检查所有代理
2. **响应时间监控**：记录每次检查的响应时间
3. **健康状态更新**：根据检查结果更新代理健康状态
4. **自动隔离**：连续失败超过阈值的代理会被自动隔离
5. **自动恢复**：隔离期满后，代理会重新参与健康检查，成功后自动恢复

## 配置环境变量

```bash
# 代理池策略: round_robin, weighted, least_connections
PROXY_POOL_STRATEGY=round_robin

# 健康检查间隔
PROXY_POOL_CHECK_INTERVAL=30s

# 健康检查超时
PROXY_POOL_TIMEOUT=10s

# 隔离阈值（连续失败次数）
PROXY_POOL_ISOLATION_THRESHOLD=3

# 隔离持续时间
PROXY_POOL_ISOLATION_DURATION=5m

# 健康检查 URL
PROXY_POOL_HEALTH_CHECK_URL=http://www.google.com/generate_204
```

## 代理生效优先级

对于已经选定账号的内部请求（Refresh Token 刷新、账号测试、用量探针），代理按以下优先级动态解析：

```text
账号 proxy_url > 账号 ID 粘性代理池 > 全局 ProxyURL > 直连
```

这意味着全局 `ProxyURL` 或代理池配置更新后会立即影响后续刷新请求，不需要重启服务；同时，已有账号自己的 `proxy_url` 不会被覆盖。

## 与 Store 集成

项目提供了 `EnhancedProxyPool` 和 `StoreProxyPoolIntegration` 用于与现有 Store 集成：

```go
// 创建增强代理池
enhancedPool := auth.NewEnhancedProxyPool(db, config)

// 初始化
err := enhancedPool.Init(ctx)

// 获取代理
proxyURL := enhancedPool.NextProxy()

// 标记成功/失败
enhancedPool.MarkProxySuccess(proxyURL)
enhancedPool.MarkProxyFailure(proxyURL)
```

## 文件列表

- `auth/proxy_pool.go` - 核心代理池实现
- `auth/proxy_pool_test.go` - 代理池单元测试
- `auth/proxy_pool_integration.go` - Store 集成实现
- `auth/proxy_pool_integration_test.go` - 集成测试

## 测试

运行测试：

```bash
cd /path/to/project
go test -v ./auth/
```

检查覆盖率：

```bash
go test -cover ./auth/
```
