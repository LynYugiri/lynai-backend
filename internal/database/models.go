package database

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// UserSession is a revocable login session. RefreshJTI is replaced on every
// successful refresh so a refresh token can be consumed only once.
type UserSession struct {
	ID         string     `gorm:"primaryKey;size:64" json:"id"`
	UserID     int64      `gorm:"not null;index" json:"userId,string"`
	RefreshJTI string     `gorm:"not null;size:64" json:"-"`
	ExpiresAt  time.Time  `gorm:"not null;index" json:"expiresAt"`
	RevokedAt  *time.Time `gorm:"index" json:"revokedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// AdminSession is an opaque browser session. Only the SHA-256 token digest is
// stored so a database disclosure does not expose usable administrator cookies.
type AdminSession struct {
	TokenHash []byte    `gorm:"primaryKey;size:32" json:"-"`
	UserID    int64     `gorm:"not null;index" json:"userId,string"`
	ExpiresAt time.Time `gorm:"not null;index" json:"expiresAt"`
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
	UpdatedAt time.Time `gorm:"not null" json:"updatedAt"`
}

// UserDevice is one user's binding to a deterministic Ed25519 device identity.
type UserDevice struct {
	UserID    int64      `gorm:"primaryKey;autoIncrement:false;uniqueIndex:idx_user_devices_user_public_key,priority:1;index:idx_user_devices_user_session,priority:1;index:idx_user_devices_user_revoked,priority:1" json:"userId,string"`
	DeviceID  string     `gorm:"primaryKey;size:52" json:"id"`
	SessionID string     `gorm:"not null;size:64;index:idx_user_devices_user_session,priority:2" json:"-"`
	Name      string     `gorm:"not null;size:64" json:"name"`
	Platform  string     `gorm:"not null;size:32" json:"platform"`
	Protocol  uint16     `gorm:"not null" json:"protocolVersion"`
	PublicKey []byte     `gorm:"not null;size:32;uniqueIndex:idx_user_devices_user_public_key,priority:2" json:"-"`
	RevokedAt *time.Time `gorm:"index:idx_user_devices_user_revoked,priority:2" json:"revokedAt"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// DeviceChallenge binds a short-lived challenge to proposed enrollment data.
type DeviceChallenge struct {
	ID            string     `gorm:"primaryKey;size:32" json:"id"`
	UserID        int64      `gorm:"not null;index" json:"userId,string"`
	SessionID     string     `gorm:"not null;size:64;index" json:"-"`
	DeviceID      string     `gorm:"not null;size:52" json:"deviceId"`
	PublicKey     []byte     `gorm:"not null;size:32" json:"-"`
	Name          string     `gorm:"not null;size:64" json:"name"`
	Platform      string     `gorm:"not null;size:32" json:"platform"`
	Protocol      uint16     `gorm:"not null" json:"protocolVersion"`
	ChallengeHash []byte     `gorm:"not null;size:32" json:"-"`
	ExpiresAt     time.Time  `gorm:"not null;index" json:"expiresAt"`
	ConsumedAt    *time.Time `gorm:"index" json:"consumedAt"`
	CreatedAt     time.Time  `gorm:"not null" json:"createdAt"`
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
	ID              int64     `gorm:"primaryKey;autoIncrement" json:"-"`
	UserID          int64     `gorm:"not null;index:idx_user_seq,unique;uniqueIndex:idx_sync_changes_user_change" json:"userId,string"`
	Seq             int64     `gorm:"not null;index:idx_user_seq,unique" json:"seq"`
	ChangeID        string    `gorm:"not null;size:128;uniqueIndex:idx_sync_changes_user_change" json:"changeId"`
	DeviceID        *string   `gorm:"size:52;index" json:"deviceId,omitempty"`
	TableName       string    `gorm:"not null" json:"table"`
	Op              string    `gorm:"not null" json:"op"`
	RecordID        string    `gorm:"not null" json:"recordId"`
	Data            *string   `gorm:"type:text" json:"data"`
	ClientCreatedAt time.Time `gorm:"not null" json:"clientCreatedAt"`
	CreatedAt       time.Time `gorm:"not null" json:"createdAt"`
}

// SyncRequestReplay stores the exact committed response for a sync request ID.
type SyncRequestReplay struct {
	ID                  int64     `gorm:"primaryKey;autoIncrement" json:"-"`
	UserID              int64     `gorm:"not null;uniqueIndex:idx_sync_request_replays_user_request" json:"userId,string"`
	RequestID           string    `gorm:"not null;size:32;uniqueIndex:idx_sync_request_replays_user_request" json:"requestId"`
	Operation           string    `gorm:"not null;size:128" json:"operation"`
	BodyHash            []byte    `gorm:"not null;size:32" json:"-"`
	ResponseStatus      int       `gorm:"not null" json:"responseStatus"`
	ResponseContentType string    `gorm:"not null;size:128" json:"responseContentType"`
	ResponseBody        []byte    `gorm:"not null" json:"-"`
	CreatedAt           time.Time `gorm:"not null" json:"createdAt"`
	ExpiresAt           time.Time `gorm:"not null;index" json:"expiresAt"`
}

// SyncBlob tracks which binary blobs a user has uploaded.
type SyncBlob struct {
	ID        uint      `gorm:"primaryKey" json:"-"`
	UserID    int64     `gorm:"not null;uniqueIndex:idx_user_blob" json:"userId,string"`
	SHA256    string    `gorm:"not null;uniqueIndex:idx_user_blob" json:"sha256"`
	Size      int64     `gorm:"not null" json:"size"`
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
}

// RelayProvider stores an admin-managed upstream provider for LynAI relay.
type RelayProvider struct {
	ID        int64        `gorm:"primaryKey" json:"id,string"`
	Name      string       `gorm:"not null" json:"name"`
	Endpoint  string       `gorm:"not null" json:"endpoint"`
	APIKey    string       `gorm:"not null" json:"-"`
	APIFormat string       `gorm:"not null;index" json:"apiFormat"`
	Config    string       `gorm:"type:text" json:"-"`
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

// RelayRequestLog stores privacy-safe metadata for one authenticated relay call.
type RelayRequestLog struct {
	ID             int64     `gorm:"primaryKey;autoIncrement" json:"id,string"`
	UserID         int64     `gorm:"not null;index" json:"userId,string"`
	Username       string    `gorm:"not null" json:"username"`
	ProviderID     int64     `gorm:"index" json:"providerId,string"`
	ProviderName   string    `json:"providerName"`
	APIType        string    `gorm:"index" json:"apiType"`
	ModelID        string    `gorm:"index" json:"modelId"`
	Category       string    `json:"category"`
	Operation      string    `gorm:"not null;index" json:"operation"`
	Route          string    `gorm:"not null" json:"route"`
	Protocol       string    `gorm:"not null;index" json:"protocol"`
	HTTPStatus     int       `gorm:"not null;index" json:"httpStatus"`
	UpstreamStatus int       `json:"upstreamStatus"`
	DurationMS     int64     `gorm:"not null" json:"durationMs"`
	RequestBytes   int64     `gorm:"not null" json:"requestBytes"`
	ResponseBytes  int64     `gorm:"not null" json:"responseBytes"`
	ErrorType      string    `gorm:"index" json:"errorType"`
	CreatedAt      time.Time `gorm:"not null;index" json:"createdAt"`
}

// RelaySpeechSession is the shared state for one long-running speech relay.
// A row with an empty UpstreamAudioID is a capacity reservation in progress.
type RelaySpeechSession struct {
	ID              string    `gorm:"primaryKey;size:32" json:"id"`
	UserID          int64     `gorm:"not null;index:idx_relay_speech_user_expires,priority:1" json:"userId,string"`
	ProviderID      int64     `gorm:"not null" json:"providerId,string"`
	ModelID         string    `gorm:"not null" json:"modelId"`
	AppID           string    `gorm:"not null" json:"-"`
	UpstreamAudioID string    `gorm:"not null" json:"-"`
	TaskID          string    `gorm:"not null" json:"-"`
	ExpiresAt       time.Time `gorm:"not null;index;index:idx_relay_speech_user_expires,priority:2" json:"expiresAt"`
	CreatedAt       time.Time `gorm:"not null" json:"createdAt"`
	UpdatedAt       time.Time `gorm:"not null" json:"updatedAt"`
}

// CommunityProfile stores public community-only profile data.
type CommunityProfile struct {
	UserID       int64     `gorm:"primaryKey;autoIncrement:false" json:"userId,string"`
	Bio          string    `gorm:"not null;type:text" json:"bio"`
	PinnedPostID *int64    `gorm:"index" json:"pinnedPostId,string"`
	CreatedAt    time.Time `gorm:"not null" json:"createdAt"`
	UpdatedAt    time.Time `gorm:"not null" json:"updatedAt"`
}

// CommunityPost is a user-authored Markdown post. DeletedAt is an explicit
// soft-delete marker so administrators can restore records through the API.
type CommunityPost struct {
	ID        int64      `gorm:"primaryKey;autoIncrement:false" json:"id,string"`
	AuthorID  int64      `gorm:"not null;index" json:"authorId,string"`
	Title     string     `gorm:"not null;size:120" json:"title"`
	Content   string     `gorm:"not null;type:text" json:"content"`
	DeletedAt *time.Time `gorm:"index" json:"deletedAt"`
	CreatedAt time.Time  `gorm:"not null" json:"createdAt"`
	UpdatedAt time.Time  `gorm:"not null" json:"updatedAt"`
}

// CommunityComment is a Markdown comment on a post.
type CommunityComment struct {
	ID        int64      `gorm:"primaryKey;autoIncrement:false" json:"id,string"`
	PostID    int64      `gorm:"not null;index" json:"postId,string"`
	AuthorID  int64      `gorm:"not null;index" json:"authorId,string"`
	Content   string     `gorm:"not null;type:text" json:"content"`
	DeletedAt *time.Time `gorm:"index" json:"deletedAt"`
	CreatedAt time.Time  `gorm:"not null" json:"createdAt"`
	UpdatedAt time.Time  `gorm:"not null" json:"updatedAt"`
}

// CommunityLike records one user's idempotent like of a post.
type CommunityLike struct {
	UserID    int64     `gorm:"primaryKey;autoIncrement:false" json:"userId,string"`
	PostID    int64     `gorm:"primaryKey;autoIncrement:false;index" json:"postId,string"`
	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
}

// CommunityFavorite records one user's private saved post.
type CommunityFavorite struct {
	UserID    int64     `gorm:"primaryKey;autoIncrement:false;index" json:"userId,string"`
	PostID    int64     `gorm:"primaryKey;autoIncrement:false" json:"postId,string"`
	CreatedAt time.Time `gorm:"not null;index" json:"createdAt"`
}

// CommunityMedia describes one immutable content-addressed image.
type CommunityMedia struct {
	ID          int64      `gorm:"primaryKey;autoIncrement:false" json:"id,string"`
	OwnerUserID int64      `gorm:"not null;index" json:"ownerUserId,string"`
	SHA256      string     `gorm:"not null;size:64;index" json:"sha256"`
	Path        string     `gorm:"not null" json:"-"`
	MediaType   string     `gorm:"not null;size:32" json:"mimeType"`
	Size        int64      `gorm:"not null" json:"size"`
	Width       int        `gorm:"not null" json:"width"`
	Height      int        `gorm:"not null" json:"height"`
	AttachedAt  *time.Time `gorm:"index" json:"attachedAt"`
	DeletedAt   *time.Time `gorm:"index" json:"deletedAt"`
	CreatedAt   time.Time  `gorm:"not null" json:"createdAt"`
}

// CommunityPostMedia preserves image order independently of post topology.
type CommunityPostMedia struct {
	PostID    int64 `gorm:"primaryKey;autoIncrement:false" json:"postId,string"`
	MediaID   int64 `gorm:"not null;uniqueIndex" json:"mediaId,string"`
	SortOrder int   `gorm:"primaryKey;autoIncrement:false" json:"sortOrder"`
}

// CommunityAuditRecord stores administrator restore and purge actions.
type CommunityAuditRecord struct {
	ID         int64     `gorm:"primaryKey;autoIncrement:false" json:"id,string"`
	ActorID    int64     `gorm:"not null;index" json:"actorId,string"`
	Action     string    `gorm:"not null;size:64" json:"action"`
	TargetType string    `gorm:"not null;size:32;index:idx_community_audit_target,priority:1" json:"targetType"`
	TargetID   int64     `gorm:"not null;index:idx_community_audit_target,priority:2" json:"targetId,string"`
	Detail     string    `gorm:"not null;type:text" json:"detail"`
	CreatedAt  time.Time `gorm:"not null;index" json:"createdAt"`
}

// AllModels returns every model that should be auto-migrated.
func AllModels() []interface{} {
	return []interface{}{
		&User{},
		&UserSession{},
		&AdminSession{},
		&UserDevice{},
		&DeviceChallenge{},
		&Plugin{},
		&PluginScreenshot{},
		&PluginPermission{},
		&SyncMetadata{},
		&SyncChange{},
		&SyncRequestReplay{},
		&SyncBlob{},
		&RelayProvider{},
		&RelayModel{},
		&RelayRequestLog{},
		&RelaySpeechSession{},
		&CommunityProfile{},
		&CommunityPost{},
		&CommunityComment{},
		&CommunityLike{},
		&CommunityFavorite{},
		&CommunityMedia{},
		&CommunityPostMedia{},
		&CommunityAuditRecord{},
	}
}

// ErrAdminSeedConflict is returned when the seed phone belongs to a non-admin.
var ErrAdminSeedConflict = errors.New("admin seed phone belongs to a non-admin user")

// EnsureAdminSeed creates the initial admin account if it does not exist. An
// existing administrator is preserved, while a non-admin produces an error.
func EnsureAdminSeed(ctx context.Context, db *gorm.DB, phone, displayName, passwordHash string, snowflake *SnowflakeGenerator) error {
	id, err := snowflake.NextID(ctx)
	if err != nil {
		return err
	}
	admin := User{
		ID:           id,
		Phone:        phone,
		PasswordHash: passwordHash,
		DisplayName:  displayName,
		IsAdmin:      true,
	}
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&admin).Error; err != nil {
		return err
	}

	var existing User
	if err := db.WithContext(ctx).Where("phone = ?", phone).First(&existing).Error; err != nil {
		return err
	}
	if !existing.IsAdmin {
		return ErrAdminSeedConflict
	}
	if existing.PasswordHash != "" {
		return nil
	}
	return db.WithContext(ctx).Model(&User{}).
		Where("id = ? AND is_admin = ? AND password_hash = ''", existing.ID, true).
		Update("password_hash", passwordHash).Error
}
