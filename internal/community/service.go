package community

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	MaxPostTitle      = 120
	MaxPostContent    = 20_000
	MaxCommentContent = 4_000
	MaxBioLength      = 500
	MaxDisplayName    = 32
	MaxPostImages     = 9
)

var (
	ErrNotFound   = errors.New("community record not found")
	ErrForbidden  = errors.New("community operation forbidden")
	ErrDeleted    = errors.New("community record is deleted")
	ErrValidation = errors.New("community validation failed")
)

// PublicProfile is the complete public identity surface for Community.
type PublicProfile struct {
	ID            string  `json:"id"`
	DisplayName   string  `json:"displayName"`
	AvatarURL     *string `json:"avatarUrl"`
	Bio           string  `json:"bio"`
	PostCount     int64   `json:"postCount"`
	FollowerCount int64   `json:"followerCount"`
	PinnedPostID  *string `json:"pinnedPostId"`
}

// MediaResponse describes one uploaded image metadata record.
type MediaResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Size     int64  `json:"size"`
}

// PostResponse is the public post representation.
type PostResponse struct {
	ID           string          `json:"id"`
	Author       PublicProfile   `json:"author"`
	Title        string          `json:"title"`
	Content      string          `json:"content"`
	Media        []MediaResponse `json:"media"`
	LikeCount    int64           `json:"likeCount"`
	CommentCount int64           `json:"commentCount"`
	Liked        bool            `json:"liked"`
	Favorited    bool            `json:"favorited"`
	Pinned       bool            `json:"pinned"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

// CommentResponse is the public comment representation.
type CommentResponse struct {
	ID        string        `json:"id"`
	PostID    string        `json:"postId"`
	Author    PublicProfile `json:"author"`
	Content   string        `json:"content"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

// Page contains bounded page metadata shared by list endpoints.
type Page[T any] struct {
	Items    []T   `json:"items"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
	Total    int64 `json:"total"`
	HasMore  bool  `json:"hasMore"`
}

// AuditResponse is a privacy-safe administrator moderation event.
type AuditResponse struct {
	ID         string    `json:"id"`
	ActorID    string    `json:"actorId"`
	Action     string    `json:"action"`
	TargetType string    `json:"targetType"`
	TargetID   string    `json:"targetId"`
	Detail     string    `json:"detail"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Service implements Community persistence and authorization rules.
type Service struct {
	db        *gorm.DB
	storage   *Storage
	snowflake *database.SnowflakeGenerator
}

// NewService creates the Community service.
func NewService(db *gorm.DB, storage *Storage, snowflake *database.SnowflakeGenerator) *Service {
	return &Service{db: db, storage: storage, snowflake: snowflake}
}

// UploadMedia creates an owned, unattached metadata record for validated bytes.
func (s *Service) UploadMedia(ctx context.Context, ownerID int64, image StoredImage) (*MediaResponse, error) {
	id, err := s.snowflake.NextID(ctx)
	if err != nil {
		return nil, err
	}
	media := database.CommunityMedia{
		ID: id, OwnerUserID: ownerID, SHA256: image.SHA256, Path: image.Path,
		MediaType: image.MediaType, Size: image.Size, Width: image.Width, Height: image.Height,
	}
	if err := s.db.WithContext(ctx).Create(&media).Error; err != nil {
		return nil, err
	}
	response := mediaResponse(media)
	return &response, nil
}

// CreatePost creates a JSON-authored post and atomically attaches owned media.
func (s *Service) CreatePost(ctx context.Context, authorID int64, title, content string, mediaIDs []int64) (*PostResponse, error) {
	title, content, err := validatePost(title, content, mediaIDs)
	if err != nil {
		return nil, err
	}
	postID, err := s.snowflake.NextID(ctx)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&database.CommunityPost{ID: postID, AuthorID: authorID, Title: title, Content: content}).Error; err != nil {
			return err
		}
		return s.replacePostMedia(tx, authorID, postID, mediaIDs)
	})
	if err != nil {
		return nil, err
	}
	return s.GetPost(ctx, postID, &authorID)
}

// UpdatePost atomically updates post text and the ordered media attachment set.
func (s *Service) UpdatePost(ctx context.Context, userID, postID int64, title, content string, mediaIDs []int64) (*PostResponse, error) {
	title, content, err := validatePost(title, content, mediaIDs)
	if err != nil {
		return nil, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var post database.CommunityPost
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", postID).First(&post).Error; err != nil {
			return mapNotFound(err)
		}
		if post.DeletedAt != nil {
			return ErrDeleted
		}
		if post.AuthorID != userID {
			return ErrForbidden
		}
		if err := tx.Model(&post).Updates(map[string]any{"title": title, "content": content}).Error; err != nil {
			return err
		}
		return s.replacePostMedia(tx, userID, postID, mediaIDs)
	})
	if err != nil {
		return nil, err
	}
	return s.GetPost(ctx, postID, &userID)
}

func (s *Service) replacePostMedia(tx *gorm.DB, ownerID, postID int64, mediaIDs []int64) error {
	if hasDuplicateIDs(mediaIDs) {
		return validation("mediaIds must not contain duplicates")
	}
	var current []database.CommunityPostMedia
	if err := tx.Where("post_id = ?", postID).Find(&current).Error; err != nil {
		return err
	}
	currentIDs := make([]int64, 0, len(current))
	for _, link := range current {
		currentIDs = append(currentIDs, link.MediaID)
	}
	if len(mediaIDs) > 0 {
		var media []database.CommunityMedia
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", mediaIDs).Find(&media).Error; err != nil {
			return err
		}
		if len(media) != len(mediaIDs) {
			return validation("one or more mediaIds do not exist")
		}
		currentSet := idSet(currentIDs)
		for _, item := range media {
			if item.OwnerUserID != ownerID {
				return ErrForbidden
			}
			if item.DeletedAt != nil {
				return validation("deleted media cannot be attached")
			}
			if item.AttachedAt != nil {
				if _, alreadyHere := currentSet[item.ID]; !alreadyHere {
					return validation("media is already attached to another post")
				}
			}
		}
	}
	if err := tx.Where("post_id = ?", postID).Delete(&database.CommunityPostMedia{}).Error; err != nil {
		return err
	}
	if len(currentIDs) > 0 {
		if err := tx.Model(&database.CommunityMedia{}).Where("id IN ?", currentIDs).Update("attached_at", nil).Error; err != nil {
			return err
		}
	}
	now := time.Now()
	for order, mediaID := range mediaIDs {
		if err := tx.Create(&database.CommunityPostMedia{PostID: postID, MediaID: mediaID, SortOrder: order}).Error; err != nil {
			return err
		}
	}
	if len(mediaIDs) > 0 {
		if err := tx.Model(&database.CommunityMedia{}).Where("id IN ?", mediaIDs).Update("attached_at", now).Error; err != nil {
			return err
		}
	}
	return nil
}

// ListPosts returns the chronological global feed or one user's pinned-first list.
func (s *Service) ListPosts(ctx context.Context, viewerID *int64, authorID *int64, page, pageSize int) (Page[PostResponse], error) {
	page, pageSize = normalizePage(page, pageSize)
	query := s.db.WithContext(ctx).Model(&database.CommunityPost{}).Where("deleted_at IS NULL")
	order := "created_at DESC, id DESC"
	if authorID != nil {
		query = query.Where("author_id = ?", *authorID)
		var profile database.CommunityProfile
		if err := s.db.WithContext(ctx).First(&profile, "user_id = ?", *authorID).Error; err == nil && profile.PinnedPostID != nil {
			order = "CASE WHEN id = " + strconv.FormatInt(*profile.PinnedPostID, 10) + " THEN 0 ELSE 1 END, created_at DESC, id DESC"
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return Page[PostResponse]{}, err
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return Page[PostResponse]{}, err
	}
	var posts []database.CommunityPost
	if err := query.Order(order).Offset((page - 1) * pageSize).Limit(pageSize).Find(&posts).Error; err != nil {
		return Page[PostResponse]{}, err
	}
	items := make([]PostResponse, 0, len(posts))
	for i := range posts {
		response, err := s.postResponse(ctx, &posts[i], viewerID)
		if err != nil {
			return Page[PostResponse]{}, err
		}
		items = append(items, *response)
	}
	return Page[PostResponse]{Items: items, Page: page, PageSize: pageSize, Total: total, HasMore: int64(page*pageSize) < total}, nil
}

// GetPost returns one visible post.
func (s *Service) GetPost(ctx context.Context, postID int64, viewerID *int64) (*PostResponse, error) {
	var post database.CommunityPost
	if err := s.db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", postID).First(&post).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return s.postResponse(ctx, &post, viewerID)
}

func (s *Service) postResponse(ctx context.Context, post *database.CommunityPost, viewerID *int64) (*PostResponse, error) {
	profile, err := s.GetProfile(ctx, post.AuthorID)
	if err != nil {
		return nil, err
	}
	var likeCount, commentCount int64
	if err := s.db.WithContext(ctx).Model(&database.CommunityLike{}).Where("post_id = ?", post.ID).Count(&likeCount).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&database.CommunityComment{}).Where("post_id = ? AND deleted_at IS NULL", post.ID).Count(&commentCount).Error; err != nil {
		return nil, err
	}
	var mediaRows []database.CommunityMedia
	if err := s.db.WithContext(ctx).Table("community_media").Joins("JOIN community_post_media ON community_post_media.media_id = community_media.id").Where("community_post_media.post_id = ? AND community_media.deleted_at IS NULL", post.ID).Order("community_post_media.sort_order ASC").Find(&mediaRows).Error; err != nil {
		return nil, err
	}
	media := make([]MediaResponse, 0, len(mediaRows))
	for _, item := range mediaRows {
		media = append(media, mediaResponse(item))
	}
	pinned := profile.PinnedPostID != nil && *profile.PinnedPostID == idString(post.ID)
	response := &PostResponse{ID: idString(post.ID), Author: *profile, Title: post.Title, Content: post.Content, Media: media, LikeCount: likeCount, CommentCount: commentCount, Pinned: pinned, CreatedAt: post.CreatedAt, UpdatedAt: post.UpdatedAt}
	if viewerID != nil {
		response.Liked = s.relationExists(ctx, &database.CommunityLike{}, "user_id = ? AND post_id = ?", *viewerID, post.ID)
		response.Favorited = s.relationExists(ctx, &database.CommunityFavorite{}, "user_id = ? AND post_id = ?", *viewerID, post.ID)
	}
	return response, nil
}

// DeletePost soft-deletes an owned post, or any post for an administrator.
func (s *Service) DeletePost(ctx context.Context, actorID, postID int64, isAdmin bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var post database.CommunityPost
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&post, postID).Error; err != nil {
			return mapNotFound(err)
		}
		if post.DeletedAt != nil {
			return ErrDeleted
		}
		if post.AuthorID != actorID && !isAdmin {
			return ErrForbidden
		}
		now := time.Now()
		if err := tx.Model(&post).Update("deleted_at", now).Error; err != nil {
			return err
		}
		if err := tx.Model(&database.CommunityProfile{}).Where("pinned_post_id = ?", postID).Update("pinned_post_id", nil).Error; err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, "delete", "post", postID)
	})
}

// SetPinned idempotently sets or clears the user's sole profile pin.
func (s *Service) SetPinned(ctx context.Context, userID, postID int64, pinned bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var post database.CommunityPost
		if err := tx.Where("id = ? AND deleted_at IS NULL", postID).First(&post).Error; err != nil {
			return mapNotFound(err)
		}
		if post.AuthorID != userID {
			return ErrForbidden
		}
		var value any
		profile := database.CommunityProfile{UserID: userID}
		if pinned {
			value = postID
			profile.PinnedPostID = &postID
		}
		if pinned {
			return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.Assignments(map[string]any{"pinned_post_id": value, "updated_at": time.Now()})}).Create(&profile).Error
		}
		return tx.Model(&database.CommunityProfile{}).Where("user_id = ? AND pinned_post_id = ?", userID, postID).Updates(map[string]any{"pinned_post_id": nil, "updated_at": time.Now()}).Error
	})
}

// SetLike idempotently creates or removes a post like.
func (s *Service) SetLike(ctx context.Context, userID, postID int64, enabled bool) error {
	if err := s.requirePost(ctx, postID); err != nil {
		return err
	}
	relation := database.CommunityLike{UserID: userID, PostID: postID}
	if enabled {
		return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&relation).Error
	}
	return s.db.WithContext(ctx).Where("user_id = ? AND post_id = ?", userID, postID).Delete(&database.CommunityLike{}).Error
}

// SetFavorite idempotently creates or removes a private favorite.
func (s *Service) SetFavorite(ctx context.Context, userID, postID int64, enabled bool) error {
	if err := s.requirePost(ctx, postID); err != nil {
		return err
	}
	relation := database.CommunityFavorite{UserID: userID, PostID: postID}
	if enabled {
		return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&relation).Error
	}
	return s.db.WithContext(ctx).Where("user_id = ? AND post_id = ?", userID, postID).Delete(&database.CommunityFavorite{}).Error
}

// ListFavorites returns the current user's visible saved posts.
func (s *Service) ListFavorites(ctx context.Context, userID int64, page, pageSize int) (Page[PostResponse], error) {
	page, pageSize = normalizePage(page, pageSize)
	query := s.db.WithContext(ctx).Table("community_posts").Joins("JOIN community_favorites ON community_favorites.post_id = community_posts.id").Where("community_favorites.user_id = ? AND community_posts.deleted_at IS NULL", userID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return Page[PostResponse]{}, err
	}
	var posts []database.CommunityPost
	if err := query.Select("community_posts.*").Order("community_favorites.created_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Scan(&posts).Error; err != nil {
		return Page[PostResponse]{}, err
	}
	items := make([]PostResponse, 0, len(posts))
	for i := range posts {
		item, err := s.postResponse(ctx, &posts[i], &userID)
		if err != nil {
			return Page[PostResponse]{}, err
		}
		items = append(items, *item)
	}
	return Page[PostResponse]{Items: items, Page: page, PageSize: pageSize, Total: total, HasMore: int64(page*pageSize) < total}, nil
}

// ListComments returns visible comments in chronological order.
func (s *Service) ListComments(ctx context.Context, postID int64, page, pageSize int) (Page[CommentResponse], error) {
	if err := s.requirePost(ctx, postID); err != nil {
		return Page[CommentResponse]{}, err
	}
	page, pageSize = normalizePage(page, pageSize)
	query := s.db.WithContext(ctx).Model(&database.CommunityComment{}).Where("post_id = ? AND deleted_at IS NULL", postID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return Page[CommentResponse]{}, err
	}
	var comments []database.CommunityComment
	if err := query.Order("created_at ASC, id ASC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&comments).Error; err != nil {
		return Page[CommentResponse]{}, err
	}
	items := make([]CommentResponse, 0, len(comments))
	for i := range comments {
		item, err := s.commentResponse(ctx, &comments[i])
		if err != nil {
			return Page[CommentResponse]{}, err
		}
		items = append(items, *item)
	}
	return Page[CommentResponse]{Items: items, Page: page, PageSize: pageSize, Total: total, HasMore: int64(page*pageSize) < total}, nil
}

// CreateComment creates a Markdown comment on a visible post.
func (s *Service) CreateComment(ctx context.Context, authorID, postID int64, content string) (*CommentResponse, error) {
	content = strings.TrimSpace(content)
	if content == "" || utf8.RuneCountInString(content) > MaxCommentContent {
		return nil, validation("comment is required and must not exceed 4000 characters")
	}
	if err := s.requirePost(ctx, postID); err != nil {
		return nil, err
	}
	id, err := s.snowflake.NextID(ctx)
	if err != nil {
		return nil, err
	}
	comment := database.CommunityComment{ID: id, PostID: postID, AuthorID: authorID, Content: content}
	if err := s.db.WithContext(ctx).Create(&comment).Error; err != nil {
		return nil, err
	}
	return s.commentResponse(ctx, &comment)
}

// UpdateComment edits an owner's visible comment.
func (s *Service) UpdateComment(ctx context.Context, userID, commentID int64, content string) (*CommentResponse, error) {
	content = strings.TrimSpace(content)
	if content == "" || utf8.RuneCountInString(content) > MaxCommentContent {
		return nil, validation("comment is required and must not exceed 4000 characters")
	}
	result := s.db.WithContext(ctx).Model(&database.CommunityComment{}).Where("id = ? AND author_id = ? AND deleted_at IS NULL", commentID, userID).Update("content", content)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, s.ownerError(ctx, &database.CommunityComment{}, commentID, userID)
	}
	var comment database.CommunityComment
	if err := s.db.WithContext(ctx).First(&comment, commentID).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return s.commentResponse(ctx, &comment)
}

// DeleteComment allows the comment author, post author, or an administrator.
func (s *Service) DeleteComment(ctx context.Context, actorID, commentID int64, isAdmin bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var comment database.CommunityComment
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&comment, commentID).Error; err != nil {
			return mapNotFound(err)
		}
		if comment.DeletedAt != nil {
			return ErrDeleted
		}
		var post database.CommunityPost
		if err := tx.First(&post, comment.PostID).Error; err != nil {
			return err
		}
		if actorID != comment.AuthorID && actorID != post.AuthorID && !isAdmin {
			return ErrForbidden
		}
		if err := tx.Model(&comment).Update("deleted_at", time.Now()).Error; err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, "delete", "comment", commentID)
	})
}

func (s *Service) commentResponse(ctx context.Context, comment *database.CommunityComment) (*CommentResponse, error) {
	profile, err := s.GetProfile(ctx, comment.AuthorID)
	if err != nil {
		return nil, err
	}
	return &CommentResponse{ID: idString(comment.ID), PostID: idString(comment.PostID), Author: *profile, Content: comment.Content, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt}, nil
}

// GetProfile returns public user fields and community metadata only.
func (s *Service) GetProfile(ctx context.Context, userID int64) (*PublicProfile, error) {
	var user database.User
	if err := s.db.WithContext(ctx).First(&user, userID).Error; err != nil {
		return nil, mapNotFound(err)
	}
	var profile database.CommunityProfile
	err := s.db.WithContext(ctx).First(&profile, "user_id = ?", userID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	var postCount int64
	if err := s.db.WithContext(ctx).Model(&database.CommunityPost{}).Where("author_id = ? AND deleted_at IS NULL", userID).Count(&postCount).Error; err != nil {
		return nil, err
	}
	var pinnedPostID *string
	if profile.PinnedPostID != nil {
		value := idString(*profile.PinnedPostID)
		pinnedPostID = &value
	}
	return &PublicProfile{ID: idString(user.ID), DisplayName: user.DisplayName, AvatarURL: user.AvatarURL, Bio: profile.Bio, PostCount: postCount, PinnedPostID: pinnedPostID}, nil
}

// UpdateProfile atomically updates the account display name and community bio.
func (s *Service) UpdateProfile(ctx context.Context, userID int64, displayName, bio string, avatarMediaID *int64) (*PublicProfile, error) {
	displayName = strings.TrimSpace(displayName)
	bio = strings.TrimSpace(bio)
	if displayName == "" || utf8.RuneCountInString(displayName) > MaxDisplayName {
		return nil, validation("displayName is required and must not exceed 32 characters")
	}
	if utf8.RuneCountInString(bio) > MaxBioLength {
		return nil, validation("bio must not exceed 500 characters")
	}
	if avatarMediaID != nil {
		return nil, validation("avatarMediaId is not supported in this release")
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&database.User{}).Where("id = ?", userID).Update("display_name", displayName)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		profile := database.CommunityProfile{UserID: userID, Bio: bio}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.Assignments(map[string]any{"bio": bio, "updated_at": time.Now()})}).Create(&profile).Error
	})
	if err != nil {
		return nil, err
	}
	return s.GetProfile(ctx, userID)
}

// GetMedia returns attached public media or an unattached upload to its owner.
func (s *Service) GetMedia(ctx context.Context, mediaID int64, viewerID *int64) (*database.CommunityMedia, error) {
	var media database.CommunityMedia
	if err := s.db.WithContext(ctx).Where("id = ? AND deleted_at IS NULL", mediaID).First(&media).Error; err != nil {
		return nil, mapNotFound(err)
	}
	if viewerID != nil && media.OwnerUserID == *viewerID {
		return &media, nil
	}
	var count int64
	if err := s.db.WithContext(ctx).Table("community_post_media").Joins("JOIN community_posts ON community_posts.id = community_post_media.post_id").Where("community_post_media.media_id = ? AND community_posts.deleted_at IS NULL", mediaID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrNotFound
	}
	return &media, nil
}

// OpenMedia opens media bytes from the configured store.
func (s *Service) OpenMedia(media *database.CommunityMedia) (*os.File, error) {
	return s.storage.Open(media.Path)
}

// DeleteOrphanMedia removes uploads that were never attached to a post.
func (s *Service) DeleteOrphanMedia(ctx context.Context, before time.Time) error {
	var media []database.CommunityMedia
	if err := s.db.WithContext(ctx).
		Where("attached_at IS NULL AND deleted_at IS NULL AND created_at < ?", before).
		Limit(200).
		Find(&media).Error; err != nil {
		return err
	}
	for _, item := range media {
		result := s.db.WithContext(ctx).
			Where("id = ? AND attached_at IS NULL AND deleted_at IS NULL", item.ID).
			Delete(&database.CommunityMedia{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}
		var references int64
		if err := s.db.WithContext(ctx).Model(&database.CommunityMedia{}).
			Where("path = ?", item.Path).Count(&references).Error; err != nil {
			return err
		}
		if references == 0 {
			_ = s.storage.Delete(item.Path)
		}
	}
	return nil
}

// RestorePost restores a soft-deleted post and records an audit event.
func (s *Service) RestorePost(ctx context.Context, actorID, postID int64) error {
	return s.adminChange(ctx, actorID, "restore", "post", postID, func(tx *gorm.DB) error {
		result := tx.Model(&database.CommunityPost{}).Where("id = ? AND deleted_at IS NOT NULL", postID).Update("deleted_at", nil)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RestoreComment restores a soft-deleted comment when its post is visible.
func (s *Service) RestoreComment(ctx context.Context, actorID, commentID int64) error {
	return s.adminChange(ctx, actorID, "restore", "comment", commentID, func(tx *gorm.DB) error {
		result := tx.Model(&database.CommunityComment{}).Where("id = ? AND deleted_at IS NOT NULL AND post_id IN (SELECT id FROM community_posts WHERE deleted_at IS NULL)", commentID).Update("deleted_at", nil)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// PurgeComment permanently deletes only a soft-deleted comment.
func (s *Service) PurgeComment(ctx context.Context, actorID, commentID int64) error {
	return s.adminChange(ctx, actorID, "purge", "comment", commentID, func(tx *gorm.DB) error {
		result := tx.Where("id = ? AND deleted_at IS NOT NULL", commentID).Delete(&database.CommunityComment{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// PurgePost permanently deletes only a soft-deleted post and its attached metadata.
func (s *Service) PurgePost(ctx context.Context, actorID, postID int64) error {
	var deletedMedia []database.CommunityMedia
	err := s.adminChange(ctx, actorID, "purge", "post", postID, func(tx *gorm.DB) error {
		var post database.CommunityPost
		if err := tx.Where("id = ? AND deleted_at IS NOT NULL", postID).First(&post).Error; err != nil {
			return mapNotFound(err)
		}
		if err := tx.Table("community_media").Joins("JOIN community_post_media ON community_post_media.media_id = community_media.id").Where("community_post_media.post_id = ?", postID).Find(&deletedMedia).Error; err != nil {
			return err
		}
		for _, model := range []any{&database.CommunityLike{}, &database.CommunityFavorite{}, &database.CommunityComment{}, &database.CommunityPostMedia{}} {
			if err := tx.Where("post_id = ?", postID).Delete(model).Error; err != nil {
				return err
			}
		}
		if len(deletedMedia) > 0 {
			ids := make([]int64, 0, len(deletedMedia))
			for _, media := range deletedMedia {
				ids = append(ids, media.ID)
			}
			if err := tx.Where("id IN ?", ids).Delete(&database.CommunityMedia{}).Error; err != nil {
				return err
			}
		}
		return tx.Delete(&database.CommunityPost{}, "id = ?", postID).Error
	})
	if err != nil {
		return err
	}
	for _, media := range deletedMedia {
		var remaining int64
		if s.db.WithContext(ctx).Model(&database.CommunityMedia{}).Where("path = ?", media.Path).Count(&remaining).Error == nil && remaining == 0 {
			_ = s.storage.Delete(media.Path)
		}
	}
	return nil
}

// ListAudit returns recent community moderation events for administrators.
func (s *Service) ListAudit(ctx context.Context, page, pageSize int) (Page[AuditResponse], error) {
	page, pageSize = normalizePage(page, pageSize)
	query := s.db.WithContext(ctx).Model(&database.CommunityAuditRecord{})
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return Page[AuditResponse]{}, err
	}
	var records []database.CommunityAuditRecord
	if err := query.Order("created_at DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&records).Error; err != nil {
		return Page[AuditResponse]{}, err
	}
	items := make([]AuditResponse, 0, len(records))
	for _, record := range records {
		items = append(items, AuditResponse{ID: idString(record.ID), ActorID: idString(record.ActorID), Action: record.Action, TargetType: record.TargetType, TargetID: idString(record.TargetID), Detail: record.Detail, CreatedAt: record.CreatedAt})
	}
	return Page[AuditResponse]{Items: items, Page: page, PageSize: pageSize, Total: total, HasMore: int64(page*pageSize) < total}, nil
}

func (s *Service) adminChange(ctx context.Context, actorID int64, action, targetType string, targetID int64, change func(*gorm.DB) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := change(tx); err != nil {
			return err
		}
		return s.audit(ctx, tx, actorID, action, targetType, targetID)
	})
}

func (s *Service) audit(ctx context.Context, tx *gorm.DB, actorID int64, action, targetType string, targetID int64) error {
	auditID, err := s.snowflake.NextID(ctx)
	if err != nil {
		return err
	}
	return tx.Create(&database.CommunityAuditRecord{ID: auditID, ActorID: actorID, Action: action, TargetType: targetType, TargetID: targetID}).Error
}

func validatePost(title, content string, mediaIDs []int64) (string, string, error) {
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if utf8.RuneCountInString(title) > MaxPostTitle {
		return "", "", validation("title must not exceed 120 characters")
	}
	if utf8.RuneCountInString(content) > MaxPostContent {
		return "", "", validation("content must not exceed 20000 characters")
	}
	if len(mediaIDs) > MaxPostImages {
		return "", "", validation("a post may contain at most 9 images")
	}
	if title == "" && content == "" && len(mediaIDs) == 0 {
		return "", "", validation("title, content, or mediaIds is required")
	}
	return title, content, nil
}

func (s *Service) requirePost(ctx context.Context, postID int64) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(&database.CommunityPost{}).Where("id = ? AND deleted_at IS NULL", postID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) ownerError(ctx context.Context, model any, id, userID int64) error {
	var count int64
	if err := s.db.WithContext(ctx).Model(model).Where("id = ?", id).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	var ownerCount int64
	if err := s.db.WithContext(ctx).Model(model).Where("id = ? AND author_id = ?", id, userID).Count(&ownerCount).Error; err != nil {
		return err
	}
	if ownerCount == 0 {
		return ErrForbidden
	}
	return ErrDeleted
}

func (s *Service) relationExists(ctx context.Context, model any, query string, args ...any) bool {
	var count int64
	return s.db.WithContext(ctx).Model(model).Where(query, args...).Count(&count).Error == nil && count > 0
}

func mediaResponse(media database.CommunityMedia) MediaResponse {
	return MediaResponse{ID: idString(media.ID), URL: "/community/media/" + idString(media.ID), MimeType: media.MediaType, Width: media.Width, Height: media.Height, Size: media.Size}
}

func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 50 {
		pageSize = 50
	}
	return page, pageSize
}

func idSet(ids []int64) map[int64]struct{} {
	result := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		result[id] = struct{}{}
	}
	return result
}

func hasDuplicateIDs(ids []int64) bool { return len(idSet(ids)) != len(ids) }

func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }

func idString(id int64) string { return strconv.FormatInt(id, 10) }

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
