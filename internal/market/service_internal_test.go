package market

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestNormalizePageSizeMatchesListAndHasMoreCalculation(t *testing.T) {
	for _, tc := range []struct {
		input int
		want  int
	}{{input: -1, want: 20}, {input: 0, want: 20}, {input: 1, want: 1}, {input: 100, want: 100}, {input: 101, want: 20}} {
		if got := normalizePageSize(tc.input); got != tc.want {
			t.Fatalf("normalizePageSize(%d) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestListPluginsHasMoreUsesEffectivePageSize(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.Plugin{}, &database.PluginPermission{}, &database.PluginScreenshot{}); err != nil {
		t.Fatal(err)
	}
	for i := range 21 {
		if err := db.Create(&database.Plugin{
			ID: "plugin-" + string(rune('a'+i)), Name: "Plugin", Author: "Author", Description: "Description",
			Version: "1.0.0", ZipPath: "unused", Status: database.PluginStatusApproved,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	handler := NewHandler(NewService(db, nil))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/market/plugins", handler.ListPlugins)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/market/plugins?page_size=101", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"hasMore":true`)) {
		t.Fatalf("response = %s, want hasMore true for effective page size 20", recorder.Body.String())
	}
}

func TestListPluginsUsesIDToOrderUpdatedAtTiesAcrossPages(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.Plugin{}, &database.PluginPermission{}, &database.PluginScreenshot{}); err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"plugin-a", "plugin-b", "plugin-c"} {
		if err := db.Create(&database.Plugin{
			ID: id, Name: id, Author: "Author", Description: "Description", Version: "1.0.0",
			ZipPath: "unused", Status: database.PluginStatusApproved, UpdatedAt: updatedAt,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	service := NewService(db, nil)
	firstPage, total, err := service.ListPlugins("", "", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	secondPage, _, err := service.ListPlugins("", "", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(firstPage) != 2 || firstPage[0].ID != "plugin-c" || firstPage[1].ID != "plugin-b" {
		t.Fatalf("first page IDs = %v, want [plugin-c plugin-b]", pluginIDs(firstPage))
	}
	if len(secondPage) != 1 || secondPage[0].ID != "plugin-a" {
		t.Fatalf("second page IDs = %v, want [plugin-a]", pluginIDs(secondPage))
	}
}

func pluginIDs(plugins []database.Plugin) []string {
	ids := make([]string, len(plugins))
	for i := range plugins {
		ids[i] = plugins[i].ID
	}
	return ids
}

func TestUpsertSubmissionDBFailurePreservesApprovedZip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.Plugin{}, &database.PluginPermission{}, &database.PluginScreenshot{}); err != nil {
		t.Fatal(err)
	}
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db, storage)
	manifest := &manifestJSON{
		ID:          "atomic-plugin",
		Name:        "Atomic Plugin",
		Author:      "owner",
		Description: "original",
		Version:     "1.0.0",
		Permissions: []string{"network"},
	}
	original := []byte("reviewed zip contents")
	originalSHA := "original-sha"
	originalTemp := stageTestZip(t, storage, original)
	plugin, err := service.UpsertSubmission(manifest, originalTemp, &originalSHA, 101)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(plugin).Update("status", database.PluginStatusApproved).Error; err != nil {
		t.Fatal(err)
	}
	originalPath := plugin.ZipPath
	originalFullPath := storage.FullPath(originalPath)

	replacement := []byte("unreviewed replacement contents")
	replacementSHA := "replacement-sha"
	replacementTemp := stageTestZip(t, storage, replacement)
	manifest.Description = "replacement"

	injectedErr := errors.New("injected update failure")
	var observedBeforeCommit bool
	callbackName := "market:test_fail_submission_update"
	if err := db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "plugins" {
			return
		}
		matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(originalFullPath), "*.zip"))
		if globErr != nil {
			tx.AddError(globErr)
			return
		}
		if len(matches) != 2 {
			tx.AddError(errors.New("old and new immutable ZIPs did not coexist before commit"))
			return
		}
		oldContents, readErr := os.ReadFile(originalFullPath)
		if readErr != nil {
			tx.AddError(readErr)
			return
		}
		if !bytes.Equal(oldContents, original) {
			tx.AddError(errors.New("approved ZIP changed before commit"))
			return
		}
		observedBeforeCommit = true
		tx.AddError(injectedErr)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.UpsertSubmission(manifest, replacementTemp, &replacementSHA, 101); !errors.Is(err, injectedErr) {
		t.Fatalf("UpsertSubmission error = %v, want %v", err, injectedErr)
	}
	if !observedBeforeCommit {
		t.Fatal("did not observe immutable files before the failed database update")
	}

	var stored database.Plugin
	if err := db.First(&stored, "id = ?", manifest.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ZipPath != originalPath {
		t.Fatalf("ZipPath = %q, want original %q", stored.ZipPath, originalPath)
	}
	if stored.Status != database.PluginStatusApproved {
		t.Fatalf("status = %q, want approved", stored.Status)
	}
	if stored.Description != "original" {
		t.Fatalf("description = %q, want original", stored.Description)
	}
	contents, err := os.ReadFile(originalFullPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, original) {
		t.Fatal("failed submission changed the approved ZIP")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(originalFullPath), "*.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0] != originalFullPath {
		t.Fatalf("ZIP files after rollback = %v, want only %q", matches, originalFullPath)
	}
}

func TestUpsertSubmissionUsesNewPathForSameVersion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.Plugin{}, &database.PluginPermission{}, &database.PluginScreenshot{}); err != nil {
		t.Fatal(err)
	}
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db, storage)
	manifest := &manifestJSON{ID: "same-version", Name: "Same Version", Version: "1.0.0"}
	first, err := service.UpsertSubmission(manifest, stageTestZip(t, storage, []byte("first")), nil, 101)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := first.ZipPath
	second, err := service.UpsertSubmission(manifest, stageTestZip(t, storage, []byte("second")), nil, 101)
	if err != nil {
		t.Fatal(err)
	}
	if second.ZipPath == firstPath {
		t.Fatalf("same-version resubmission reused path %q", firstPath)
	}
	if _, err := os.Stat(storage.FullPath(firstPath)); !os.IsNotExist(err) {
		t.Fatalf("old unreferenced ZIP stat error = %v, want not exist", err)
	}
	contents, err := os.ReadFile(storage.FullPath(second.ZipPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, []byte("second")) {
		t.Fatalf("new ZIP contents = %q, want second", contents)
	}
}

func TestPublishPluginZipDoesNotReplaceExistingFile(t *testing.T) {
	storage, err := NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, fullPath, err := storage.PluginZipPath("no-replace", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	tempPath := stageTestZip(t, storage, []byte("replacement"))
	if err := storage.PublishPluginZip(tempPath, fullPath); err == nil {
		t.Fatal("PublishPluginZip replaced an existing target")
	}
	contents, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, []byte("existing")) {
		t.Fatalf("existing ZIP contents = %q, want existing", contents)
	}
}

func stageTestZip(t *testing.T, storage *Storage, contents []byte) string {
	t.Helper()
	path, err := storage.StagePluginZip(bytes.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	return path
}
