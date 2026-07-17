package market

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestExtractManifestRejectsUnsafeArchiveLayouts(t *testing.T) {
	manifest, err := json.Marshal(manifestJSON{ID: "safe-plugin", Name: "Safe", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		entries []string
	}{
		{name: "nested", entries: []string{"nested/plugin.json"}},
		{name: "duplicate", entries: []string{"plugin.json", "plugin.json"}},
		{name: "backslash", entries: []string{`folder\plugin.json`, "plugin.json"}},
		{name: "dot segment", entries: []string{"./plugin.json"}},
		{name: "parent segment", entries: []string{"folder/../plugin.json"}},
		{name: "windows absolute", entries: []string{"C:/file.txt", "plugin.json"}},
		{name: "colon", entries: []string{"folder/name:stream", "plugin.json"}},
		{name: "trailing dot", entries: []string{"folder./file.txt", "plugin.json"}},
		{name: "trailing space", entries: []string{"folder /file.txt", "plugin.json"}},
		{name: "windows reserved", entries: []string{"assets/CON.txt", "plugin.json"}},
		{name: "windows reserved numbered", entries: []string{"assets/lpt9.json", "plugin.json"}},
		{name: "case insensitive duplicate", entries: []string{"plugin.json", "PLUGIN.JSON"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugin.zip")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			for _, name := range tc.entries {
				entry, err := writer.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := entry.Write(manifest); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, _, err := extractManifest(path); err == nil {
				t.Fatal("extractManifest accepted unsafe archive layout")
			}
		})
	}
}

func TestExtractManifestRejectsNonRegularEntries(t *testing.T) {
	manifest := []byte(`{"id":"safe-plugin","name":"Safe","version":"1.0.0"}`)
	for _, mode := range []os.FileMode{os.ModeSymlink | 0o777, os.ModeNamedPipe | 0o644} {
		path := filepath.Join(t.TempDir(), "plugin.zip")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		writer := zip.NewWriter(file)
		manifestEntry, err := writer.Create("plugin.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manifestEntry.Write(manifest); err != nil {
			t.Fatal(err)
		}
		header := &zip.FileHeader{Name: "unsafe"}
		header.SetMode(mode)
		if _, err := writer.CreateHeader(header); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := extractManifest(path); err == nil {
			t.Fatalf("extractManifest accepted mode %v", mode)
		}
	}
}

func TestExtractManifestEnforcesArchiveLimits(t *testing.T) {
	manifest := []byte(`{"id":"safe-plugin","name":"Safe","version":"1.0.0"}`)
	for _, tc := range []struct {
		name  string
		build func(*testing.T, *zip.Writer)
	}{
		{
			name: "entry count",
			build: func(t *testing.T, writer *zip.Writer) {
				for i := 0; i < pluginArchiveMaxEntries; i++ {
					if _, err := writer.Create(fmt.Sprintf("entry-%04d", i)); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "single entry",
			build: func(t *testing.T, writer *zip.Writer) {
				entry, err := writer.Create("large.bin")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := entry.Write(bytes.Repeat([]byte("x"), int(pluginArchiveMaxEntryBytes+1))); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "total size",
			build: func(t *testing.T, writer *zip.Writer) {
				chunk := strings.Repeat("x", int(pluginArchiveMaxEntryBytes))
				for i := 0; i < 5; i++ {
					entry, err := writer.Create(fmt.Sprintf("large-%d.bin", i))
					if err != nil {
						t.Fatal(err)
					}
					size := pluginArchiveMaxEntryBytes
					if i == 4 {
						size = 1
					}
					if _, err := io.CopyN(entry, strings.NewReader(chunk), size); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plugin.zip")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			writer := zip.NewWriter(file)
			entry, err := writer.Create("plugin.json")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write(manifest); err != nil {
				t.Fatal(err)
			}
			tc.build(t, writer)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, _, err := extractManifest(path); err == nil {
				t.Fatal("extractManifest accepted archive over limit")
			}
		})
	}
}

func TestSemVerValidationAndOrdering(t *testing.T) {
	for _, tc := range []struct {
		version string
		valid   bool
	}{
		{version: "1.0.0", valid: true},
		{version: "1.0.0-beta.1+build.2", valid: true},
		{version: "1", valid: false},
		{version: "v1.0.0", valid: false},
		{version: "1.0.0_bad", valid: false},
	} {
		if got := isValidSemVer(tc.version); got != tc.valid {
			t.Fatalf("isValidSemVer(%q) = %v, want %v", tc.version, got, tc.valid)
		}
	}
}

func TestCheckUpdatesUsesSemVerOrdering(t *testing.T) {
	tests := []struct {
		name      string
		installed string
		market    string
		want      int
	}{
		{name: "numeric comparison", installed: "1.9.0", market: "1.10.0", want: 1},
		{name: "equal", installed: "1.0.0", market: "1.0.0", want: 0},
		{name: "installed newer", installed: "2.0.0", market: "1.0.0", want: 0},
		{name: "release beats prerelease", installed: "1.0.0-beta.1", market: "1.0.0", want: 1},
		{name: "loose installed", installed: "v1.0", market: "2.0.0", want: 1},
		{name: "invalid installed updated", installed: "development", market: "2.0.0", want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&database.Plugin{}, &database.PluginPermission{}, &database.PluginScreenshot{}); err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&database.Plugin{
				ID: "semver-plugin", Name: "SemVer", Version: tc.market,
				ZipPath: "unused", Status: database.PluginStatusApproved,
			}).Error; err != nil {
				t.Fatal(err)
			}
			updates, err := NewService(db, nil).CheckUpdates([]InstalledItem{{
				ID: "semver-plugin", Version: tc.installed,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(updates) != tc.want {
				t.Fatalf("updates len = %d, want %d", len(updates), tc.want)
			}
		})
	}
}

func TestMarketIconURLOnlyAllowsAbsoluteHTTPURLs(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "icons/plugin.png", want: false},
		{value: "javascript:alert(1)", want: false},
		{value: "https://cdn.example/plugin.png", want: true},
		{value: "http://cdn.example/plugin.png", want: true},
	} {
		if got := marketIconURL(tc.value) != nil; got != tc.want {
			t.Fatalf("marketIconURL(%q) present = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestDownloadPluginOnlyCountsExistingFiles(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	plugin := database.Plugin{
		ID: "missing-file", Name: "Missing", Version: "1.0.0",
		ZipPath: "missing.zip", Status: database.PluginStatusApproved,
	}
	if err := db.Create(&plugin).Error; err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/market/plugins/:id/download", NewHandler(NewService(db, storage)).DownloadPlugin)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/market/plugins/missing-file/download", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	var stored database.Plugin
	if err := db.First(&stored, "id = ?", plugin.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DownloadCount != 0 {
		t.Fatalf("download count = %d, want 0", stored.DownloadCount)
	}
}

func TestDownloadPluginSetsZipHeadersAndCountsSynchronously(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	sha := "abc123"
	plugin := database.Plugin{
		ID: "download-file", Name: "Download", Version: "1.0.0",
		ZipPath: "download.zip", SHA256: &sha, Status: database.PluginStatusApproved,
	}
	if err := os.WriteFile(storage.FullPath(plugin.ZipPath), []byte("zip contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&plugin).Error; err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/market/plugins/:id/download", NewHandler(NewService(db, storage)).DownloadPlugin)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/market/plugins/download-file/download", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", got)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Header().Get("ETag"); got != `"abc123"` {
		t.Fatalf("ETag = %q, want quoted SHA", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); got != `attachment; filename="download-file-1.0.0.zip"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	var stored database.Plugin
	if err := db.First(&stored, "id = ?", plugin.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DownloadCount != 1 {
		t.Fatalf("download count = %d, want 1", stored.DownloadCount)
	}
}

func TestDownloadPluginCountsPartialButNotNotModified(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	sha := "abc123"
	plugin := database.Plugin{ID: "conditional", Name: "Conditional", Version: "1.0.0", ZipPath: "conditional.zip", SHA256: &sha, Status: database.PluginStatusApproved}
	if err := os.WriteFile(storage.FullPath(plugin.ZipPath), []byte("zip contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&plugin).Error; err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/market/plugins/:id/download", NewHandler(NewService(db, storage)).DownloadPlugin)

	partial := httptest.NewRecorder()
	partialRequest := httptest.NewRequest(http.MethodGet, "/market/plugins/conditional/download", nil)
	partialRequest.Header.Set("Range", "bytes=0-2")
	router.ServeHTTP(partial, partialRequest)
	if partial.Code != http.StatusPartialContent {
		t.Fatalf("partial status = %d, want 206", partial.Code)
	}

	notModified := httptest.NewRecorder()
	notModifiedRequest := httptest.NewRequest(http.MethodGet, "/market/plugins/conditional/download", nil)
	notModifiedRequest.Header.Set("If-None-Match", `"abc123"`)
	router.ServeHTTP(notModified, notModifiedRequest)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", notModified.Code)
	}

	var stored database.Plugin
	if err := db.First(&stored, "id = ?", plugin.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.DownloadCount != 1 {
		t.Fatalf("download count = %d, want only the 206 counted", stored.DownloadCount)
	}
}

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
	manifest.Version = "2.0.0"

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

func TestUpsertSubmissionRejectsDifferentArchiveForSameVersion(t *testing.T) {
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
	firstSHA := "first-sha"
	first, err := service.UpsertSubmission(manifest, stageTestZip(t, storage, []byte("first")), &firstSHA, 101)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := first.ZipPath
	secondSHA := "second-sha"
	if _, err := service.UpsertSubmission(manifest, stageTestZip(t, storage, []byte("second")), &secondSHA, 101); !errors.Is(err, ErrPluginVersionConflict) {
		t.Fatalf("same-version error = %v, want ErrPluginVersionConflict", err)
	}
	contents, err := os.ReadFile(storage.FullPath(firstPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, []byte("first")) {
		t.Fatalf("original ZIP contents = %q, want first", contents)
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
