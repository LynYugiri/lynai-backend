package community_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lynai/backend/internal/community"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/testutil"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const communityPassword = "secret123"

type testUser struct {
	ID    string
	Token string
}

type mediaResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
}

type postResponse struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Liked     bool   `json:"liked"`
	Favorited bool   `json:"favorited"`
	Pinned    bool   `json:"pinned"`
	LikeCount int64  `json:"likeCount"`
	Author    struct {
		ID           string  `json:"id"`
		DisplayName  string  `json:"displayName"`
		Bio          string  `json:"bio"`
		PinnedPostID *string `json:"pinnedPostId"`
	} `json:"author"`
	Media []mediaResponse `json:"media"`
}

func TestCommunityCanonicalUploadCreateUpdateAndPrivacy(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	owner := registerUser(t, ts.URL, "13810000001", "Owner")
	other := registerUser(t, ts.URL, "13810000002", "Other")
	imageBytes := validPNG(t)
	first := uploadMedia(t, ts.URL, owner.Token, imageBytes)
	second := uploadMedia(t, ts.URL, owner.Token, imageBytes)
	otherMedia := uploadMedia(t, ts.URL, other.Token, imageBytes)
	if first.ID == second.ID || first.ID == otherMedia.ID {
		t.Fatal("same content was merged into one metadata ID")
	}

	requireMediaStatus(t, ts.URL, first.ID, "", http.StatusNotFound)
	requireMediaStatus(t, ts.URL, first.ID, owner.Token, http.StatusOK)
	requireMediaStatus(t, ts.URL, first.ID, other.Token, http.StatusNotFound)

	bad := createPostRequest(t, ts.URL, owner.Token, "foreign", "body", []string{otherMedia.ID})
	testutil.RequireStatus(t, bad, http.StatusForbidden)
	bad.Body.Close()

	post := createPost(t, ts.URL, owner.Token, "Title", "body", []string{first.ID, second.ID})
	if post.Title != "Title" || len(post.Media) != 2 || post.Media[0].ID != first.ID || post.Media[1].ID != second.ID {
		t.Fatalf("created post = %+v", post)
	}
	requireMediaStatus(t, ts.URL, first.ID, "", http.StatusOK)

	reordered := updatePost(t, ts.URL, owner.Token, post.ID, "Reordered", "new body", []string{second.ID, first.ID})
	if len(reordered.Media) != 2 || reordered.Media[0].ID != second.ID || reordered.Media[1].ID != first.ID {
		t.Fatalf("reordered post = %+v", reordered)
	}
	failed := updatePostRequest(t, ts.URL, owner.Token, post.ID, "Must roll back", "bad", []string{otherMedia.ID})
	testutil.RequireStatus(t, failed, http.StatusForbidden)
	failed.Body.Close()
	if afterFailure := getPost(t, ts.URL, post.ID, owner.Token); afterFailure.Title != "Reordered" || afterFailure.Media[0].ID != second.ID {
		t.Fatalf("failed media update was not atomic: %+v", afterFailure)
	}
	updated := updatePost(t, ts.URL, owner.Token, post.ID, "Updated", "new body", []string{second.ID})
	if updated.Title != "Updated" || len(updated.Media) != 1 || updated.Media[0].ID != second.ID {
		t.Fatalf("updated post = %+v", updated)
	}
	requireMediaStatus(t, ts.URL, first.ID, "", http.StatusNotFound)
	requireMediaStatus(t, ts.URL, first.ID, owner.Token, http.StatusOK)

	secondPost := createPost(t, ts.URL, owner.Token, "Reattach", "", []string{first.ID})
	if secondPost.Media[0].ID != first.ID {
		t.Fatal("removed media could not be reattached by its owner")
	}
	conflict := createPostRequest(t, ts.URL, owner.Token, "duplicate attachment", "", []string{first.ID})
	testutil.RequireStatus(t, conflict, http.StatusBadRequest)
	conflict.Body.Close()
}

func TestCommunityClientRoutesProfileFavoritesAndOptionalAuth(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	user := registerUser(t, ts.URL, "13810000003", "Before")
	post := createPost(t, ts.URL, user.Token, "Client title", "hello", nil)

	req := testutil.NewJSONRequest(t, http.MethodPatch, ts.URL+"/community/me/profile", map[string]any{"displayName": "After", "bio": "public bio"})
	testutil.SetBearer(req, user.Token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	var profile struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		Bio         string `json:"bio"`
	}
	testutil.DecodeJSON(t, resp, &profile)
	resp.Body.Close()
	if profile.ID != user.ID || profile.DisplayName != "After" || profile.Bio != "public bio" {
		t.Fatalf("updated profile = %+v", profile)
	}

	resp, err := http.Get(ts.URL + "/community/users/" + user.ID)
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	body := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("13810000003")) || bytes.Contains(body, []byte("phone")) || bytes.Contains(body, []byte("email")) {
		t.Fatalf("public user leaked private fields: %s", body)
	}

	authorizedStatus(t, http.MethodPut, ts.URL+"/community/posts/"+post.ID+"/favorite", user.Token, http.StatusNoContent)
	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/community/me/favorites", nil)
	testutil.SetBearer(req, user.Token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	if favoritesBody := testutil.ReadAll(t, resp.Body); !bytes.Contains(favoritesBody, []byte(post.ID)) {
		t.Fatalf("favorites missing post: %s", favoritesBody)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/community/users/" + user.ID + "/posts")
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusOK)
	resp.Body.Close()

	req = testutil.NewRequest(t, http.MethodGet, ts.URL+"/community/posts", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusUnauthorized)
	resp.Body.Close()
}

func TestCommunityPinIsProfileOnlyAndSingle(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	user := registerUser(t, ts.URL, "13810000004", "Pinned")
	first := createPost(t, ts.URL, user.Token, "First", "", nil)
	second := createPost(t, ts.URL, user.Token, "Second", "", nil)
	authorizedStatus(t, http.MethodPut, ts.URL+"/community/me/pinned-post/"+first.ID, user.Token, http.StatusNoContent)

	global := listPosts(t, ts.URL+"/community/posts")
	if len(global) != 2 || global[0].ID != second.ID || global[1].ID != first.ID {
		t.Fatalf("global feed was influenced by profile pin: %+v", global)
	}
	profilePosts := listPosts(t, ts.URL+"/community/users/"+user.ID+"/posts")
	if len(profilePosts) != 2 || profilePosts[0].ID != first.ID || !profilePosts[0].Pinned {
		t.Fatalf("profile list did not prioritize pinned post: %+v", profilePosts)
	}

	authorizedStatus(t, http.MethodPut, ts.URL+"/community/me/pinned-post/"+second.ID, user.Token, http.StatusNoContent)
	profilePosts = listPosts(t, ts.URL+"/community/users/"+user.ID+"/posts")
	if profilePosts[0].ID != second.ID || !profilePosts[0].Pinned || profilePosts[1].Pinned {
		t.Fatalf("single pin replacement failed: %+v", profilePosts)
	}
}

func TestCommunityDeletionPermissionsAuditAndAdminPurgeGuard(t *testing.T) {
	adminPhone, adminPassword, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	postOwner := registerUser(t, ts.URL, "13810000005", "Post owner")
	commenter := registerUser(t, ts.URL, "13810000006", "Commenter")
	other := registerUser(t, ts.URL, "13810000007", "Other")
	adminToken := testutil.LoginAndGetToken(t, ts.URL, adminPhone, adminPassword)
	post := createPost(t, ts.URL, postOwner.Token, "Moderation", "body", nil)
	commentID := createComment(t, ts.URL, commenter.Token, post.ID, "comment")

	authorizedStatus(t, http.MethodDelete, ts.URL+"/community/comments/"+commentID, other.Token, http.StatusForbidden)
	authorizedStatus(t, http.MethodDelete, ts.URL+"/community/comments/"+commentID, postOwner.Token, http.StatusNoContent)
	authorizedStatus(t, http.MethodPost, ts.URL+"/community/admin/comments/"+commentID+"/restore", adminToken, http.StatusNoContent)

	authorizedStatus(t, http.MethodDelete, ts.URL+"/community/admin/posts/"+post.ID+"/purge", adminToken, http.StatusNotFound)
	authorizedStatus(t, http.MethodDelete, ts.URL+"/community/posts/"+post.ID, adminToken, http.StatusNoContent)
	resp, err := http.Get(ts.URL + "/community/posts/" + post.ID)
	if err != nil {
		t.Fatal(err)
	}
	testutil.RequireStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
	authorizedStatus(t, http.MethodPost, ts.URL+"/community/admin/posts/"+post.ID+"/restore", adminToken, http.StatusNoContent)
	_ = getPost(t, ts.URL, post.ID, "")
	authorizedStatus(t, http.MethodDelete, ts.URL+"/community/posts/"+post.ID, postOwner.Token, http.StatusNoContent)
	authorizedStatus(t, http.MethodDelete, ts.URL+"/community/admin/posts/"+post.ID+"/purge", adminToken, http.StatusNoContent)
	req := testutil.NewRequest(t, http.MethodGet, ts.URL+"/community/admin/audit", nil)
	testutil.SetBearer(req, adminToken)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusOK)
	auditBody := testutil.ReadAll(t, resp.Body)
	resp.Body.Close()
	for _, action := range []string{`"action":"delete"`, `"action":"restore"`, `"action":"purge"`} {
		if !bytes.Contains(auditBody, []byte(action)) {
			t.Fatalf("audit response missing %s: %s", action, auditBody)
		}
	}
}

func TestCommunityValidationLimits(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	user := registerUser(t, ts.URL, "13810000008", "Limits")
	resp := createPostRequest(t, ts.URL, user.Token, strings.Repeat("x", 121), "body", nil)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()
	post := createPost(t, ts.URL, user.Token, "Comments", "", nil)
	req := testutil.NewJSONRequest(t, http.MethodPost, ts.URL+"/community/posts/"+post.ID+"/comments", map[string]string{"content": strings.Repeat("x", 4001)})
	testutil.SetBearer(req, user.Token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	req = testutil.NewJSONRequest(t, http.MethodPatch, ts.URL+"/community/me/profile", map[string]any{"displayName": "Name", "bio": "bio", "avatarMediaId": "1"})
	testutil.SetBearer(req, user.Token)
	resp = testutil.Do(t, req)
	testutil.RequireStatus(t, resp, http.StatusBadRequest)
	resp.Body.Close()

	badUpload := uploadMediaRequest(t, ts.URL, user.Token, []byte("not an image"))
	testutil.RequireStatus(t, badUpload, http.StatusBadRequest)
	badUpload.Body.Close()
}

func TestCommunityPageSizeCapsAtFifty(t *testing.T) {
	_, _, ts, cleanup := testutil.SetupTest()
	defer cleanup()
	resp, err := http.Get(ts.URL + "/community/posts?page=1&page_size=500")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var page map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if page["pageSize"] != float64(50) {
		t.Fatalf("pageSize = %v, want 50", page["pageSize"])
	}
}

func TestCommunityOrphanCleanupPreservesSharedContent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.User{}, &database.CommunityMedia{}); err != nil {
		t.Fatal(err)
	}
	snowflake := database.NewSnowflakeGenerator(0)
	storage, err := community.NewStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := community.NewService(db, storage, snowflake)
	image, err := storage.PutImage(bytes.NewReader(validPNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-25 * time.Hour)
	rows := []database.CommunityMedia{
		{ID: 1, OwnerUserID: 1, SHA256: image.SHA256, Path: image.Path, MediaType: image.MediaType, Size: image.Size, Width: image.Width, Height: image.Height, CreatedAt: old},
		{ID: 2, OwnerUserID: 1, SHA256: image.SHA256, Path: image.Path, MediaType: image.MediaType, Size: image.Size, Width: image.Width, Height: image.Height, CreatedAt: time.Now()},
	}
	if err := db.Create(&database.User{ID: 1, Phone: "13810000009", DisplayName: "Media owner"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteOrphanMedia(context.Background(), time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&database.CommunityMedia{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("media metadata count = %d, want 1", count)
	}
	file, err := storage.Open(image.Path)
	if err != nil {
		t.Fatalf("shared physical media was deleted: %v", err)
	}
	file.Close()
}

func registerUser(t *testing.T, baseURL, phone, displayName string) testUser {
	t.Helper()
	resp := testutil.PostJSON(t, baseURL+"/auth/register", map[string]string{"phone": phone, "password": communityPassword, "displayName": displayName})
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var result struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Token struct {
			AccessToken string `json:"accessToken"`
		} `json:"token"`
	}
	testutil.DecodeJSON(t, resp, &result)
	return testUser{ID: result.User.ID, Token: result.Token.AccessToken}
}

func uploadMedia(t *testing.T, baseURL, token string, data []byte) mediaResponse {
	t.Helper()
	resp := uploadMediaRequest(t, baseURL, token, data)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusCreated)
	var media mediaResponse
	testutil.DecodeJSON(t, resp, &media)
	if _, err := strconv.ParseInt(media.ID, 10, 64); err != nil {
		t.Fatalf("media ID is not a decimal string: %q", media.ID)
	}
	if media.MimeType != "image/png" {
		t.Fatalf("mimeType = %q", media.MimeType)
	}
	return media
}

func uploadMediaRequest(t *testing.T, baseURL, token string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := testutil.NewRequest(t, http.MethodPost, baseURL+"/community/media", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	testutil.SetBearer(req, token)
	return testutil.Do(t, req)
}

func createPost(t *testing.T, baseURL, token, title, content string, mediaIDs []string) postResponse {
	t.Helper()
	resp := createPostRequest(t, baseURL, token, title, content, mediaIDs)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusCreated)
	var post postResponse
	testutil.DecodeJSON(t, resp, &post)
	if _, err := strconv.ParseInt(post.ID, 10, 64); err != nil {
		t.Fatalf("post ID is not a decimal string: %q", post.ID)
	}
	return post
}

func createPostRequest(t *testing.T, baseURL, token, title, content string, mediaIDs []string) *http.Response {
	t.Helper()
	req := testutil.NewJSONRequest(t, http.MethodPost, baseURL+"/community/posts", map[string]any{"title": title, "content": content, "mediaIds": mediaIDs})
	testutil.SetBearer(req, token)
	return testutil.Do(t, req)
}

func updatePost(t *testing.T, baseURL, token, postID, title, content string, mediaIDs []string) postResponse {
	t.Helper()
	resp := updatePostRequest(t, baseURL, token, postID, title, content, mediaIDs)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var post postResponse
	testutil.DecodeJSON(t, resp, &post)
	return post
}

func updatePostRequest(t *testing.T, baseURL, token, postID, title, content string, mediaIDs []string) *http.Response {
	t.Helper()
	req := testutil.NewJSONRequest(t, http.MethodPatch, baseURL+"/community/posts/"+postID, map[string]any{"title": title, "content": content, "mediaIds": mediaIDs})
	testutil.SetBearer(req, token)
	return testutil.Do(t, req)
}

func getPost(t *testing.T, baseURL, postID, token string) postResponse {
	t.Helper()
	req := testutil.NewRequest(t, http.MethodGet, baseURL+"/community/posts/"+postID, nil)
	if token != "" {
		testutil.SetBearer(req, token)
	}
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var post postResponse
	testutil.DecodeJSON(t, resp, &post)
	return post
}

func listPosts(t *testing.T, target string) []postResponse {
	t.Helper()
	resp, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusOK)
	var page struct {
		Items []postResponse `json:"items"`
	}
	testutil.DecodeJSON(t, resp, &page)
	return page.Items
}

func createComment(t *testing.T, baseURL, token, postID, content string) string {
	t.Helper()
	req := testutil.NewJSONRequest(t, http.MethodPost, baseURL+"/community/posts/"+postID+"/comments", map[string]string{"content": content})
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	defer resp.Body.Close()
	testutil.RequireStatus(t, resp, http.StatusCreated)
	var comment struct {
		ID string `json:"id"`
	}
	testutil.DecodeJSON(t, resp, &comment)
	return comment.ID
}

func requireMediaStatus(t *testing.T, baseURL, mediaID, token string, status int) {
	t.Helper()
	req := testutil.NewRequest(t, http.MethodGet, baseURL+"/community/media/"+mediaID, nil)
	if token != "" {
		testutil.SetBearer(req, token)
	}
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, status)
	resp.Body.Close()
}

func authorizedStatus(t *testing.T, method, target, token string, status int) {
	t.Helper()
	req := testutil.NewRequest(t, method, target, nil)
	testutil.SetBearer(req, token)
	resp := testutil.Do(t, req)
	testutil.RequireStatus(t, resp, status)
	resp.Body.Close()
}

func validPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
