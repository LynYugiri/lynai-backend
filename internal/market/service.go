package market

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrPluginNotFound is returned when a plugin ID does not exist.
var ErrPluginNotFound = errors.New("plugin not found")

// ErrPluginNotOwned is returned when another user owns a submitted plugin ID.
var ErrPluginNotOwned = errors.New("plugin belongs to another submitter")

// Service handles marketplace queries, submissions, and review operations.
type Service struct {
	db        *gorm.DB
	storage   *Storage
	storageMu sync.Mutex
}

// NewService creates a market service with the given database and storage.
func NewService(db *gorm.DB, storage *Storage) *Service {
	return &Service{db: db, storage: storage}
}

// ListPlugins returns approved plugins matching the given query parameters.
// Only approved plugins are visible to the public.
func (s *Service) ListPlugins(category, query string, page, pageSize int) ([]database.Plugin, int64, error) {
	if page < 1 {
		page = 1
	}
	pageSize = normalizePageSize(pageSize)

	tx := s.db.Model(&database.Plugin{}).Where("status = ?", database.PluginStatusApproved)
	if category != "" {
		tx = tx.Where("category = ?", category)
	}
	if query != "" {
		like := "%" + query + "%"
		tx = tx.Where("name ILIKE ? OR description ILIKE ? OR author ILIKE ?", like, like, like)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var plugins []database.Plugin
	err := tx.
		Preload("Screenshots").
		Preload("Permissions").
		Order("updated_at DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&plugins).Error
	if err != nil {
		return nil, 0, err
	}
	return plugins, total, nil
}

func normalizePageSize(pageSize int) int {
	if pageSize < 1 || pageSize > 100 {
		return 20
	}
	return pageSize
}

// GetPlugin returns a single approved plugin by ID.
func (s *Service) GetPlugin(id string) (*database.Plugin, error) {
	var plugin database.Plugin
	err := s.db.
		Preload("Screenshots").
		Preload("Permissions").
		Where("id = ? AND status = ?", id, database.PluginStatusApproved).
		First(&plugin).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPluginNotFound
		}
		return nil, err
	}
	return &plugin, nil
}

// GetPluginAnyStatus returns a plugin by ID regardless of status.
// Used for submitter and admin views.
func (s *Service) GetPluginAnyStatus(id string) (*database.Plugin, error) {
	var plugin database.Plugin
	err := s.db.
		Preload("Screenshots").
		Preload("Permissions").
		Where("id = ?", id).
		First(&plugin).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPluginNotFound
		}
		return nil, err
	}
	return &plugin, nil
}

// IncrementDownloadCount atomically bumps the download counter.
func (s *Service) IncrementDownloadCount(id string) {
	s.db.Model(&database.Plugin{}).Where("id = ?", id).
		UpdateColumn("download_count", gorm.Expr("download_count + 1"))
}

// ListPending returns plugins with pending status.
func (s *Service) ListPending() ([]database.Plugin, error) {
	var plugins []database.Plugin
	err := s.db.
		Preload("Screenshots").
		Preload("Permissions").
		Where("status = ?", database.PluginStatusPending).
		Order("created_at ASC").
		Find(&plugins).Error
	return plugins, err
}

// ListBySubmitter returns all plugins submitted by a given user.
func (s *Service) ListBySubmitter(userID int64) ([]database.Plugin, error) {
	var plugins []database.Plugin
	err := s.db.
		Preload("Screenshots").
		Preload("Permissions").
		Where("submitted_by = ?", userID).
		Order("updated_at DESC").
		Find(&plugins).Error
	return plugins, err
}

// Approve sets a plugin's status to approved.
func (s *Service) Approve(pluginID string, reviewerID int64) error {
	result := s.db.Model(&database.Plugin{}).
		Where("id = ? AND status = ?", pluginID, database.PluginStatusPending).
		Updates(map[string]interface{}{
			"status":      database.PluginStatusApproved,
			"reviewed_by": reviewerID,
			"review_note": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// Reject sets a plugin's status to rejected with an optional reason.
func (s *Service) Reject(pluginID string, reviewerID int64, reason string) error {
	result := s.db.Model(&database.Plugin{}).
		Where("id = ? AND status = ?", pluginID, database.PluginStatusPending).
		Updates(map[string]interface{}{
			"status":      database.PluginStatusRejected,
			"reviewed_by": reviewerID,
			"review_note": reason,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// UpdatePlugin updates editable plugin metadata from the admin panel.
func (s *Service) UpdatePlugin(pluginID, name, description, category, version string) error {
	updates := map[string]interface{}{
		"name":        name,
		"description": description,
		"category":    category,
		"version":     version,
	}
	result := s.db.Model(&database.Plugin{}).Where("id = ?", pluginID).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// Unpublish moves an approved plugin back to pending review.
func (s *Service) Unpublish(pluginID string) error {
	result := s.db.Model(&database.Plugin{}).
		Where("id = ?", pluginID).
		Updates(map[string]interface{}{
			"status":      database.PluginStatusPending,
			"reviewed_by": nil,
			"review_note": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPluginNotFound
	}
	return nil
}

// DeletePlugin deletes a plugin record and its stored ZIP file.
func (s *Service) DeletePlugin(pluginID string) error {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()

	var backupPath string
	var fullPath string
	var moved bool
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var plugin database.Plugin
		if err := tx.Where("id = ?", pluginID).First(&plugin).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPluginNotFound
			}
			return err
		}

		var err error
		fullPath = s.storage.FullPath(plugin.ZipPath)
		backupPath, moved, err = moveAside(fullPath)
		if err != nil {
			return fmt.Errorf("isolate plugin zip: %w", err)
		}
		if err := tx.Where("plugin_id = ?", pluginID).Delete(&database.PluginPermission{}).Error; err != nil {
			return err
		}
		if err := tx.Where("plugin_id = ?", pluginID).Delete(&database.PluginScreenshot{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&database.Plugin{}, "id = ?", pluginID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrPluginNotFound
		}
		return nil
	})
	if err != nil {
		if moved {
			_ = os.Rename(backupPath, fullPath)
		}
		return err
	}
	if moved {
		_ = os.Remove(backupPath)
	}
	return nil
}

// UpsertSubmission creates or updates a plugin record from a submitted manifest.
// If a plugin with the same ID exists, it resets status to pending and updates
// all metadata fields. Screenshots and permissions are fully replaced.
func (s *Service) UpsertSubmission(m *manifestJSON, tempPath string, sha *string, submitterID int64) (*database.Plugin, error) {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()

	zipPath, fullPath, err := s.storage.PluginZipPath(m.ID, m.Version)
	if err != nil {
		return nil, err
	}
	if err := s.storage.PublishPluginZip(tempPath, fullPath); err != nil {
		return nil, fmt.Errorf("publish plugin zip: %w", err)
	}
	if err := os.Chmod(fullPath, 0o644); err != nil {
		_ = os.Remove(fullPath)
		return nil, fmt.Errorf("set plugin zip permissions: %w", err)
	}

	var oldZipPath string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var existing database.Plugin
		queryErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", m.ID).First(&existing).Error
		if queryErr != nil && !errors.Is(queryErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("query existing plugin: %w", queryErr)
		}
		if queryErr == nil && existing.SubmittedBy != submitterID {
			return ErrPluginNotOwned
		}
		if queryErr == nil {
			oldZipPath = existing.ZipPath
		}

		if errors.Is(queryErr, gorm.ErrRecordNotFound) {
			plugin := database.Plugin{
				ID:          m.ID,
				Name:        m.Name,
				Author:      m.Author,
				Description: m.Description,
				Version:     m.Version,
				ZipPath:     zipPath,
				SHA256:      sha,
				Status:      database.PluginStatusPending,
				SubmittedBy: submitterID,
			}
			if m.Icon != "" {
				plugin.IconURL = &m.Icon
			}
			if err := tx.Create(&plugin).Error; err != nil {
				return fmt.Errorf("create plugin: %w", err)
			}
		} else {
			updates := map[string]interface{}{
				"name":        m.Name,
				"author":      m.Author,
				"description": m.Description,
				"version":     m.Version,
				"zip_path":    zipPath,
				"sha256":      sha,
				"status":      database.PluginStatusPending,
				"reviewed_by": nil,
				"review_note": nil,
			}
			if m.Icon != "" {
				updates["icon_url"] = m.Icon
			}
			if err := tx.Model(&existing).Updates(updates).Error; err != nil {
				return fmt.Errorf("update plugin: %w", err)
			}
		}
		return replacePermissions(tx, m.ID, m.Permissions)
	})
	if err != nil {
		_ = os.Remove(fullPath)
		return nil, err
	}
	if oldZipPath != "" && oldZipPath != zipPath {
		var references int64
		if err := s.db.Model(&database.Plugin{}).Where("zip_path = ?", oldZipPath).Count(&references).Error; err == nil && references == 0 {
			_ = s.storage.DeletePluginZip(oldZipPath)
		}
	}
	return s.reloadPlugin(m.ID)
}

// reloadPlugin loads a plugin with all relations preloaded.
func (s *Service) reloadPlugin(id string) (*database.Plugin, error) {
	var plugin database.Plugin
	if err := s.db.
		Preload("Screenshots").
		Preload("Permissions").
		Where("id = ?", id).
		First(&plugin).Error; err != nil {
		return nil, err
	}
	return &plugin, nil
}

// replacePermissions deletes all existing permissions for a plugin and inserts
// the new set.
func replacePermissions(tx *gorm.DB, pluginID string, perms []string) error {
	if err := tx.Where("plugin_id = ?", pluginID).Delete(&database.PluginPermission{}).Error; err != nil {
		return fmt.Errorf("clear permissions: %w", err)
	}
	for _, p := range perms {
		if err := tx.Create(&database.PluginPermission{
			PluginID:   pluginID,
			Permission: p,
		}).Error; err != nil {
			return fmt.Errorf("create permission: %w", err)
		}
	}
	return nil
}

// CheckUpdates compares installed plugin versions against approved market versions.
// Returns market entries for plugins where the installed version differs from
// the latest approved version.
type InstalledItem struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type UpdateEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Author      string   `json:"author"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	IconURL     *string  `json:"iconUrl"`
	Screenshots []string `json:"screenshots"`
	Permissions []string `json:"permissions"`
	DownloadURL string   `json:"downloadUrl"`
	SHA256      *string  `json:"sha256"`
	Category    string   `json:"category"`
	Status      string   `json:"status"`
	ReviewNote  *string  `json:"reviewNote"`
}

func (s *Service) CheckUpdates(items []InstalledItem) ([]UpdateEntry, error) {
	if len(items) == 0 {
		return []UpdateEntry{}, nil
	}

	ids := make([]string, len(items))
	installedVersions := make(map[string]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
		installedVersions[item.ID] = item.Version
	}

	var plugins []database.Plugin
	err := s.db.
		Preload("Screenshots").
		Preload("Permissions").
		Where("id IN ? AND status = ?", ids, database.PluginStatusApproved).
		Find(&plugins).Error
	if err != nil {
		return nil, err
	}

	var updates []UpdateEntry
	for _, p := range plugins {
		if p.Version != installedVersions[p.ID] {
			updates = append(updates, pluginToUpdateEntry(&p))
		}
	}
	if updates == nil {
		updates = []UpdateEntry{}
	}
	return updates, nil
}

// pluginToUpdateEntry converts a Plugin model to the update-check response format.
func pluginToUpdateEntry(p *database.Plugin) UpdateEntry {
	return UpdateEntry{
		ID:          p.ID,
		Name:        p.Name,
		Author:      p.Author,
		Description: p.Description,
		Version:     p.Version,
		IconURL:     p.IconURL,
		Screenshots: screenshotURLs(p.Screenshots),
		Permissions: permissionNames(p.Permissions),
		DownloadURL: fmt.Sprintf("/market/plugins/%s/download", p.ID),
		SHA256:      p.SHA256,
		Category:    p.Category,
		Status:      p.Status,
		ReviewNote:  p.ReviewNote,
	}
}

func screenshotURLs(shots []database.PluginScreenshot) []string {
	out := make([]string, 0, len(shots))
	for _, s := range shots {
		out = append(out, s.URL)
	}
	return out
}

func permissionNames(perms []database.PluginPermission) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, p.Permission)
	}
	return out
}
