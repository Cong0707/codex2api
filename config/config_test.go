package config

import "testing"

func TestLoadDefaultsToPostgresAndRedis(t *testing.T) {
	keys := []string{
		"CODEX_PORT",
		"CODEX_MAX_REQUEST_BODY_SIZE_MB",
		"PORT",
		"ADMIN_SECRET",
		"DATABASE_DRIVER",
		"DATABASE_PATH",
		"DATABASE_HOST",
		"DATABASE_PORT",
		"DATABASE_USER",
		"DATABASE_PASSWORD",
		"DATABASE_NAME",
		"DATABASE_SSLMODE",
		"CACHE_DRIVER",
		"REDIS_ADDR",
		"REDIS_USERNAME",
		"REDIS_PASSWORD",
		"REDIS_DB",
		"REDIS_TLS",
		"REDIS_INSECURE_SKIP_VERIFY",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}

	// 不设置 DATABASE_DRIVER / CACHE_DRIVER，只提供各自默认驱动所需的最小参数。
	t.Setenv("DATABASE_HOST", "postgres")
	t.Setenv("REDIS_ADDR", "redis:6379")

	cfg, err := Load("__not_exists__.env")
	if err != nil {
		t.Fatalf("Load() 返回错误: %v", err)
	}

	if got := cfg.Database.Driver; got != "postgres" {
		t.Fatalf("Database.Driver = %q, want %q", got, "postgres")
	}
	if got := cfg.Cache.Driver; got != "redis" {
		t.Fatalf("Cache.Driver = %q, want %q", got, "redis")
	}
	if got := cfg.Database.Port; got != 5432 {
		t.Fatalf("Database.Port = %d, want %d", got, 5432)
	}
	if got := cfg.Database.SSLMode; got != "disable" {
		t.Fatalf("Database.SSLMode = %q, want %q", got, "disable")
	}
	if got := cfg.Port; got != 8080 {
		t.Fatalf("Port = %d, want %d", got, 8080)
	}
	if got := cfg.MaxRequestBodySize; got != 32*1024*1024 {
		t.Fatalf("MaxRequestBodySize = %d, want %d", got, 32*1024*1024)
	}
}

func TestLoadAllowsExplicitSQLiteAndMemory(t *testing.T) {
	keys := []string{
		"CODEX_PORT",
		"CODEX_MAX_REQUEST_BODY_SIZE_MB",
		"PORT",
		"ADMIN_SECRET",
		"DATABASE_DRIVER",
		"DATABASE_PATH",
		"DATABASE_HOST",
		"DATABASE_PORT",
		"DATABASE_USER",
		"DATABASE_PASSWORD",
		"DATABASE_NAME",
		"DATABASE_SSLMODE",
		"CACHE_DRIVER",
		"REDIS_ADDR",
		"REDIS_USERNAME",
		"REDIS_PASSWORD",
		"REDIS_DB",
		"REDIS_TLS",
		"REDIS_INSECURE_SKIP_VERIFY",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}

	t.Setenv("DATABASE_DRIVER", "sqlite")
	t.Setenv("DATABASE_PATH", "/data/codex2api.db")
	t.Setenv("CACHE_DRIVER", "memory")

	cfg, err := Load("__not_exists__.env")
	if err != nil {
		t.Fatalf("Load() 返回错误: %v", err)
	}

	if got := cfg.Database.Driver; got != "sqlite" {
		t.Fatalf("Database.Driver = %q, want %q", got, "sqlite")
	}
	if got := cfg.Database.Path; got != "/data/codex2api.db" {
		t.Fatalf("Database.Path = %q, want %q", got, "/data/codex2api.db")
	}
	if got := cfg.Cache.Driver; got != "memory" {
		t.Fatalf("Cache.Driver = %q, want %q", got, "memory")
	}
}

func TestLoadReadsAdminSecretFromEnv(t *testing.T) {
	keys := []string{
		"CODEX_PORT",
		"CODEX_MAX_REQUEST_BODY_SIZE_MB",
		"PORT",
		"ADMIN_SECRET",
		"DATABASE_DRIVER",
		"DATABASE_PATH",
		"DATABASE_HOST",
		"DATABASE_PORT",
		"DATABASE_USER",
		"DATABASE_PASSWORD",
		"DATABASE_NAME",
		"DATABASE_SSLMODE",
		"CACHE_DRIVER",
		"REDIS_ADDR",
		"REDIS_USERNAME",
		"REDIS_PASSWORD",
		"REDIS_DB",
		"REDIS_TLS",
		"REDIS_INSECURE_SKIP_VERIFY",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}

	t.Setenv("DATABASE_HOST", "postgres")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("ADMIN_SECRET", "from-env-secret")

	cfg, err := Load("__not_exists__.env")
	if err != nil {
		t.Fatalf("Load() 返回错误: %v", err)
	}

	if got := cfg.AdminSecret; got != "from-env-secret" {
		t.Fatalf("AdminSecret = %q, want %q", got, "from-env-secret")
	}
}

func TestLoadReadsMaxRequestBodySizeFromEnv(t *testing.T) {
	keys := []string{
		"CODEX_PORT",
		"CODEX_MAX_REQUEST_BODY_SIZE_MB",
		"PORT",
		"ADMIN_SECRET",
		"DATABASE_DRIVER",
		"DATABASE_PATH",
		"DATABASE_HOST",
		"DATABASE_PORT",
		"DATABASE_USER",
		"DATABASE_PASSWORD",
		"DATABASE_NAME",
		"DATABASE_SSLMODE",
		"CACHE_DRIVER",
		"REDIS_ADDR",
		"REDIS_USERNAME",
		"REDIS_PASSWORD",
		"REDIS_DB",
		"REDIS_TLS",
		"REDIS_INSECURE_SKIP_VERIFY",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}

	t.Setenv("DATABASE_HOST", "postgres")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("CODEX_MAX_REQUEST_BODY_SIZE_MB", "64")

	cfg, err := Load("__not_exists__.env")
	if err != nil {
		t.Fatalf("Load() 返回错误: %v", err)
	}

	if got := cfg.MaxRequestBodySize; got != 64*1024*1024 {
		t.Fatalf("MaxRequestBodySize = %d, want %d", got, 64*1024*1024)
	}
}

func TestLoadReadsRedisTLSSettings(t *testing.T) {
	keys := []string{
		"CODEX_PORT",
		"CODEX_MAX_REQUEST_BODY_SIZE_MB",
		"PORT",
		"ADMIN_SECRET",
		"DATABASE_DRIVER",
		"DATABASE_PATH",
		"DATABASE_HOST",
		"DATABASE_PORT",
		"DATABASE_USER",
		"DATABASE_PASSWORD",
		"DATABASE_NAME",
		"DATABASE_SSLMODE",
		"CACHE_DRIVER",
		"REDIS_ADDR",
		"REDIS_USERNAME",
		"REDIS_PASSWORD",
		"REDIS_DB",
		"REDIS_TLS",
		"REDIS_INSECURE_SKIP_VERIFY",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}

	t.Setenv("DATABASE_HOST", "postgres")
	t.Setenv("REDIS_ADDR", "rediss://default:url-pass@example.upstash.io:6379/2")
	t.Setenv("REDIS_USERNAME", "env-user")
	t.Setenv("REDIS_PASSWORD", "env-pass")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("REDIS_TLS", "true")
	t.Setenv("REDIS_INSECURE_SKIP_VERIFY", "1")

	cfg, err := Load("__not_exists__.env")
	if err != nil {
		t.Fatalf("Load() 返回错误: %v", err)
	}

	if got := cfg.Cache.Redis.Addr; got != "rediss://default:url-pass@example.upstash.io:6379/2" {
		t.Fatalf("Redis.Addr = %q, want rediss URL", got)
	}
	if got := cfg.Cache.Redis.Username; got != "env-user" {
		t.Fatalf("Redis.Username = %q, want env-user", got)
	}
	if got := cfg.Cache.Redis.Password; got != "env-pass" {
		t.Fatalf("Redis.Password = %q, want env-pass", got)
	}
	if got := cfg.Cache.Redis.DB; got != 3 {
		t.Fatalf("Redis.DB = %d, want 3", got)
	}
	if !cfg.Cache.Redis.TLS {
		t.Fatal("Redis.TLS = false, want true")
	}
	if !cfg.Cache.Redis.InsecureSkipVerify {
		t.Fatal("Redis.InsecureSkipVerify = false, want true")
	}
}
