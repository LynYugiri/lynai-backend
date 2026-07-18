package community

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/requestbody"
)

const (
	communityJSONLimit      int64 = 64 << 10
	communityUploadOverhead int64 = 1 << 20
	communityUploadLimit          = MaxImageBytes + communityUploadOverhead
)

// Handler exposes the /community API.
type Handler struct{ svc *Service }

// NewHandler creates a Community HTTP handler.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Feed handles GET /community/posts.
func (h *Handler) Feed(c *gin.Context) {
	page, pageSize := pagination(c)
	result, err := h.svc.ListPosts(c.Request.Context(), optionalUserID(c), nil, page, pageSize)
	h.respond(c, result, err)
}

// PostDetail handles GET /community/posts/:id.
func (h *Handler) PostDetail(c *gin.Context) {
	postID, ok := pathID(c, "id")
	if !ok {
		return
	}
	result, err := h.svc.GetPost(c.Request.Context(), postID, optionalUserID(c))
	h.respond(c, result, err)
}

type postRequest struct {
	Title    string   `json:"title"`
	Content  string   `json:"content"`
	MediaIDs []string `json:"mediaIds"`
}

// CreatePost handles JSON POST /community/posts.
func (h *Handler) CreatePost(c *gin.Context) {
	userID, ok := requiredUserID(c)
	if !ok {
		return
	}
	var req postRequest
	if !bindJSON(c, &req) {
		return
	}
	mediaIDs, ok := requestIDs(c, req.MediaIDs)
	if !ok {
		return
	}
	result, err := h.svc.CreatePost(c.Request.Context(), userID, req.Title, req.Content, mediaIDs)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

// UpdatePost handles PATCH /community/posts/:id.
func (h *Handler) UpdatePost(c *gin.Context) {
	userID, postID, ok := userAndPathID(c)
	if !ok {
		return
	}
	var req postRequest
	if !bindJSON(c, &req) {
		return
	}
	mediaIDs, ok := requestIDs(c, req.MediaIDs)
	if !ok {
		return
	}
	result, err := h.svc.UpdatePost(c.Request.Context(), userID, postID, req.Title, req.Content, mediaIDs)
	h.respond(c, result, err)
}

// DeletePost handles author or administrator soft deletion.
func (h *Handler) DeletePost(c *gin.Context) {
	userID, postID, ok := userAndPathID(c)
	if !ok {
		return
	}
	if err := h.svc.DeletePost(c.Request.Context(), userID, postID, currentIsAdmin(c)); err != nil {
		h.respond(c, nil, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// UploadMedia handles multipart POST /community/media with field "file".
func (h *Handler) UploadMedia(c *gin.Context) {
	userID, ok := requiredUserID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, communityUploadLimit)
	if err := c.Request.ParseMultipartForm(1 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": ErrImageTooLarge.Error()})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart upload"})
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	files := c.Request.MultipartForm.File["file"]
	if len(files) != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exactly one file field is required"})
		return
	}
	if files[0].Size > MaxImageBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": ErrImageTooLarge.Error()})
		return
	}
	file, err := files[0].Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read uploaded image"})
		return
	}
	image, err := h.svc.storage.PutImage(file)
	_ = file.Close()
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrImageTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	result, err := h.svc.UploadMedia(c.Request.Context(), userID, image)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

// PinPost and UnpinPost set the current user's sole profile pin.
func (h *Handler) PinPost(c *gin.Context)   { h.setPinned(c, true) }
func (h *Handler) UnpinPost(c *gin.Context) { h.setPinned(c, false) }

func (h *Handler) setPinned(c *gin.Context, pinned bool) {
	userID, postID, ok := userAndPathID(c)
	if !ok {
		return
	}
	if err := h.svc.SetPinned(c.Request.Context(), userID, postID, pinned); err != nil {
		h.respond(c, nil, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// LikePost and UnlikePost implement idempotent like state.
func (h *Handler) LikePost(c *gin.Context)   { h.setRelation(c, true, h.svc.SetLike) }
func (h *Handler) UnlikePost(c *gin.Context) { h.setRelation(c, false, h.svc.SetLike) }

// FavoritePost and UnfavoritePost implement idempotent favorite state.
func (h *Handler) FavoritePost(c *gin.Context)   { h.setRelation(c, true, h.svc.SetFavorite) }
func (h *Handler) UnfavoritePost(c *gin.Context) { h.setRelation(c, false, h.svc.SetFavorite) }

func (h *Handler) setRelation(c *gin.Context, enabled bool, change func(context.Context, int64, int64, bool) error) {
	userID, postID, ok := userAndPathID(c)
	if !ok {
		return
	}
	if err := change(c.Request.Context(), userID, postID, enabled); err != nil {
		h.respond(c, nil, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// Favorites handles GET /community/me/favorites.
func (h *Handler) Favorites(c *gin.Context) {
	userID, ok := requiredUserID(c)
	if !ok {
		return
	}
	page, pageSize := pagination(c)
	result, err := h.svc.ListFavorites(c.Request.Context(), userID, page, pageSize)
	h.respond(c, result, err)
}

// Comments handles GET /community/posts/:id/comments.
func (h *Handler) Comments(c *gin.Context) {
	postID, ok := pathID(c, "id")
	if !ok {
		return
	}
	page, pageSize := pagination(c)
	result, err := h.svc.ListComments(c.Request.Context(), postID, page, pageSize)
	h.respond(c, result, err)
}

type contentRequest struct {
	Content string `json:"content"`
}

// CreateComment handles POST /community/posts/:id/comments.
func (h *Handler) CreateComment(c *gin.Context) {
	userID, postID, ok := userAndPathID(c)
	if !ok {
		return
	}
	var req contentRequest
	if !bindJSON(c, &req) {
		return
	}
	result, err := h.svc.CreateComment(c.Request.Context(), userID, postID, req.Content)
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	c.JSON(http.StatusCreated, result)
}

// UpdateComment handles PATCH /community/comments/:id.
func (h *Handler) UpdateComment(c *gin.Context) {
	userID, commentID, ok := userAndPathID(c)
	if !ok {
		return
	}
	var req contentRequest
	if !bindJSON(c, &req) {
		return
	}
	result, err := h.svc.UpdateComment(c.Request.Context(), userID, commentID, req.Content)
	h.respond(c, result, err)
}

// DeleteComment handles author, post-author, or administrator soft deletion.
func (h *Handler) DeleteComment(c *gin.Context) {
	userID, commentID, ok := userAndPathID(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteComment(c.Request.Context(), userID, commentID, currentIsAdmin(c)); err != nil {
		h.respond(c, nil, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// User handles GET /community/users/:id.
func (h *Handler) User(c *gin.Context) {
	userID, ok := pathID(c, "id")
	if !ok {
		return
	}
	result, err := h.svc.GetProfile(c.Request.Context(), userID)
	h.respond(c, result, err)
}

// UserPosts handles GET /community/users/:id/posts.
func (h *Handler) UserPosts(c *gin.Context) {
	userID, ok := pathID(c, "id")
	if !ok {
		return
	}
	page, pageSize := pagination(c)
	result, err := h.svc.ListPosts(c.Request.Context(), optionalUserID(c), &userID, page, pageSize)
	h.respond(c, result, err)
}

type profileRequest struct {
	DisplayName   string  `json:"displayName"`
	Bio           string  `json:"bio"`
	AvatarMediaID *string `json:"avatarMediaId"`
}

// UpdateMyProfile handles PATCH /community/me/profile.
func (h *Handler) UpdateMyProfile(c *gin.Context) {
	userID, ok := requiredUserID(c)
	if !ok {
		return
	}
	var req profileRequest
	if !bindJSON(c, &req) {
		return
	}
	var avatarMediaID *int64
	if req.AvatarMediaID != nil {
		value, err := strconv.ParseInt(*req.AvatarMediaID, 10, 64)
		if err != nil || value < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid avatarMediaId"})
			return
		}
		avatarMediaID = &value
	}
	result, err := h.svc.UpdateProfile(c.Request.Context(), userID, req.DisplayName, req.Bio, avatarMediaID)
	h.respond(c, result, err)
}

// Media handles GET /community/media/:id.
func (h *Handler) Media(c *gin.Context) {
	mediaID, ok := pathID(c, "id")
	if !ok {
		return
	}
	media, err := h.svc.GetMedia(c.Request.Context(), mediaID, optionalUserID(c))
	if err != nil {
		h.respond(c, nil, err)
		return
	}
	file, err := h.svc.OpenMedia(media)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "media not found"})
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		c.JSON(http.StatusNotFound, gin.H{"error": "media not found"})
		return
	}
	c.Header("Content-Type", media.MediaType)
	c.Header("Content-Length", strconv.FormatInt(info.Size(), 10))
	c.Header("ETag", `"`+media.SHA256+`"`)
	if media.AttachedAt == nil {
		c.Header("Cache-Control", "private, no-store")
	} else {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, media.SHA256, media.CreatedAt, file)
}

// Admin moderation endpoints.
func (h *Handler) RestorePost(c *gin.Context)    { h.adminAction(c, h.svc.RestorePost) }
func (h *Handler) PurgePost(c *gin.Context)      { h.adminAction(c, h.svc.PurgePost) }
func (h *Handler) RestoreComment(c *gin.Context) { h.adminAction(c, h.svc.RestoreComment) }
func (h *Handler) PurgeComment(c *gin.Context)   { h.adminAction(c, h.svc.PurgeComment) }

// Audit handles GET /community/admin/audit.
func (h *Handler) Audit(c *gin.Context) {
	page, pageSize := pagination(c)
	result, err := h.svc.ListAudit(c.Request.Context(), page, pageSize)
	h.respond(c, result, err)
}

func (h *Handler) adminAction(c *gin.Context, action func(context.Context, int64, int64) error) {
	actorID, targetID, ok := userAndPathID(c)
	if !ok {
		return
	}
	if err := action(c.Request.Context(), actorID, targetID); err != nil {
		h.respond(c, nil, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) respond(c *gin.Context, value any, err error) {
	if err == nil {
		c.JSON(http.StatusOK, value)
		return
	}
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrDeleted):
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	case errors.Is(err, ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "operation not permitted"})
	case errors.Is(err, ErrValidation):
		c.JSON(http.StatusBadRequest, gin.H{"error": strings.TrimPrefix(err.Error(), ErrValidation.Error()+": ")})
	case errors.Is(err, database.ErrSnowflakeUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "id generator unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

func bindJSON(c *gin.Context, target any) bool {
	requestbody.Limit(c, communityJSONLimit)
	if err := c.ShouldBindJSON(target); err != nil {
		if requestbody.TooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
		} else if errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "request body is required"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		}
		return false
	}
	return true
}

func requestIDs(c *gin.Context, raw []string) ([]int64, bool) {
	ids := make([]int64, len(raw))
	for i, value := range raw {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "mediaIds must contain decimal strings"})
			return nil, false
		}
		ids[i] = id
	}
	return ids, true
}

func pagination(c *gin.Context) (int, int) {
	return parsePositive(c.Query("page"), 1), parsePositive(c.Query("page_size"), 20)
}

func parsePositive(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if raw == "" || err != nil || value < 1 {
		return fallback
	}
	return value
}

func pathID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid %s", name)})
		return 0, false
	}
	return id, true
}

func requiredUserID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.GetString("userID"), 10, 64)
	if err != nil || id < 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
		return 0, false
	}
	return id, true
}

func optionalUserID(c *gin.Context) *int64 {
	id, err := strconv.ParseInt(c.GetString("userID"), 10, 64)
	if err != nil || id < 1 {
		return nil
	}
	return &id
}

func currentIsAdmin(c *gin.Context) bool {
	value, ok := c.Get("isAdmin")
	admin, valid := value.(bool)
	return ok && valid && admin
}

func userAndPathID(c *gin.Context) (int64, int64, bool) {
	userID, ok := requiredUserID(c)
	if !ok {
		return 0, 0, false
	}
	targetID, ok := pathID(c, "id")
	return userID, targetID, ok
}
