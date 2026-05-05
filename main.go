package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/codex2api/admin"
	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

//go:embed frontend/dist/*
var frontendFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Codex2API v2 启动中...")

	// 1. 加载配置 (.env)
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("加载核心环境配置失败 (请检查 .env 文件): %v", err)
	}
	log.Printf("物理层配置加载成功: port=%d, database=%s, cache=%s", cfg.Port, cfg.Database.Label(), cfg.Cache.Label())

	// 2. 初始化数据库
	db, err := database.New(cfg.Database.Driver, cfg.Database.DSN())
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	defer db.Close()
	switch cfg.Database.Driver {
	case "sqlite":
		log.Printf("%s 连接成功: %s", cfg.Database.Label(), cfg.Database.Path)
	default:
		log.Printf("%s 连接成功: %s:%d/%s", cfg.Database.Label(), cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName)
	}

	// 3. 读取运行时的系统逻辑设置（需在缓存初始化之前，以获取连接池大小）
	sysCtx, sysCancel := context.WithTimeout(context.Background(), 5*time.Second)
	settings, err := db.GetSystemSettings(sysCtx)
	sysCancel()

	if err == nil && settings == nil {
		// 初次运行，保存初始安全设置到数据库
		log.Printf("初次运行，初始化系统默认设置...")
		settings = &database.SystemSettings{
			MaxConcurrency:                   2,
			GlobalRPM:                        0,
			TestModel:                        "gpt-5.4",
			TestConcurrency:                  50,
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			RecoveryProbeIntervalMinutes:     30,
			ProxyURL:                         "",
			PgMaxConns:                       50,
			RedisPoolSize:                    30,
			AutoCleanUnauthorized:            false,
			AutoCleanRateLimited:             false,
			AutoCleanFullUsageMode:           auth.AutoCleanFullUsageModeOff,
			PlusPortEnabled:                  false,
			PlusPortAccessFree:               true,
			ImageRoutePriority:               auth.ImageRoutePriorityOfficialFirst,
			PublicInitialCreditUSD:           0.1,
			PublicFullCreditUSD:              2,
			QuotaRatePlus:                    10,
			QuotaRatePro:                     100,
			QuotaRatePro5x:                   25,
			QuotaRatePro20x:                  100,
			QuotaRateTeam:                    10,
		}
		_ = db.UpdateSystemSettings(context.Background(), settings)
	} else if err != nil {
		log.Printf("警告: 读取系统设置失败: %v，将采用安全后备策略", err)
		settings = &database.SystemSettings{
			MaxConcurrency:                   2,
			GlobalRPM:                        0,
			TestModel:                        "gpt-5.4",
			TestConcurrency:                  50,
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			RecoveryProbeIntervalMinutes:     30,
			PgMaxConns:                       50,
			RedisPoolSize:                    30,
			PlusPortEnabled:                  false,
			PlusPortAccessFree:               true,
			ImageRoutePriority:               auth.ImageRoutePriorityOfficialFirst,
			PublicInitialCreditUSD:           0.1,
			PublicFullCreditUSD:              2,
			QuotaRatePlus:                    10,
			QuotaRatePro:                     100,
			QuotaRatePro5x:                   25,
			QuotaRatePro20x:                  100,
			QuotaRateTeam:                    10,
		}
	} else {
		log.Printf("已加载持久化业务设置: ProxyURL=%s, MaxConcurrency=%d, GlobalRPM=%d, PgMaxConns=%d, RedisPoolSize=%d",
			settings.ProxyURL, settings.MaxConcurrency, settings.GlobalRPM, settings.PgMaxConns, settings.RedisPoolSize)
	}

	// 4. 初始化缓存（使用数据库中保存的连接池大小）
	redisPoolSize := 30
	if settings.RedisPoolSize > 0 {
		redisPoolSize = settings.RedisPoolSize
	}
	var tc cache.TokenCache
	switch cfg.Cache.Driver {
	case "memory":
		tc = cache.NewMemory(redisPoolSize)
	default:
		tc, err = cache.NewRedisWithOptions(cache.RedisOptions{
			Addr:               cfg.Cache.Redis.Addr,
			Username:           cfg.Cache.Redis.Username,
			Password:           cfg.Cache.Redis.Password,
			DB:                 cfg.Cache.Redis.DB,
			PoolSize:           redisPoolSize,
			TLS:                cfg.Cache.Redis.TLS,
			InsecureSkipVerify: cfg.Cache.Redis.InsecureSkipVerify,
		})
		if err != nil {
			log.Fatalf("缓存初始化失败: %v", err)
		}
	}
	defer tc.Close()
	switch cfg.Cache.Driver {
	case "memory":
		log.Printf("%s 缓存已启用: pool_size=%d", cfg.Cache.Label(), redisPoolSize)
	default:
		log.Printf("%s 连接成功: %s, pool_size=%d", cfg.Cache.Label(), cache.RedactRedisAddr(cfg.Cache.Redis.Addr), redisPoolSize)
	}

	// 4b. 应用数据库连接池设置
	if settings.PgMaxConns > 0 {
		db.SetMaxOpenConns(settings.PgMaxConns)
		log.Printf("%s 连接池: max_conns=%d", cfg.Database.Label(), settings.PgMaxConns)
	}

	// 5. 初始化账号管理器
	store := auth.NewStore(db, tc, settings)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := store.Init(ctx); err != nil {
		cancel()
		log.Fatalf("账号初始化失败: %v", err)
	}
	cancel()

	// 全局 RPM 限流器
	rateLimiter := proxy.NewRateLimiter(settings.GlobalRPM)
	adminHandler := admin.NewHandler(store, db, tc, rateLimiter, cfg.AdminSecret)
	// 初始化 admin handler 的连接池设置跟踪
	adminHandler.SetPoolSizes(settings.PgMaxConns, settings.RedisPoolSize)
	store.SetUsageProbeFunc(adminHandler.ProbeUsageSnapshot)
	store.SetImageQuotaProbeFunc(adminHandler.ProbeImageQuotaSnapshot)

	// 启动后台刷新
	store.StartBackgroundRefresh()
	store.TriggerUsageProbeAsync()
	store.TriggerImageQuotaProbeAsync()
	store.TriggerRecoveryProbeAsync()
	store.TriggerAutoCleanupAsync()
	defer store.Stop()

	log.Printf("账号就绪: %d/%d 可用", store.AvailableCount(), store.AccountCount())

	// 6. 启动 HTTP 服务
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(api.RecoveryMiddleware())
	r.Use(api.RequestContextMiddleware())
	r.Use(api.VersionMiddleware())
	security.MaxRequestBodySize = cfg.MaxRequestBodySize
	r.Use(security.RequestSizeLimiter(int64(security.MaxRequestBodySize)))
	r.Use(api.BodyCacheMiddleware())
	r.Use(api.CORSMiddleware())
	r.Use(api.SecurityHeadersMiddleware())
	r.Use(loggerMiddleware())
	r.Use(security.SecurityHeadersMiddleware())

	// handler 不再接收 cfg.APIKeys
	// 从环境变量读取 Codex 画像与 Beta 配置，并以稳定默认值为基线。
	deviceCfg := proxy.DeviceProfileConfigFromEnv(os.Getenv)
	handler := proxy.NewHandler(store, db, cfg, deviceCfg)

	// 注册 WebSocket 执行函数（避免 proxy ↔ wsrelay 循环依赖）
	proxy.WebsocketExecuteFunc = wsrelay.ExecuteRequestWebsocket

	r.Use(rateLimiter.Middleware())
	if settings.GlobalRPM > 0 {
		log.Printf("全局限流已生效: %d RPM", settings.GlobalRPM)
	}
	log.Printf("单账号并发上限: %d", settings.MaxConcurrency)

	handler.RegisterRoutes(r)
	adminHandler.RegisterRoutes(r)
	adminHandler.RegisterCliproxyRoutes(r)
	adminHandler.RegisterPublicRoutes(r)

	// 管理后台前端静态文件
	subFS, err := fs.Sub(frontendFS, "frontend/dist")
	if err != nil {
		log.Printf("前端静态文件加载失败（开发模式可忽略）: %v", err)
	} else {
		httpFS := http.FS(subFS)
		// 预读 index.html（SPA 回退时直接返回，避免 FileServer 重定向）
		indexHTML, _ := fs.ReadFile(subFS, "index.html")

		serveAdmin := func(c *gin.Context) {
			fp := c.Param("filepath")
			// 尝试打开请求的文件（排除目录和根路径）
			if fp != "/" && len(fp) > 1 {
				trimmed := fp[1:] // 去掉开头的 /
				if f, err := subFS.Open(trimmed); err == nil {
					fi, statErr := f.Stat()
					f.Close()
					if statErr == nil && !fi.IsDir() {
						c.FileFromFS(fp, httpFS)
						return
					}
				}
			}
			// 文件不存在或者是目录 → 直接返回 index.html 字节（让 React Router 处理）
			c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
		}

		// 同时处理 /admin 和 /admin/*，避免依赖自动补斜杠重定向。
		r.GET("/admin", serveAdmin)
		r.GET("/admin/*filepath", serveAdmin)
	}

	// 根路径重定向到管理后台（使用 302 避免浏览器永久缓存）
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/admin/")
	})

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":    "ok",
			"available": store.AvailableCount(),
			"total":     store.AccountCount(),
		})
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	plusAddr := fmt.Sprintf(":%d", cfg.Port+1)
	plusPortEnabled := store.GetPlusPortEnabled()
	log.Println("==========================================")
	log.Printf("  Codex2API v2 已启动")
	log.Printf("  HTTP:   http://0.0.0.0%s", addr)
	log.Printf("  管理台: http://0.0.0.0%s/admin/", addr)
	if plusPortEnabled {
		log.Printf("  Plus端口: http://0.0.0.0%s (访问策略: 全套餐%s)", plusAddr, func() string {
			if store.GetPlusPortAccessFree() {
				return "+free"
			}
			return "，不含free"
		}())
		log.Printf("  主端口策略: 仅 free 套餐")
	}
	log.Printf("  API:    POST /v1/chat/completions")
	log.Printf("  API:    POST /v1/responses")
	log.Printf("  API:    POST /v1/responses/compact (compat)")
	log.Printf("  API:    POST /v1/images/generations")
	log.Printf("  API:    POST /v1/images/edits")
	log.Printf("  API:    POST /v1/messages")
	log.Printf("  API:    GET  /v1/models")
	log.Printf("  Compat: POST /chat/completions, /responses, /responses/compact, /images/generations, /images/edits, /messages")
	log.Printf("  Compat: GET  /models")
	log.Println("==========================================")

	mainSrv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	var plusSrv *http.Server
	if plusPortEnabled {
		plusSrv = &http.Server{
			Addr:    plusAddr,
			Handler: r,
		}
	}

	// 优雅关闭
	go func() {
		if err := mainSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()
	if plusSrv != nil {
		go func() {
			if err := plusSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("Plus 端口服务启动失败: %v", err)
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("正在关闭...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := mainSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("主 HTTP 服务关闭异常: %v", err)
	}
	if plusSrv != nil {
		if err := plusSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("Plus 端口服务关闭异常: %v", err)
		}
	}
	store.Stop()
	wsrelay.ShutdownExecutor()
	proxy.CloseErrorLogger()
	log.Println("已关闭")
}

// loggerMiddleware 简单日志中间件（增强版，支持敏感信息脱敏）
func loggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Set("x-request-start-time", start)
		c.Next()
		latency := time.Since(start)

		email, _ := c.Get("x-account-email")
		proxyURL, _ := c.Get("x-account-proxy")
		modelVal, _ := c.Get("x-model")
		effortVal, _ := c.Get("x-reasoning-effort")
		tierVal, _ := c.Get("x-service-tier")
		attemptsVal, _ := c.Get("x-upstream-attempts")
		failedAttemptsVal, _ := c.Get("x-upstream-failed-attempts")
		requestIDVal, _ := c.Get("x-request-id")
		schedulerAcquireVal, _ := c.Get("x-scheduler-acquire-ms")
		schedulerAttemptVal, _ := c.Get("x-scheduler-attempt-ms")
		schedulerWaitRoundsVal, _ := c.Get("x-scheduler-wait-rounds")
		upstreamStageVal, _ := c.Get("x-upstream-stage-ms")
		upstreamHeaderVal, _ := c.Get("x-upstream-header-ms")
		upstreamFrameVal, _ := c.Get("x-upstream-first-frame-ms")
		upstreamFirstByteVal, _ := c.Get("x-upstream-first-byte-ms")
		upstreamAttemptVal, _ := c.Get("x-upstream-attempt-total-ms")
		upstreamConnectVal, _ := c.Get("x-upstream-connect-ms")
		upstreamReusedVal, _ := c.Get("x-upstream-reused-conn")
		firstTokenVal, _ := c.Get("x-first-token-ms")

		emailStr := ""
		if e, ok := email.(string); ok && e != "" {
			// 脱敏邮箱
			emailStr = security.MaskEmail(e)
		}
		proxyStr := "no proxy"
		if p, ok := proxyURL.(string); ok && p != "" {
			proxyStr = security.SanitizeLog(p)
		}

		// 构建扩展标签
		var tags []string
		if m, ok := modelVal.(string); ok && m != "" {
			tags = append(tags, security.SanitizeLog(m))
		}
		if rid, ok := requestIDVal.(string); ok && rid != "" {
			tags = append(tags, "rid="+security.SanitizeLog(rid))
		}
		if e, ok := effortVal.(string); ok && e != "" {
			tags = append(tags, "effort="+security.SanitizeLog(e))
		}
		if t, ok := tierVal.(string); ok && t == "fast" {
			tags = append(tags, "fast")
		}
		if attempts, ok := attemptsVal.(int); ok && attempts > 1 {
			tags = append(tags, fmt.Sprintf("attempts=%d", attempts))
		}
		if failed, ok := failedAttemptsVal.(string); ok && failed != "" {
			tags = append(tags, "failed="+security.SanitizeLog(failed))
		}
		if acquireMs, ok := schedulerAcquireVal.(int64); ok && acquireMs >= 20 {
			tags = append(tags, fmt.Sprintf("sched=%dms", acquireMs))
		}
		if attemptPickMs, ok := schedulerAttemptVal.(int); ok && attemptPickMs >= 20 {
			tags = append(tags, fmt.Sprintf("pick=%dms", attemptPickMs))
		}
		if waitRounds, ok := schedulerWaitRoundsVal.(int); ok && waitRounds > 0 {
			tags = append(tags, fmt.Sprintf("wait=%d", waitRounds))
		}
		if upstreamMs, ok := upstreamStageVal.(int64); ok && upstreamMs > 0 {
			tags = append(tags, fmt.Sprintf("forward=%dms", upstreamMs))
		}
		if headerMs, ok := upstreamHeaderVal.(int); ok && headerMs > 0 {
			tags = append(tags, fmt.Sprintf("header=%dms", headerMs))
		}
		if frameMs, ok := upstreamFrameVal.(int); ok && frameMs > 0 {
			tags = append(tags, fmt.Sprintf("frame=%dms", frameMs))
		}
		if firstByteMs, ok := upstreamFirstByteVal.(int); ok && firstByteMs > 0 {
			tags = append(tags, fmt.Sprintf("ttfb=%dms", firstByteMs))
		}
		if connectMs, ok := upstreamConnectVal.(int); ok && connectMs > 0 {
			tags = append(tags, fmt.Sprintf("connect=%dms", connectMs))
		}
		if attemptMs, ok := upstreamAttemptVal.(int); ok && attemptMs > 0 {
			tags = append(tags, fmt.Sprintf("attempt=%dms", attemptMs))
		}
		if firstMs, ok := firstTokenVal.(int); ok && firstMs > 0 {
			tags = append(tags, fmt.Sprintf("first=%dms", firstMs))
		}
		if reused, ok := upstreamReusedVal.(bool); ok && reused {
			tags = append(tags, "reused")
		}
		tagStr := ""
		if len(tags) > 0 {
			tagStr = " " + strings.Join(tags, " ")
		}

		if emailStr != "" {
			log.Printf("%s %s %d %v%s [%s] [%s]", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), latency, tagStr, emailStr, proxyStr)
		} else {
			log.Printf("%s %s %d %v%s", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), latency, tagStr)
		}
	}
}
