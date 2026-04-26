package proxy

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
)

func newTestModelRegistryDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestParseOfficialCodexModelIDs(t *testing.T) {
	html := `
		<astro-island props="{&quot;name&quot;:[0,&quot;gpt-5.5&quot;]}"></astro-island>
		<code>codex -m gpt-5.4</code>
		<code>codex -m gpt-5.3-codex-spark</code>
		<code>codex -m gpt-5.2</code>
		<code>codex -m gpt-5.2-codex</code>
		<code>codex -m gpt-4.1</code>
	`
	models, skipped := ParseOfficialCodexModelIDs(html)
	for _, model := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.3-codex-spark", "gpt-5.2"} {
		if !slices.Contains(models, model) {
			t.Fatalf("parsed models missing %q in %v", model, models)
		}
	}
	for _, model := range []string{"gpt-5.2-codex", "gpt-4.1"} {
		if !slices.Contains(skipped, model) {
			t.Fatalf("skipped models missing %q in %v", model, skipped)
		}
	}
}

func TestApplyOfficialCodexModelSyncMergesWithBuiltinImageModel(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	html := `gpt-5.5 gpt-5.4 gpt-5.4-mini gpt-5.3-codex gpt-5.3-codex-spark gpt-5.2 gpt-5.2-codex gpt-4.1`

	result, err := ApplyOfficialCodexModelSync(ctx, db, html, time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ApplyOfficialCodexModelSync error: %v", err)
	}
	if !slices.Contains(result.Models, "gpt-image-2") {
		t.Fatalf("sync should keep builtin image model, got %v", result.Models)
	}
	if !slices.Contains(result.Skipped, "gpt-5.2-codex") {
		t.Fatalf("sync should skip gpt-5.2-codex, got %v", result.Skipped)
	}

	var spark *ModelInfo
	for i := range result.Items {
		if result.Items[i].ID == "gpt-5.3-codex-spark" {
			spark = &result.Items[i]
			break
		}
	}
	if spark == nil || !spark.ProOnly {
		t.Fatalf("spark model should be marked pro_only, got %#v", spark)
	}
}

func TestDynamicModelRegistryAffectsValidationImmediately(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	err := db.UpsertModelRegistryRows(ctx, []database.ModelRegistryRow{
		{
			ID:                  "gpt-6.0",
			Enabled:             true,
			Category:            ModelCategoryCodex,
			Source:              ModelSourceOfficialCodexDocs,
			APIKeyAuthAvailable: true,
		},
	})
	if err != nil {
		t.Fatalf("UpsertModelRegistryRows error: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	models := handler.supportedModelIDs(ctx)
	if !slices.Contains(models, "gpt-6.0") {
		t.Fatalf("runtime supported models missing synced model: %v", models)
	}

	result := api.ValidateResponsesAPIRequest([]byte(`{"model":"gpt-6.0","input":"hello"}`), models)
	if !result.Valid {
		t.Fatalf("synced model should pass validation: %#v", result.Errors)
	}
}
