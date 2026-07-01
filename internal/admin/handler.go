package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/market"
	"gorm.io/gorm"
)

// CookieName is the HTTP cookie used for admin panel session.
const CookieName = "lynai_admin_token"

// CSRFCookieName is the HTTP-only cookie carrying the admin CSRF token.
const CSRFCookieName = "lynai_admin_csrf"

var csrfTokenMaxAge = int(auth.RefreshTokenExpiry.Seconds())

// Handler serves the HTML admin panel.
type Handler struct {
	db        *gorm.DB
	authSvc   *auth.Service
	marketSvc *market.Service
	jwtMgr    *auth.JWTManager
	templates *template.Template
}

// NewHandler creates an admin handler. Templates are parsed from the given
// directory at startup.
func NewHandler(db *gorm.DB, authSvc *auth.Service, marketSvc *market.Service, jwtMgr *auth.JWTManager, templateDir string) (*Handler, error) {
	tmpl, err := template.ParseGlob(filepath.Join(templateDir, "*.html"))
	if err != nil {
		return nil, err
	}
	return &Handler{
		db:        db,
		authSvc:   authSvc,
		marketSvc: marketSvc,
		jwtMgr:    jwtMgr,
		templates: tmpl,
	}, nil
}

// RegisterRoutes mounts the admin panel routes on the given engine.
func (h *Handler) RegisterRoutes(r *gin.Engine, jwtMgr *auth.JWTManager) {
	_ = jwtMgr // already stored in h.jwtMgr

	adminGrp := r.Group("/admin")
	{
		adminGrp.GET("/login", h.ShowLogin)
		adminGrp.POST("/login", h.DoLogin)
	}

	protected := adminGrp.Group("")
	protected.Use(h.adminCookieMiddleware(), h.csrfMiddleware())
	{
		protected.GET("/", h.Dashboard)
		protected.POST("/logout", h.DoLogout)
		protected.GET("/pending", h.Pending)
		protected.GET("/users", h.Users)
		protected.POST("/users/create", h.CreateAdminUser)
		protected.POST("/users/:id/promote", h.PromoteUser)
		protected.POST("/users/:id/demote", h.DemoteUser)
		protected.POST("/plugins/:id/approve", h.Approve)
		protected.POST("/plugins/:id/reject", h.Reject)
		protected.GET("/plugins", h.AllPlugins)
		protected.GET("/plugins/:id", h.PluginDetail)
		protected.GET("/plugins/:id/edit", h.EditPluginForm)
		protected.POST("/plugins/:id/edit", h.EditPlugin)
		protected.POST("/plugins/:id/unpublish", h.UnpublishPlugin)
		protected.POST("/plugins/:id/delete", h.DeletePlugin)
	}
}

// adminCookieMiddleware verifies the JWT from the admin cookie.
func (h *Handler) adminCookieMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(CookieName)
		if err != nil || token == "" {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}
		claims, err := h.jwtMgr.Verify(token)
		if err != nil || !claims.IsAdmin {
			setAdminCookie(c, CookieName, "", -1)
			setAdminCookie(c, CSRFCookieName, "", -1)
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}
		if claims.ExpiresAt != nil && time.Until(claims.ExpiresAt.Time) < 7*24*time.Hour {
			_, pair, err := h.authSvc.Refresh(token)
			if err != nil {
				setAdminCookie(c, CookieName, "", -1)
				setAdminCookie(c, CSRFCookieName, "", -1)
				c.Redirect(http.StatusFound, "/admin/login")
				c.Abort()
				return
			}
			setAdminCookie(c, CookieName, pair.RefreshToken, int(auth.RefreshTokenExpiry.Seconds()))
		}
		csrfToken := ""
		if isSafeMethod(c.Request.Method) {
			csrfToken, err = generateCSRFToken()
			if err != nil {
				c.String(http.StatusInternalServerError, "csrf token error")
				c.Abort()
				return
			}
			setAdminCookie(c, CSRFCookieName, csrfToken, csrfTokenMaxAge)
		} else if existing, err := c.Cookie(CSRFCookieName); err == nil {
			csrfToken = existing
		}
		c.Set("csrfToken", csrfToken)
		c.Set("userID", claims.UserID)
		c.Set("username", claims.Username)
		c.Set("isAdmin", claims.IsAdmin)
		c.Next()
	}
}

func (h *Handler) csrfMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if isSafeMethod(c.Request.Method) {
			c.Next()
			return
		}
		cookieToken, err := c.Cookie(CSRFCookieName)
		if err != nil || cookieToken == "" || c.PostForm("_csrf") != cookieToken {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Next()
	}
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// --- Login ---

func (h *Handler) ShowLogin(c *gin.Context) {
	h.render(c, "login.html", nil)
}

func (h *Handler) DoLogin(c *gin.Context) {
	phone := strings.TrimSpace(c.PostForm("phone"))
	password := c.PostForm("password")

	user, pair, err := h.authSvc.Login(phone, password)
	if err != nil || !user.IsAdmin {
		h.render(c, "login.html", map[string]interface{}{"Error": "Invalid phone, password, or not an admin"})
		return
	}

	setAdminCookie(c, CookieName, pair.RefreshToken, int(auth.RefreshTokenExpiry.Seconds()))
	c.Redirect(http.StatusFound, "/admin")
}

func (h *Handler) DoLogout(c *gin.Context) {
	setAdminCookie(c, CookieName, "", -1)
	setAdminCookie(c, CSRFCookieName, "", -1)
	c.Redirect(http.StatusFound, "/admin/login")
}

// --- Dashboard ---

func (h *Handler) Dashboard(c *gin.Context) {
	var pendingCount, approvedCount, userCount int64
	h.db.Model(&database.Plugin{}).Where("status = ?", database.PluginStatusPending).Count(&pendingCount)
	h.db.Model(&database.Plugin{}).Where("status = ?", database.PluginStatusApproved).Count(&approvedCount)
	h.db.Model(&database.User{}).Count(&userCount)

	h.render(c, "dashboard.html", h.pageData(c, "dashboard", map[string]interface{}{
		"PendingCount":  pendingCount,
		"ApprovedCount": approvedCount,
		"UserCount":     userCount,
	}))
}

// --- Pending ---

func (h *Handler) Pending(c *gin.Context) {
	plugins, err := h.marketSvc.ListPending()
	if err != nil {
		h.render(c, "pending.html", h.pageData(c, "pending", map[string]interface{}{
			"Error": "Failed to load pending plugins",
		}))
		return
	}
	h.render(c, "pending.html", h.pageData(c, "pending", map[string]interface{}{"Plugins": plugins}))
}

// --- All Plugins ---

func (h *Handler) AllPlugins(c *gin.Context) {
	var plugins []database.Plugin
	h.db.Order("updated_at DESC").Find(&plugins)
	h.render(c, "plugins.html", h.pageData(c, "plugins", map[string]interface{}{"Plugins": plugins}))
}

func (h *Handler) Users(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	users, total, err := h.authSvc.ListUsers(page, 20)
	data := map[string]interface{}{"Users": users, "Total": total, "Page": page, "Error": c.Query("error")}
	if err != nil {
		data["Error"] = "Failed to load users"
	}
	h.render(c, "users.html", h.pageData(c, "users", data))
}

func (h *Handler) CreateAdminUser(c *gin.Context) {
	phone := strings.TrimSpace(c.PostForm("phone"))
	password := c.PostForm("password")
	displayName := strings.TrimSpace(c.PostForm("displayName"))
	if phone == "" || len(password) < 6 {
		h.redirectUsersWithError(c, "手机号和至少 6 位密码必填")
		return
	}
	if _, err := h.authSvc.CreateAdmin(phone, password, displayName); err != nil {
		if errors.Is(err, auth.ErrPhoneTaken) {
			h.redirectUsersWithError(c, "手机号已注册")
			return
		}
		h.redirectUsersWithError(c, "创建管理员失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/users")
}

func (h *Handler) PromoteUser(c *gin.Context) {
	if err := h.authSvc.SetAdminRole(c.Param("id"), true); err != nil {
		h.redirectUsersWithError(c, "提升管理员失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/users")
}

func (h *Handler) DemoteUser(c *gin.Context) {
	if c.Param("id") == c.GetString("userID") {
		h.redirectUsersWithError(c, "不能取消自己的管理员权限")
		return
	}
	if err := h.authSvc.SetAdminRole(c.Param("id"), false); err != nil {
		h.redirectUsersWithError(c, "取消管理员失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/users")
}

func (h *Handler) PluginDetail(c *gin.Context) {
	plugin, err := h.marketSvc.GetPluginAnyStatus(c.Param("id"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	h.render(c, "plugin_detail.html", h.pageData(c, "plugins", map[string]interface{}{"Plugin": plugin}))
}

func (h *Handler) EditPluginForm(c *gin.Context) {
	plugin, err := h.marketSvc.GetPluginAnyStatus(c.Param("id"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	h.render(c, "plugin_edit.html", h.pageData(c, "plugins", map[string]interface{}{"Plugin": plugin}))
}

func (h *Handler) EditPlugin(c *gin.Context) {
	id := c.Param("id")
	if err := h.marketSvc.UpdatePlugin(
		id,
		strings.TrimSpace(c.PostForm("name")),
		strings.TrimSpace(c.PostForm("description")),
		strings.TrimSpace(c.PostForm("category")),
		strings.TrimSpace(c.PostForm("version")),
	); err != nil {
		c.String(http.StatusInternalServerError, "update failed")
		return
	}
	c.Redirect(http.StatusFound, "/admin/plugins/"+id)
}

func (h *Handler) UnpublishPlugin(c *gin.Context) {
	id := c.Param("id")
	if err := h.marketSvc.Unpublish(id); err != nil {
		c.String(http.StatusInternalServerError, "unpublish failed")
		return
	}
	c.Redirect(http.StatusFound, "/admin/plugins/"+id)
}

func (h *Handler) DeletePlugin(c *gin.Context) {
	if err := h.marketSvc.DeletePlugin(c.Param("id")); err != nil {
		c.String(http.StatusInternalServerError, "delete failed")
		return
	}
	c.Redirect(http.StatusFound, "/admin/plugins")
}

// --- Approve ---

func (h *Handler) Approve(c *gin.Context) {
	id := c.Param("id")
	reviewerID, _ := strconv.ParseInt(c.GetString("userID"), 10, 64)
	_ = h.marketSvc.Approve(id, reviewerID)
	c.Redirect(http.StatusFound, redirectBack(c, "/admin/pending"))
}

// --- Reject ---

func (h *Handler) Reject(c *gin.Context) {
	id := c.Param("id")
	reason := c.PostForm("reason")
	reviewerID, _ := strconv.ParseInt(c.GetString("userID"), 10, 64)
	_ = h.marketSvc.Reject(id, reviewerID, reason)
	c.Redirect(http.StatusFound, redirectBack(c, "/admin/pending"))
}

// render executes the named template with the given data.
func (h *Handler) render(c *gin.Context, name string, data interface{}) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(c.Writer, name, data); err != nil {
		c.String(http.StatusInternalServerError, "template error: %v", err)
	}
}

func (h *Handler) pageData(c *gin.Context, active string, values map[string]interface{}) map[string]interface{} {
	data := map[string]interface{}{
		"Active":    active,
		"Username":  c.GetString("username"),
		"CSRFToken": c.GetString("csrfToken"),
	}
	for k, v := range values {
		data[k] = v
	}
	return data
}

func (h *Handler) redirectUsersWithError(c *gin.Context, message string) {
	c.Redirect(http.StatusFound, "/admin/users?error="+url.QueryEscape(message))
}

func redirectBack(c *gin.Context, fallback string) string {
	if v := c.PostForm("redirect"); strings.HasPrefix(v, "/admin/") {
		return v
	}
	return fallback
}

func setAdminCookie(c *gin.Context, name, value string, maxAge int) {
	c.SetCookie(name, value, maxAge, "/admin", "", c.Request.TLS != nil, true)
}

func generateCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
