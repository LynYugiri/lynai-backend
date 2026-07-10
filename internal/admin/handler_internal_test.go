package admin

import (
	"testing"

	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/relay"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestReplaceRelayModelsClearsLegacyModels(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := db.AutoMigrate(&database.RelayProvider{}, &database.RelayModel{}); err != nil {
		t.Fatalf("migrate database: %v", err)
	}
	provider := database.RelayProvider{
		Name:      "legacy provider",
		Endpoint:  "https://example.com/v1",
		APIKey:    "secret",
		APIFormat: relay.APIFormatOpenAI,
		Models:    `["legacy-model"]`,
		Enabled:   true,
	}
	if err := db.Create(&provider).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		return replaceRelayModelsTx(tx, provider.ID, nil)
	}); err != nil {
		t.Fatalf("replace models: %v", err)
	}
	if err := db.First(&provider, "id = ?", provider.ID).Error; err != nil {
		t.Fatalf("reload provider: %v", err)
	}
	if provider.Models != "" {
		t.Fatalf("legacy models = %q, want empty", provider.Models)
	}

	resolved, err := relay.NewService(db).Resolve(relay.APIFormatOpenAI, "legacy-model")
	if err == nil || resolved != nil {
		t.Fatalf("legacy model resolved after deletion: resolved=%#v err=%v", resolved, err)
	}
}
