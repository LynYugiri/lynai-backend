package database

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// User is the account model. Phone is the unique login identifier.
// DisplayName is the user-facing nickname, not unique, not used for login.
type User struct {
	ID            int64     `gorm:"primaryKey" json:"id,string"`
	Phone         string    `gorm:"uniqueIndex;not null" json:"phone"`
	PasswordHash  string    `gorm:"not null;default:''" json:"-"`
	DisplayName   string    `gorm:"not null" json:"displayName"`
	Email         *string   `json:"email"`
	PhoneVerified bool      `gorm:"default:true" json:"phoneVerified"`
	AvatarURL     *string   `json:"avatarUrl"`
	IsAdmin       bool      `gorm:"default:false" json:"isAdmin"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// PluginStatus enumerates the review lifecycle of a submitted plugin.
const (
	PluginStatusPending  = "pending"
	PluginStatusApproved = "approved"
	PluginStatusRejected = "rejected"
)

// Plugin is a marketplace plugin entry with review state.
type Plugin struct {
	ID            string             `gorm:"primaryKey" json:"id"`
	Name          string             `gorm:"not null" json:"name"`
	Author        string             `gorm:"not null" json:"author"`
	Description   string             `gorm:"not null" json:"description"`
	Version       string             `gorm:"not null" json:"version"`
	IconURL       *string            `json:"iconUrl"`
	Category      string             `json:"category"`
	ZipPath       string             `gorm:"not null" json:"-"`
	SHA256        *string            `json:"sha256"`
	DownloadCount int                `gorm:"default:0" json:"downloadCount"`
	Status        string             `gorm:"default:pending;index" json:"status"`
	SubmittedBy   int64              `gorm:"not null;index" json:"submittedBy,string"`
	ReviewedBy    *int64             `json:"reviewedBy,string"`
	ReviewNote    *string            `json:"reviewNote"`
	Screenshots   []PluginScreenshot `gorm:"foreignKey:PluginID" json:"screenshots"`
	Permissions   []PluginPermission `gorm:"foreignKey:PluginID" json:"permissions"`
	CreatedAt     time.Time          `json:"createdAt"`
	UpdatedAt     time.Time          `json:"updatedAt"`
}

// PluginScreenshot stores screenshot URLs for a plugin detail page.
type PluginScreenshot struct {
	ID        uint   `gorm:"primaryKey" json:"-"`
	PluginID  string `gorm:"index;not null" json:"-"`
	URL       string `gorm:"not null" json:"url"`
	SortOrder int    `gorm:"default:0" json:"-"`
}

// PluginPermission stores the declared permission list for a plugin.
type PluginPermission struct {
	ID         uint   `gorm:"primaryKey" json:"-"`
	PluginID   string `gorm:"index;not null" json:"-"`
	Permission string `gorm:"not null" json:"permission"`
}

// SyncMetadata holds the per-user sync sequence counter.
type SyncMetadata struct {
	UserID    int64     `gorm:"primaryKey" json:"userId,string"`
	LastSeq   int64     `gorm:"not null;default:0" json:"lastSeq"`
	UpdatedAt time.Time `gorm:"not null" json:"updatedAt"`
}

// SyncChange is a single change record in the incremental sync log.
type SyncChange struct {
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"-"`
	UserID    int64     `gorm:"not null;index:idx_user_seq,unique" json:"userId,string"`
	Seq       int64     `gorm:"not null;index:idx_user_seq,unique" json:"seq"`
	TableName string    `gorm:"not null" json:"table"`
	Op        string    `gorm:"not null" json:"op"`
	RecordID  string    `gorm:"not null" json:"recordId"`
	Data      *string   `gorm:"type:text" json:"data"`
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
}

// SyncBlob tracks which binary blobs a user has uploaded.
type SyncBlob struct {
	ID        uint      `gorm:"primaryKey" json:"-"`
	UserID    int64     `gorm:"not null;uniqueIndex:idx_user_blob" json:"userId,string"`
	SHA256    string    `gorm:"not null;uniqueIndex:idx_user_blob" json:"sha256"`
	Size      int       `gorm:"not null" json:"size"`
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
}

// RelayProvider stores an admin-managed upstream provider for LynAI relay.
type RelayProvider struct {
	ID        int64  `gorm:"primaryKey" json:"id,string"`
	Name      string `gorm:"not null" json:"name"`
	Endpoint  string `gorm:"not null" json:"endpoint"`
	APIKey    string `gorm:"not null" json:"-"`
	APIFormat string `gorm:"not null;index" json:"apiFormat"`
	Config    string `gorm:"type:text" json:"-"`
	// Models is kept only for migrating legacy newline/JSON model lists into RelayModel rows.
	Models    string       `gorm:"type:text" json:"-"`
	Enabled   bool         `gorm:"default:true;index" json:"enabled"`
	Entries   []RelayModel `gorm:"foreignKey:ProviderID;constraint:OnDelete:CASCADE" json:"entries"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

// RelayModel is one admin-managed model exposed to clients by the relay.
type RelayModel struct {
	ID             int64     `gorm:"primaryKey;autoIncrement" json:"id,string"`
	ProviderID     int64     `gorm:"not null;index:idx_relay_model_provider_name,unique" json:"providerId,string"`
	ModelID        string    `gorm:"not null;index:idx_relay_model_provider_name,unique" json:"modelId"`
	DisplayName    string    `json:"displayName"`
	Description    string    `gorm:"type:text" json:"description"`
	Category       string    `gorm:"not null;default:chat;index" json:"category"`
	Capabilities   string    `gorm:"type:text" json:"capabilities"`
	AdvancedParams string    `gorm:"type:text" json:"advancedParams"`
	Enabled        bool      `gorm:"default:true;index" json:"enabled"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// AllModels returns every model that should be auto-migrated.
func AllModels() []interface{} {
	return []interface{}{
		&User{},
		&Plugin{},
		&PluginScreenshot{},
		&PluginPermission{},
		&SyncMetadata{},
		&SyncChange{},
		&SyncBlob{},
		&RelayProvider{},
		&RelayModel{},
	}
}

// EnsureAdminSeed creates the initial admin account if it does not exist.
// Uses the snowflake generator for the user ID.
func EnsureAdminSeed(db *gorm.DB, phone, displayName, passwordHash string, snowflake *SnowflakeGenerator) error {
	var existing User
	if err := db.Where("phone = ?", phone).First(&existing).Error; err == nil {
		updates := map[string]interface{}{}
		if existing.PasswordHash == "" {
			updates["password_hash"] = passwordHash
		}
		if !existing.IsAdmin {
			updates["is_admin"] = true
		}
		if len(updates) == 0 {
			return nil
		}
		return db.Model(&existing).Updates(updates).Error
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	admin := User{
		ID:           snowflake.NextID(),
		Phone:        phone,
		PasswordHash: passwordHash,
		DisplayName:  displayName,
		IsAdmin:      true,
	}
	return db.Create(&admin).Error
}
