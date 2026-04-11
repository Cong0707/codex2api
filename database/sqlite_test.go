package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestNewSQLiteInitializesFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	if got := db.Driver(); got != "sqlite" {
		t.Fatalf("Driver() = %q, want %q", got, "sqlite")
	}
}

func TestGetAllRefreshTokensSkipsDeletedAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	activeID, err := db.InsertAccount(ctx, "active", "rt-active", "")
	if err != nil {
		t.Fatalf("InsertAccount(active) 返回错误: %v", err)
	}
	deletedID, err := db.InsertAccount(ctx, "deleted", "rt-deleted", "")
	if err != nil {
		t.Fatalf("InsertAccount(deleted) 返回错误: %v", err)
	}
	if err := db.SetError(ctx, deletedID, "deleted"); err != nil {
		t.Fatalf("SetError(deleted) 返回错误: %v", err)
	}

	tokens, err := db.GetAllRefreshTokens(ctx)
	if err != nil {
		t.Fatalf("GetAllRefreshTokens 返回错误: %v", err)
	}

	if !tokens["rt-active"] {
		t.Fatalf("active token 未返回, activeID=%d", activeID)
	}
	if tokens["rt-deleted"] {
		t.Fatalf("deleted token 不应参与去重, deletedID=%d", deletedID)
	}
}

func TestGetAllAccessTokensSkipsDeletedAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	activeID, err := db.InsertATAccount(ctx, "active-at", "at-active", "")
	if err != nil {
		t.Fatalf("InsertATAccount(active) 返回错误: %v", err)
	}
	deletedID, err := db.InsertATAccount(ctx, "deleted-at", "at-deleted", "")
	if err != nil {
		t.Fatalf("InsertATAccount(deleted) 返回错误: %v", err)
	}
	if err := db.SetError(ctx, deletedID, "deleted"); err != nil {
		t.Fatalf("SetError(deleted) 返回错误: %v", err)
	}

	tokens, err := db.GetAllAccessTokens(ctx)
	if err != nil {
		t.Fatalf("GetAllAccessTokens 返回错误: %v", err)
	}

	if !tokens["at-active"] {
		t.Fatalf("active access token 未返回, activeID=%d", activeID)
	}
	if tokens["at-deleted"] {
		t.Fatalf("deleted access token 不应参与去重, deletedID=%d", deletedID)
	}
}
