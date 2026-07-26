package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/market"
	"github.com/lynai/backend/internal/relay"
	"github.com/lynai/backend/internal/requestbody"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CookieName is the HTTP cookie used for admin panel session.
const CookieName = "lynai_admin_token"

// CSRFCookieName is the HTTP-only cookie carrying the admin CSRF token.
const CSRFCookieName = "lynai_admin_csrf"

const adminLoginBodyLimit = 16 << 10

// Handler serves the HTML admin panel.
type Handler struct {
	db                 *gorm.DB
	relayLogs          *relay.LogService
	authSvc            *auth.Service
	marketSvc          *market.Service
	templates          *template.Template
	endpointPolicy     *relay.EndpointPolicy
	sessions           *sessionService
	sessionTTL         time.Duration
	credentialReleaser CredentialReleaser
}

// CredentialReleaser clears relay runtime cooldown state for a credential.
type CredentialReleaser interface {
	ReleaseCredential(id int64)
}

// NewHandler creates an admin handler using templates embedded in the binary.
func NewHandler(db *gorm.DB, authSvc *auth.Service, marketSvc *market.Service, jwtMgr *auth.JWTManager) (*Handler, error) {
	policy, err := relay.NewEndpointPolicy(nil)
	if err != nil {
		return nil, err
	}
	return NewHandlerWithConfig(db, authSvc, marketSvc, policy, auth.RefreshTokenExpiry)
}

// NewHandlerWithEndpointPolicy creates an admin handler using the relay endpoint policy.
func NewHandlerWithEndpointPolicy(db *gorm.DB, authSvc *auth.Service, marketSvc *market.Service, jwtMgr *auth.JWTManager, policy *relay.EndpointPolicy) (*Handler, error) {
	return NewHandlerWithConfig(db, authSvc, marketSvc, policy, auth.RefreshTokenExpiry)
}

// NewHandlerWithEndpointPolicyAndReleaser creates an admin handler with relay runtime access.
func NewHandlerWithEndpointPolicyAndReleaser(db *gorm.DB, authSvc *auth.Service, marketSvc *market.Service, jwtMgr *auth.JWTManager, policy *relay.EndpointPolicy, releaser CredentialReleaser) (*Handler, error) {
	return NewHandlerWithConfigAndReleaser(db, authSvc, marketSvc, policy, auth.RefreshTokenExpiry, releaser)
}

// NewHandlerWithConfig creates an admin handler with opaque server-side sessions.
func NewHandlerWithConfig(db *gorm.DB, authSvc *auth.Service, marketSvc *market.Service, policy *relay.EndpointPolicy, sessionTTL time.Duration) (*Handler, error) {
	return NewHandlerWithConfigAndReleaser(db, authSvc, marketSvc, policy, sessionTTL, nil)
}

// NewHandlerWithConfigAndReleaser creates an admin handler with relay runtime access.
func NewHandlerWithConfigAndReleaser(db *gorm.DB, authSvc *auth.Service, marketSvc *market.Service, policy *relay.EndpointPolicy, sessionTTL time.Duration, releaser CredentialReleaser) (*Handler, error) {
	tmpl, err := parseAdminTemplates()
	if err != nil {
		return nil, err
	}
	return &Handler{
		db:                 db,
		relayLogs:          relay.NewLogService(db),
		authSvc:            authSvc,
		marketSvc:          marketSvc,
		templates:          tmpl,
		endpointPolicy:     policy,
		sessions:           newSessionService(db, sessionTTL),
		sessionTTL:         sessionTTL,
		credentialReleaser: releaser,
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
		protected.GET("/relay", h.RelayProviders)
		protected.GET("/relay/dashboard", h.RelayDashboard)
		protected.GET("/relay/logs", h.RelayLogs)
		protected.GET("/relay/new", h.NewRelayProviderForm)
		protected.POST("/relay/new", h.CreateRelayProvider)
		protected.GET("/relay/:id/edit", h.EditRelayProviderForm)
		protected.POST("/relay/:id/edit", h.UpdateRelayProvider)
		protected.POST("/relay/:id/toggle", h.ToggleRelayProvider)
		protected.POST("/relay/:id/delete", h.DeleteRelayProvider)
		protected.GET("/relay/:id/credentials", h.RelayCredentials)
		protected.GET("/relay/:id/credentials/new", h.NewRelayCredentialForm)
		protected.POST("/relay/:id/credentials/new", h.CreateRelayCredential)
		protected.GET("/relay/credentials/:id/edit", h.EditRelayCredentialForm)
		protected.POST("/relay/credentials/:id/edit", h.UpdateRelayCredential)
		protected.POST("/relay/credentials/:id/toggle", h.ToggleRelayCredential)
		protected.POST("/relay/credentials/:id/delete", h.DeleteRelayCredential)
		protected.POST("/relay/credentials/:id/release", h.ReleaseRelayCredential)
		protected.GET("/relay/models", h.RelayModels)
		protected.GET("/relay/models/new", h.NewRelayModelForm)
		protected.POST("/relay/models/new", h.CreateRelayModel)
		protected.GET("/relay/models/:id/edit", h.EditRelayModelForm)
		protected.POST("/relay/models/:id/edit", h.UpdateRelayModel)
		protected.POST("/relay/models/:id/toggle", h.ToggleRelayModel)
		protected.POST("/relay/models/:id/delete", h.DeleteRelayModel)
		protected.POST("/relay/models/:id/bindings", h.CreateRelayBinding)
		protected.GET("/relay/bindings/:id/edit", h.EditRelayBindingForm)
		protected.POST("/relay/bindings/:id/edit", h.UpdateRelayBinding)
		protected.POST("/relay/bindings/:id/toggle", h.ToggleRelayBinding)
		protected.POST("/relay/bindings/:id/delete", h.DeleteRelayBinding)
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
		user, renewed, err := h.sessions.authenticate(token)
		if err != nil {
			setAdminCookie(c, CookieName, "", -1)
			setAdminCookie(c, CSRFCookieName, "", -1)
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}
		if renewed {
			setAdminCookie(c, CookieName, token, int(h.sessionTTL.Seconds()))
		}
		csrfToken := ""
		if isSafeMethod(c.Request.Method) {
			csrfToken, err = generateCSRFToken()
			if err != nil {
				c.String(http.StatusInternalServerError, "csrf token error")
				c.Abort()
				return
			}
			setAdminCookie(c, CSRFCookieName, csrfToken, int(h.sessionTTL.Seconds()))
		} else if existing, err := c.Cookie(CSRFCookieName); err == nil {
			csrfToken = existing
		}
		c.Set("csrfToken", csrfToken)
		c.Set("userID", userIDString(user))
		c.Set("username", user.DisplayName)
		c.Set("isAdmin", user.IsAdmin)
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
	requestbody.Limit(c, adminLoginBodyLimit)
	if err := c.Request.ParseForm(); err != nil {
		if requestbody.TooLarge(err) {
			c.String(http.StatusRequestEntityTooLarge, "Request body is too large")
			return
		}
		h.render(c, "login.html", map[string]interface{}{"Error": "Invalid login request"})
		return
	}
	phone := strings.TrimSpace(c.PostForm("phone"))
	password := c.PostForm("password")

	user, err := h.authSvc.AuthenticatePassword(phone, password)
	if err != nil {
		h.render(c, "login.html", map[string]interface{}{"Error": "Invalid phone, password, or not an admin"})
		return
	}
	if !user.IsAdmin {
		h.render(c, "login.html", map[string]interface{}{"Error": "Invalid phone, password, or not an admin"})
		return
	}
	token, err := h.sessions.create(user.ID)
	if err != nil {
		h.render(c, "login.html", map[string]interface{}{"Error": "Failed to create admin session"})
		return
	}
	setAdminCookie(c, CookieName, token, int(h.sessionTTL.Seconds()))
	c.Redirect(http.StatusFound, "/admin")
}

func (h *Handler) DoLogout(c *gin.Context) {
	token, _ := c.Cookie(CookieName)
	_ = h.sessions.revoke(token)
	setAdminCookie(c, CookieName, "", -1)
	setAdminCookie(c, CSRFCookieName, "", -1)
	c.Redirect(http.StatusFound, "/admin/login")
}

// Close stops resources owned by the admin handler.
func (h *Handler) Close() {
	h.relayLogs.Close()
}

// DeleteExpiredSessions removes expired administrator browser sessions.
func (h *Handler) DeleteExpiredSessions(now time.Time) error {
	return h.sessions.deleteExpired(now)
}

// --- Dashboard ---

func (h *Handler) Dashboard(c *gin.Context) {
	var pendingCount, approvedCount, userCount int64
	h.db.Model(&database.Plugin{}).Where("status = ?", database.PluginStatusPending).Count(&pendingCount)
	h.db.Model(&database.Plugin{}).Where("status = ?", database.PluginStatusApproved).Count(&approvedCount)
	h.db.Model(&database.User{}).Count(&userCount)

	relayToday, todayErr := h.relayLogs.Summary("today", time.Now())
	relaySevenDays, sevenDayErr := h.relayLogs.Summary("7d", time.Now())
	relayError := ""
	if todayErr != nil || sevenDayErr != nil {
		relayError = "中转调用统计暂不可用"
	}
	h.render(c, "dashboard.html", h.pageData(c, "dashboard", map[string]interface{}{
		"PendingCount":   pendingCount,
		"ApprovedCount":  approvedCount,
		"UserCount":      userCount,
		"RelayToday":     relayToday,
		"RelaySevenDays": relaySevenDays,
		"RelayError":     relayError,
	}))
}

func (h *Handler) RelayDashboard(c *gin.Context) {
	dashboard, err := h.relayLogs.Dashboard(c.DefaultQuery("range", "7d"), time.Now())
	data := map[string]interface{}{"Dashboard": dashboard}
	if err != nil {
		data["Error"] = "调用统计加载失败"
	}
	h.render(c, "relay_dashboard.html", h.pageData(c, "relay_dashboard", data))
}

func (h *Handler) RelayLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	filter := relay.LogFilter{
		Range: c.DefaultQuery("range", "7d"), UserID: c.Query("userId"), Username: c.Query("username"),
		Provider: c.Query("provider"), APIType: c.Query("apiType"), ModelID: c.Query("model"),
		Operation: c.Query("operation"), Protocol: c.Query("protocol"), Result: c.Query("result"), Page: page,
	}
	logs, err := h.relayLogs.List(filter, time.Now())
	data := map[string]interface{}{"LogPage": logs, "Filter": filter}
	if err != nil {
		data["Error"] = "调用日志加载失败"
	}
	h.render(c, "relay_logs.html", h.pageData(c, "relay_logs", data))
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
	if _, err := h.authSvc.CreateAdmin(c.Request.Context(), phone, password, displayName); err != nil {
		if errors.Is(err, auth.ErrPhoneTaken) {
			h.redirectUsersWithError(c, "手机号已注册")
			return
		}
		if errors.Is(err, database.ErrSnowflakeUnavailable) {
			c.String(http.StatusServiceUnavailable, "Service temporarily unavailable")
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

// --- Relay providers ---

type relayProviderView struct {
	Provider               database.RelayProvider
	CredentialCount        int
	EnabledCredentialCount int
	BindingCount           int
}

type relayModelView struct {
	Model        database.RelayModel
	BindingCount int
}

func (h *Handler) RelayProviders(c *gin.Context) {
	var providers []database.RelayProvider
	h.db.Preload("Credentials").Preload("Bindings").Order("updated_at DESC").Find(&providers)
	views := make([]relayProviderView, 0, len(providers))
	for _, provider := range providers {
		enabled := 0
		for _, credential := range provider.Credentials {
			if credential.Enabled {
				enabled++
			}
		}
		views = append(views, relayProviderView{Provider: provider, CredentialCount: len(provider.Credentials), EnabledCredentialCount: enabled, BindingCount: len(provider.Bindings)})
	}
	h.render(c, "relay.html", h.pageData(c, "relay", map[string]interface{}{"Providers": views, "Error": c.Query("error")}))
}

func (h *Handler) NewRelayProviderForm(c *gin.Context) {
	h.render(c, "relay_edit.html", h.pageData(c, "relay", map[string]interface{}{
		"Title":            "新增中转上游",
		"Action":           "/admin/relay/new",
		"APIFormat":        "openai",
		"Enabled":          true,
		"ClientVersion":    "1.0.0",
		"Package":          "lynai",
		"OCRPos":           "2",
		"BusinessIDPrefix": "aigc",
		"ImageModule":      "aigc",
		"Error":            c.Query("error"),
	}))
}

func (h *Handler) CreateRelayProvider(c *gin.Context) {
	apiFormat, err := parseRelayAPIFormat(c.PostForm("apiFormat"))
	if err != nil {
		h.redirectRelayNewWithError(c, err.Error())
		return
	}
	config := parseRelayProviderConfig(c)
	if err := h.validateRelayProviderForm(apiFormat, strings.TrimSpace(c.PostForm("endpoint")), config); err != nil {
		h.redirectRelayNewWithError(c, err.Error())
		return
	}
	provider := database.RelayProvider{
		Name:      strings.TrimSpace(c.PostForm("name")),
		Endpoint:  strings.TrimRight(strings.TrimSpace(c.PostForm("endpoint")), "/"),
		APIFormat: apiFormat,
		Config:    relay.EncodeProviderConfig(config),
		Enabled:   c.PostForm("enabled") == "on",
	}
	if provider.Name == "" {
		h.redirectRelayNewWithError(c, "名称必填")
		return
	}
	if err := h.db.Create(&provider).Error; err != nil {
		h.redirectRelayNewWithError(c, "创建中转上游失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay")
}

func (h *Handler) EditRelayProviderForm(c *gin.Context) {
	provider, ok := h.loadRelayProvider(c)
	if !ok {
		return
	}
	config := relay.DecodeProviderConfig(provider.Config)
	h.render(c, "relay_edit.html", h.pageData(c, "relay", map[string]interface{}{
		"Title":            "编辑中转上游",
		"Action":           "/admin/relay/" + c.Param("id") + "/edit",
		"Provider":         provider,
		"Name":             provider.Name,
		"Endpoint":         provider.Endpoint,
		"APIFormat":        provider.APIFormat,
		"Enabled":          provider.Enabled,
		"AppID":            config.AppID,
		"ClientVersion":    defaultString(config.ClientVersion, "1.0.0"),
		"Package":          defaultString(config.Package, "lynai"),
		"OCRPos":           defaultString(config.OCRPos, "2"),
		"BusinessIDPrefix": defaultString(config.BusinessIDPrefix, "aigc"),
		"ImageModule":      defaultString(config.ImageModule, "aigc"),
		"Error":            c.Query("error"),
	}))
}

func (h *Handler) UpdateRelayProvider(c *gin.Context) {
	provider, ok := h.loadRelayProvider(c)
	if !ok {
		return
	}
	apiFormat, err := parseRelayAPIFormat(c.PostForm("apiFormat"))
	if err != nil {
		h.redirectRelayEditWithError(c, err.Error())
		return
	}
	config := parseRelayProviderConfig(c)
	provider.Name = strings.TrimSpace(c.PostForm("name"))
	provider.Endpoint = strings.TrimRight(strings.TrimSpace(c.PostForm("endpoint")), "/")
	provider.APIFormat = apiFormat
	provider.Enabled = c.PostForm("enabled") == "on"
	provider.Config = relay.EncodeProviderConfig(config)
	if provider.Name == "" {
		h.redirectRelayEditWithError(c, "名称必填")
		return
	}
	if err := h.validateRelayProviderForm(apiFormat, provider.Endpoint, config); err != nil {
		h.redirectRelayEditWithError(c, err.Error())
		return
	}
	if err := h.validateProviderBindings(provider.ID, apiFormat); err != nil {
		h.redirectRelayEditWithError(c, err.Error())
		return
	}
	if err := h.db.Save(&provider).Error; err != nil {
		h.redirectRelayEditWithError(c, "保存中转上游失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay")
}

func (h *Handler) ToggleRelayProvider(c *gin.Context) {
	provider, ok := h.loadRelayProvider(c)
	if !ok {
		return
	}
	provider.Enabled = !provider.Enabled
	if err := h.db.Save(&provider).Error; err != nil {
		h.redirectRelayWithError(c, "切换中转上游失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay")
}

func (h *Handler) DeleteRelayProvider(c *gin.Context) {
	provider, ok := h.loadRelayProvider(c)
	if !ok {
		return
	}
	if h.hasActiveSpeechSession("provider_id = ?", provider.ID) {
		h.redirectRelayWithError(c, "该上游仍被有效语音会话使用，暂不能删除")
		return
	}
	if err := h.db.Delete(&provider).Error; err != nil {
		h.redirectRelayWithError(c, "删除中转上游失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay")
}

func (h *Handler) loadRelayProvider(c *gin.Context) (database.RelayProvider, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.Status(http.StatusNotFound)
		return database.RelayProvider{}, false
	}
	var provider database.RelayProvider
	if err := h.db.First(&provider, "id = ?", id).Error; err != nil {
		c.Status(http.StatusNotFound)
		return database.RelayProvider{}, false
	}
	return provider, true
}

func parseAdvancedParams(c *gin.Context) (relay.ModelAdvancedParams, error) {
	var params relay.ModelAdvancedParams
	if v := strings.TrimSpace(c.PostForm("maxTokens")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return params, err
		}
		params.MaxTokens = &parsed
	}
	if v, err := parseOptionalFloat(c.PostForm("temperature")); err != nil {
		return params, err
	} else {
		params.Temperature = v
	}
	if v, err := parseOptionalFloat(c.PostForm("topP")); err != nil {
		return params, err
	} else {
		params.TopP = v
	}
	if v, err := parseOptionalFloat(c.PostForm("presencePenalty")); err != nil {
		return params, err
	} else {
		params.PresencePenalty = v
	}
	if v, err := parseOptionalFloat(c.PostForm("frequencyPenalty")); err != nil {
		return params, err
	} else {
		params.FrequencyPenalty = v
	}
	if v := strings.TrimSpace(c.PostForm("seed")); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return params, err
		}
		params.Seed = &parsed
	}
	if v := strings.TrimSpace(c.PostForm("stop")); v != "" {
		for _, stop := range strings.Split(v, "\n") {
			if stop = strings.TrimSpace(stop); stop != "" {
				params.Stop = append(params.Stop, stop)
			}
		}
	}
	if v := strings.TrimSpace(c.PostForm("user")); v != "" {
		params.User = &v
	}
	params.DebugSSE = c.PostForm("debugSse") == "on"
	return params, nil
}

func validateRelayModel(model database.RelayModel) error {
	params := relay.DecodeAdvancedParams(model.AdvancedParams)
	if params.MaxTokens != nil && *params.MaxTokens <= 0 {
		return adminFormError("Max Tokens 必须大于 0")
	}
	if params.TopP != nil && (*params.TopP < 0 || *params.TopP > 1) {
		return adminFormError("Top P 必须在 0 到 1 之间")
	}
	return nil
}

func parseRelayProviderConfig(c *gin.Context) relay.ProviderConfig {
	return relay.ProviderConfig{
		AppID:            strings.TrimSpace(c.PostForm("appId")),
		ClientVersion:    strings.TrimSpace(c.PostForm("clientVersion")),
		Package:          strings.TrimSpace(c.PostForm("package")),
		OCRPos:           strings.TrimSpace(c.PostForm("ocrPos")),
		BusinessIDPrefix: strings.TrimSpace(c.PostForm("businessIdPrefix")),
		ImageModule:      strings.TrimSpace(c.PostForm("imageModule")),
	}
}

func (h *Handler) validateRelayProviderForm(apiFormat, endpoint string, config relay.ProviderConfig) error {
	if h.endpointPolicy == nil {
		return adminFormError("Endpoint 安全策略未配置")
	}
	if err := h.endpointPolicy.ValidateEndpoint(endpoint); err != nil {
		return adminFormError("Endpoint 不安全: " + err.Error())
	}
	if isVivoAppIDAPI(apiFormat) && strings.TrimSpace(config.AppID) == "" {
		return errors.New("VIVO OCR/LASR 必须填写 AppID")
	}
	return nil
}

func adminFormError(message string) error {
	return errors.New(message)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func isVivoAppIDAPI(apiFormat string) bool {
	switch strings.ToLower(strings.TrimSpace(apiFormat)) {
	case relay.APIFormatVivoOCR, relay.APIFormatVivoLASR:
		return true
	default:
		return false
	}
}

func parseOptionalFloat(raw string) (*float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func intPtrString(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func floatPtrString(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func stringPtrString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func parseRelayAPIFormat(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return relay.APIFormatOpenAI, nil
	}
	if !relay.IsSupportedAPIFormat(value) {
		return "", errors.New("不支持的 API Type")
	}
	return value, nil
}

// --- Relay credentials ---

func (h *Handler) RelayCredentials(c *gin.Context) {
	provider, ok := h.loadRelayProviderParam(c, "id")
	if !ok {
		return
	}
	var credentials []database.RelayProviderCredential
	h.db.Where("provider_id = ?", provider.ID).Order("priority DESC, id ASC").Find(&credentials)
	h.render(c, "relay_credentials.html", h.pageData(c, "relay", map[string]interface{}{
		"Provider": provider, "Credentials": credentials, "Error": c.Query("error"), "Message": c.Query("message"),
	}))
}

func (h *Handler) NewRelayCredentialForm(c *gin.Context) {
	provider, ok := h.loadRelayProviderParam(c, "id")
	if !ok {
		return
	}
	h.renderRelayCredentialForm(c, provider, database.RelayProviderCredential{Enabled: true}, "新增凭据", "/admin/relay/"+strconv.FormatInt(provider.ID, 10)+"/credentials/new", false)
}

func (h *Handler) CreateRelayCredential(c *gin.Context) {
	provider, ok := h.loadRelayProviderParam(c, "id")
	if !ok {
		return
	}
	credential, err := h.relayCredentialFromForm(c, provider, database.RelayProviderCredential{})
	if err != nil {
		h.redirectCredentialsWithError(c, provider.ID, "new", err.Error())
		return
	}
	credential.ProviderID = provider.ID
	if err := h.db.Create(&credential).Error; err != nil {
		h.redirectCredentialsWithError(c, provider.ID, "new", "创建凭据失败")
		return
	}
	c.Redirect(http.StatusFound, h.credentialsURL(provider.ID))
}

func (h *Handler) EditRelayCredentialForm(c *gin.Context) {
	credential, provider, ok := h.loadRelayCredential(c)
	if !ok {
		return
	}
	h.renderRelayCredentialForm(c, provider, credential, "编辑凭据", "/admin/relay/credentials/"+strconv.FormatInt(credential.ID, 10)+"/edit", true)
}

func (h *Handler) UpdateRelayCredential(c *gin.Context) {
	credential, provider, ok := h.loadRelayCredential(c)
	if !ok {
		return
	}
	var validationErr error
	activeSession := false
	err := h.db.Transaction(func(tx *gorm.DB) error {
		query := tx
		if tx.Dialector.Name() == "postgres" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var locked database.RelayProviderCredential
		if err := query.First(&locked, "id = ?", credential.ID).Error; err != nil {
			return err
		}
		updated, err := h.relayCredentialFromForm(c, provider, locked)
		if err != nil {
			validationErr = err
			return nil
		}
		if updated.APIKey != locked.APIKey {
			var count int64
			if err := tx.Model(&database.RelaySpeechSession{}).
				Where("credential_id = ? AND expires_at > ?", locked.ID, time.Now()).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				activeSession = true
				return nil
			}
		}
		return tx.Save(&updated).Error
	})
	if validationErr != nil {
		h.redirectCredentialEditWithError(c, credential.ID, validationErr.Error())
		return
	}
	if activeSession {
		h.redirectCredentialEditWithError(c, credential.ID, "该凭据仍被有效语音会话使用，暂不能替换 API Key")
		return
	}
	if err != nil {
		h.redirectCredentialEditWithError(c, credential.ID, "保存凭据失败")
		return
	}
	c.Redirect(http.StatusFound, h.credentialsURL(provider.ID))
}

func (h *Handler) ToggleRelayCredential(c *gin.Context) {
	credential, provider, ok := h.loadRelayCredential(c)
	if !ok {
		return
	}
	credential.Enabled = !credential.Enabled
	if err := h.db.Save(&credential).Error; err != nil {
		h.redirectCredentialListWithError(c, provider.ID, "切换凭据失败")
		return
	}
	c.Redirect(http.StatusFound, h.credentialsURL(provider.ID))
}

func (h *Handler) DeleteRelayCredential(c *gin.Context) {
	credential, provider, ok := h.loadRelayCredential(c)
	if !ok {
		return
	}
	if h.hasActiveSpeechSession("credential_id = ?", credential.ID) {
		h.redirectCredentialListWithError(c, provider.ID, "该凭据仍被有效语音会话使用，暂不能删除")
		return
	}
	if err := h.db.Delete(&credential).Error; err != nil {
		h.redirectCredentialListWithError(c, provider.ID, "删除凭据失败")
		return
	}
	c.Redirect(http.StatusFound, h.credentialsURL(provider.ID))
}

func (h *Handler) ReleaseRelayCredential(c *gin.Context) {
	credential, provider, ok := h.loadRelayCredential(c)
	if !ok {
		return
	}
	if h.credentialReleaser != nil {
		h.credentialReleaser.ReleaseCredential(credential.ID)
	}
	c.Redirect(http.StatusFound, h.credentialsURL(provider.ID)+"?message="+url.QueryEscape("凭据冷却状态已释放"))
}

func (h *Handler) renderRelayCredentialForm(c *gin.Context, provider database.RelayProvider, credential database.RelayProviderCredential, title, action string, editing bool) {
	config := relay.DecodeProviderConfig(credential.Config)
	h.render(c, "relay_credentials_edit.html", h.pageData(c, "relay", map[string]interface{}{
		"Provider": provider, "Credential": credential, "Title": title, "Action": action, "Editing": editing,
		"Name": credential.Name, "Priority": credential.Priority, "Enabled": credential.Enabled, "AppID": config.AppID,
		"ClientVersion": config.ClientVersion, "Package": config.Package, "OCRPos": config.OCRPos,
		"BusinessIDPrefix": config.BusinessIDPrefix, "ImageModule": config.ImageModule, "Error": c.Query("error"),
	}))
}

func (h *Handler) relayCredentialFromForm(c *gin.Context, provider database.RelayProvider, credential database.RelayProviderCredential) (database.RelayProviderCredential, error) {
	credential.Name = strings.TrimSpace(c.PostForm("name"))
	if credential.Name == "" {
		return credential, errors.New("名称必填")
	}
	priority, err := strconv.Atoi(strings.TrimSpace(c.PostForm("priority")))
	if err != nil {
		return credential, errors.New("优先级必须是整数")
	}
	credential.Priority = priority
	credential.Enabled = c.PostForm("enabled") == "on"
	credential.Config = relay.EncodeProviderConfig(parseRelayProviderConfig(c))
	if key := strings.TrimSpace(c.PostForm("apiKey")); key != "" {
		credential.APIKey = key
	}
	if provider.APIFormat != relay.APIFormatOllama && credential.APIKey == "" {
		return credential, errors.New("API Key 必填")
	}
	merged := relay.MergeProviderConfig(relay.DecodeProviderConfig(provider.Config), relay.DecodeProviderConfig(credential.Config))
	if isVivoAppIDAPI(provider.APIFormat) && merged.AppID == "" {
		return credential, errors.New("VIVO OCR/LASR 必须填写 AppID")
	}
	return credential, nil
}

func (h *Handler) loadRelayCredential(c *gin.Context) (database.RelayProviderCredential, database.RelayProvider, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.Status(http.StatusNotFound)
		return database.RelayProviderCredential{}, database.RelayProvider{}, false
	}
	var credential database.RelayProviderCredential
	if err := h.db.First(&credential, "id = ?", id).Error; err != nil {
		c.Status(http.StatusNotFound)
		return credential, database.RelayProvider{}, false
	}
	var provider database.RelayProvider
	if err := h.db.First(&provider, "id = ?", credential.ProviderID).Error; err != nil {
		c.Status(http.StatusNotFound)
		return credential, provider, false
	}
	return credential, provider, true
}

// --- Global relay models and bindings ---

func (h *Handler) RelayModels(c *gin.Context) {
	var models []database.RelayModel
	h.db.Preload("Bindings").Order("updated_at DESC").Find(&models)
	views := make([]relayModelView, 0, len(models))
	for _, model := range models {
		views = append(views, relayModelView{Model: model, BindingCount: len(model.Bindings)})
	}
	h.render(c, "relay_models.html", h.pageData(c, "relay_models", map[string]interface{}{"Models": views, "Error": c.Query("error")}))
}

func (h *Handler) NewRelayModelForm(c *gin.Context) {
	h.renderRelayModelForm(c, database.RelayModel{Category: relay.CategoryChat, Enabled: true}, "新增全局模型", "/admin/relay/models/new")
}

func (h *Handler) CreateRelayModel(c *gin.Context) {
	model, err := relayModelFromForm(c, database.RelayModel{})
	if err != nil {
		h.redirectRelayModelNewWithError(c, err.Error())
		return
	}
	if err := h.db.Create(&model).Error; err != nil {
		h.redirectRelayModelNewWithError(c, "模型 ID 已存在或保存失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models/"+strconv.FormatInt(model.ID, 10)+"/edit")
}

func (h *Handler) EditRelayModelForm(c *gin.Context) {
	model, ok := h.loadRelayModel(c)
	if !ok {
		return
	}
	h.renderRelayModelForm(c, model, "编辑全局模型", "/admin/relay/models/"+strconv.FormatInt(model.ID, 10)+"/edit")
}

func (h *Handler) UpdateRelayModel(c *gin.Context) {
	model, ok := h.loadRelayModel(c)
	if !ok {
		return
	}
	updated, err := relayModelFromForm(c, model)
	if err != nil {
		h.redirectRelayModelEditWithError(c, model.ID, err.Error())
		return
	}
	if err := h.validateExistingBindings(updated); err != nil {
		h.redirectRelayModelEditWithError(c, model.ID, err.Error())
		return
	}
	if err := h.db.Save(&updated).Error; err != nil {
		h.redirectRelayModelEditWithError(c, model.ID, "模型 ID 已存在或保存失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models/"+strconv.FormatInt(model.ID, 10)+"/edit")
}

func (h *Handler) ToggleRelayModel(c *gin.Context) {
	model, ok := h.loadRelayModel(c)
	if !ok {
		return
	}
	model.Enabled = !model.Enabled
	if err := h.db.Save(&model).Error; err != nil {
		h.redirectRelayModelsWithError(c, "切换模型失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models")
}

func (h *Handler) DeleteRelayModel(c *gin.Context) {
	model, ok := h.loadRelayModel(c)
	if !ok {
		return
	}
	if h.hasActiveSpeechSession("model_id = ?", model.ModelID) {
		h.redirectRelayModelsWithError(c, "该模型仍被有效语音会话使用，暂不能删除")
		return
	}
	if err := h.db.Delete(&model).Error; err != nil {
		h.redirectRelayModelsWithError(c, "删除模型失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models")
}

func (h *Handler) CreateRelayBinding(c *gin.Context) {
	model, ok := h.loadRelayModel(c)
	if !ok {
		return
	}
	binding, err := h.relayBindingFromForm(c, model, database.RelayModelBinding{})
	if err != nil {
		h.redirectRelayModelEditWithError(c, model.ID, err.Error())
		return
	}
	binding.RelayModelID = model.ID
	if err := h.db.Create(&binding).Error; err != nil {
		h.redirectRelayModelEditWithError(c, model.ID, "该模型已绑定此上游或保存失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models/"+strconv.FormatInt(model.ID, 10)+"/edit")
}

func (h *Handler) EditRelayBindingForm(c *gin.Context) {
	binding, model, ok := h.loadRelayBinding(c)
	if !ok {
		return
	}
	var providers []database.RelayProvider
	h.db.Order("name ASC").Find(&providers)
	h.render(c, "relay_binding_edit.html", h.pageData(c, "relay_models", map[string]interface{}{
		"Binding": binding, "Model": model, "Providers": providers, "Error": c.Query("error"),
	}))
}

func (h *Handler) UpdateRelayBinding(c *gin.Context) {
	binding, model, ok := h.loadRelayBinding(c)
	if !ok {
		return
	}
	updated, err := h.relayBindingFromForm(c, model, binding)
	if err != nil {
		h.redirectRelayBindingEditWithError(c, binding.ID, err.Error())
		return
	}
	if err := h.db.Save(&updated).Error; err != nil {
		h.redirectRelayBindingEditWithError(c, binding.ID, "该模型已绑定此上游或保存失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models/"+strconv.FormatInt(model.ID, 10)+"/edit")
}

func (h *Handler) ToggleRelayBinding(c *gin.Context) {
	binding, model, ok := h.loadRelayBinding(c)
	if !ok {
		return
	}
	binding.Enabled = !binding.Enabled
	if err := h.db.Save(&binding).Error; err != nil {
		h.redirectRelayModelEditWithError(c, model.ID, "切换绑定失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models/"+strconv.FormatInt(model.ID, 10)+"/edit")
}

func (h *Handler) DeleteRelayBinding(c *gin.Context) {
	binding, model, ok := h.loadRelayBinding(c)
	if !ok {
		return
	}
	if h.hasActiveSpeechSession("binding_id = ?", binding.ID) {
		h.redirectRelayModelEditWithError(c, model.ID, "该绑定仍被有效语音会话使用，暂不能删除")
		return
	}
	if err := h.db.Delete(&binding).Error; err != nil {
		h.redirectRelayModelEditWithError(c, model.ID, "删除绑定失败")
		return
	}
	c.Redirect(http.StatusFound, "/admin/relay/models/"+strconv.FormatInt(model.ID, 10)+"/edit")
}

func (h *Handler) renderRelayModelForm(c *gin.Context, model database.RelayModel, title, action string) {
	capabilities := relay.DecodeCapabilities(model.Capabilities)
	params := relay.DecodeAdvancedParams(model.AdvancedParams)
	var bindings []database.RelayModelBinding
	if model.ID != 0 {
		h.db.Where("relay_model_id = ?", model.ID).Order("id ASC").Find(&bindings)
	}
	type bindingView struct {
		Binding  database.RelayModelBinding
		Provider database.RelayProvider
	}
	views := make([]bindingView, 0, len(bindings))
	for _, binding := range bindings {
		var provider database.RelayProvider
		h.db.First(&provider, "id = ?", binding.ProviderID)
		views = append(views, bindingView{Binding: binding, Provider: provider})
	}
	var providers []database.RelayProvider
	h.db.Order("name ASC").Find(&providers)
	h.render(c, "relay_models_edit.html", h.pageData(c, "relay_models", map[string]interface{}{
		"Title": title, "Action": action, "Model": model, "Bindings": views, "Providers": providers, "Error": c.Query("error"),
		"SupportsVision": capabilities.Vision, "SupportsThinking": capabilities.Thinking, "SupportsTools": capabilities.Tools,
		"MaxTokens": intPtrString(params.MaxTokens), "Temperature": floatPtrString(params.Temperature), "TopP": floatPtrString(params.TopP),
		"PresencePenalty": floatPtrString(params.PresencePenalty), "FrequencyPenalty": floatPtrString(params.FrequencyPenalty),
		"Seed": intPtrString(params.Seed), "Stop": strings.Join(params.Stop, "\n"), "User": stringPtrString(params.User), "DebugSSE": params.DebugSSE,
	}))
}

func relayModelFromForm(c *gin.Context, model database.RelayModel) (database.RelayModel, error) {
	model.ModelID = strings.TrimSpace(c.PostForm("modelId"))
	if model.ModelID == "" {
		return model, errors.New("模型 ID 必填")
	}
	model.DisplayName = strings.TrimSpace(c.PostForm("displayName"))
	model.Description = strings.TrimSpace(c.PostForm("description"))
	model.Category = relay.NormalizeCategory(c.PostForm("category"))
	model.Capabilities = relay.EncodeCapabilities(relay.ModelCapabilities{Vision: c.PostForm("supportsVision") == "on", Thinking: c.PostForm("supportsThinking") == "on", Tools: c.PostForm("supportsTools") == "on"})
	params, err := parseAdvancedParams(c)
	if err != nil {
		return model, errors.New("高级参数格式错误")
	}
	model.AdvancedParams = relay.EncodeAdvancedParams(params)
	model.Enabled = c.PostForm("enabled") == "on"
	if err := validateRelayModel(model); err != nil {
		return model, err
	}
	return model, nil
}

func (h *Handler) relayBindingFromForm(c *gin.Context, model database.RelayModel, binding database.RelayModelBinding) (database.RelayModelBinding, error) {
	providerID, err := strconv.ParseInt(strings.TrimSpace(c.PostForm("providerId")), 10, 64)
	if err != nil {
		return binding, errors.New("请选择上游")
	}
	var provider database.RelayProvider
	if err := h.db.First(&provider, "id = ?", providerID).Error; err != nil {
		return binding, errors.New("上游不存在")
	}
	if !relay.SupportsCategory(provider.APIFormat, model.Category) {
		return binding, errors.New("API Type 与模型分类不匹配")
	}
	weight, err := strconv.Atoi(strings.TrimSpace(c.PostForm("weight")))
	if err != nil || weight < 1 {
		return binding, errors.New("权重必须是大于 0 的整数")
	}
	binding.ProviderID = provider.ID
	binding.UpstreamModel = strings.TrimSpace(c.PostForm("upstreamModel"))
	if binding.UpstreamModel == "" {
		return binding, errors.New("上游模型必填")
	}
	binding.Weight = weight
	binding.Enabled = c.PostForm("enabled") == "on"
	if err := h.validateSpeechBindingMix(model.ID, binding.ID, provider.APIFormat); err != nil {
		return binding, err
	}
	return binding, nil
}

func (h *Handler) validateSpeechBindingMix(modelID, excludeBindingID int64, apiFormat string) error {
	if apiFormat != relay.APIFormatOpenAISpeech && apiFormat != relay.APIFormatVivoLASR {
		return nil
	}
	var formats []string
	query := h.db.Table("relay_model_bindings b").Select("p.api_format").Joins("JOIN relay_providers p ON p.id = b.provider_id").Where("b.relay_model_id = ?", modelID)
	if excludeBindingID != 0 {
		query = query.Where("b.id <> ?", excludeBindingID)
	}
	if err := query.Pluck("p.api_format", &formats).Error; err != nil {
		return errors.New("检查语音绑定失败")
	}
	for _, existing := range formats {
		if (apiFormat == relay.APIFormatOpenAISpeech && existing == relay.APIFormatVivoLASR) || (apiFormat == relay.APIFormatVivoLASR && existing == relay.APIFormatOpenAISpeech) {
			return errors.New("同一语音模型不能混用 openai_speech 与 vivo_lasr")
		}
	}
	return nil
}

func (h *Handler) validateExistingBindings(model database.RelayModel) error {
	var providers []database.RelayProvider
	if err := h.db.Table("relay_providers p").Joins("JOIN relay_model_bindings b ON b.provider_id = p.id").Where("b.relay_model_id = ?", model.ID).Find(&providers).Error; err != nil {
		return errors.New("检查模型绑定失败")
	}
	for _, provider := range providers {
		if !relay.SupportsCategory(provider.APIFormat, model.Category) {
			return errors.New("现有绑定的 API Type 与新模型分类不匹配")
		}
	}
	return nil
}

func (h *Handler) validateProviderBindings(providerID int64, apiFormat string) error {
	var models []database.RelayModel
	if err := h.db.Table("relay_models m").Joins("JOIN relay_model_bindings b ON b.relay_model_id = m.id").Where("b.provider_id = ?", providerID).Find(&models).Error; err != nil {
		return errors.New("检查上游绑定失败")
	}
	for _, model := range models {
		if !relay.SupportsCategory(apiFormat, model.Category) {
			return errors.New("现有绑定的模型分类与新 API Type 不匹配")
		}
		if apiFormat == relay.APIFormatOpenAISpeech || apiFormat == relay.APIFormatVivoLASR {
			var formats []string
			if err := h.db.Table("relay_model_bindings b").Select("p.api_format").Joins("JOIN relay_providers p ON p.id = b.provider_id").Where("b.relay_model_id = ? AND p.id <> ?", model.ID, providerID).Pluck("p.api_format", &formats).Error; err != nil {
				return errors.New("检查语音绑定失败")
			}
			for _, existing := range formats {
				if (apiFormat == relay.APIFormatOpenAISpeech && existing == relay.APIFormatVivoLASR) || (apiFormat == relay.APIFormatVivoLASR && existing == relay.APIFormatOpenAISpeech) {
					return errors.New("同一语音模型不能混用 openai_speech 与 vivo_lasr")
				}
			}
		}
	}
	return nil
}

func (h *Handler) loadRelayModel(c *gin.Context) (database.RelayModel, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.Status(http.StatusNotFound)
		return database.RelayModel{}, false
	}
	var model database.RelayModel
	if err := h.db.First(&model, "id = ?", id).Error; err != nil {
		c.Status(http.StatusNotFound)
		return model, false
	}
	return model, true
}

func (h *Handler) loadRelayBinding(c *gin.Context) (database.RelayModelBinding, database.RelayModel, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.Status(http.StatusNotFound)
		return database.RelayModelBinding{}, database.RelayModel{}, false
	}
	var binding database.RelayModelBinding
	if err := h.db.First(&binding, "id = ?", id).Error; err != nil {
		c.Status(http.StatusNotFound)
		return binding, database.RelayModel{}, false
	}
	var model database.RelayModel
	if err := h.db.First(&model, "id = ?", binding.RelayModelID).Error; err != nil {
		c.Status(http.StatusNotFound)
		return binding, model, false
	}
	return binding, model, true
}

func (h *Handler) loadRelayProviderParam(c *gin.Context, name string) (database.RelayProvider, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil {
		c.Status(http.StatusNotFound)
		return database.RelayProvider{}, false
	}
	var provider database.RelayProvider
	if err := h.db.First(&provider, "id = ?", id).Error; err != nil {
		c.Status(http.StatusNotFound)
		return provider, false
	}
	return provider, true
}

func (h *Handler) hasActiveSpeechSession(where string, value interface{}) bool {
	var count int64
	return h.db.Model(&database.RelaySpeechSession{}).Where(where, value).Where("expires_at > ?", time.Now()).Count(&count).Error == nil && count > 0
}

func (h *Handler) credentialsURL(providerID int64) string {
	return "/admin/relay/" + strconv.FormatInt(providerID, 10) + "/credentials"
}

func (h *Handler) redirectCredentialsWithError(c *gin.Context, providerID int64, page, message string) {
	c.Redirect(http.StatusFound, h.credentialsURL(providerID)+"/"+page+"?error="+url.QueryEscape(message))
}

func (h *Handler) redirectCredentialEditWithError(c *gin.Context, credentialID int64, message string) {
	c.Redirect(http.StatusFound, "/admin/relay/credentials/"+strconv.FormatInt(credentialID, 10)+"/edit?error="+url.QueryEscape(message))
}

func (h *Handler) redirectCredentialListWithError(c *gin.Context, providerID int64, message string) {
	c.Redirect(http.StatusFound, h.credentialsURL(providerID)+"?error="+url.QueryEscape(message))
}

func (h *Handler) redirectRelayModelsWithError(c *gin.Context, message string) {
	c.Redirect(http.StatusFound, "/admin/relay/models?error="+url.QueryEscape(message))
}

func (h *Handler) redirectRelayModelNewWithError(c *gin.Context, message string) {
	c.Redirect(http.StatusFound, "/admin/relay/models/new?error="+url.QueryEscape(message))
}

func (h *Handler) redirectRelayModelEditWithError(c *gin.Context, modelID int64, message string) {
	c.Redirect(http.StatusFound, "/admin/relay/models/"+strconv.FormatInt(modelID, 10)+"/edit?error="+url.QueryEscape(message))
}

func (h *Handler) redirectRelayBindingEditWithError(c *gin.Context, bindingID int64, message string) {
	c.Redirect(http.StatusFound, "/admin/relay/bindings/"+strconv.FormatInt(bindingID, 10)+"/edit?error="+url.QueryEscape(message))
}

// --- Approve ---

func (h *Handler) Approve(c *gin.Context) {
	id := c.Param("id")
	reviewerID, _ := strconv.ParseInt(c.GetString("userID"), 10, 64)
	if err := h.marketSvc.Approve(id, reviewerID); err != nil {
		writeMarketActionError(c, err, "approve failed")
		return
	}
	c.Redirect(http.StatusFound, redirectBack(c, "/admin/pending"))
}

// --- Reject ---

func (h *Handler) Reject(c *gin.Context) {
	id := c.Param("id")
	reason := c.PostForm("reason")
	reviewerID, _ := strconv.ParseInt(c.GetString("userID"), 10, 64)
	if err := h.marketSvc.Reject(id, reviewerID, reason); err != nil {
		writeMarketActionError(c, err, "reject failed")
		return
	}
	c.Redirect(http.StatusFound, redirectBack(c, "/admin/pending"))
}

func writeMarketActionError(c *gin.Context, err error, message string) {
	if errors.Is(err, market.ErrPluginNotFound) {
		c.String(http.StatusNotFound, message)
		return
	}
	c.String(http.StatusInternalServerError, message)
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

func (h *Handler) redirectRelayWithError(c *gin.Context, message string) {
	c.Redirect(http.StatusFound, "/admin/relay?error="+url.QueryEscape(message))
}

func (h *Handler) redirectRelayNewWithError(c *gin.Context, message string) {
	c.Redirect(http.StatusFound, "/admin/relay/new?error="+url.QueryEscape(message))
}

func (h *Handler) redirectRelayEditWithError(c *gin.Context, message string) {
	c.Redirect(http.StatusFound, "/admin/relay/"+c.Param("id")+"/edit?error="+url.QueryEscape(message))
}

func redirectBack(c *gin.Context, fallback string) string {
	if v := c.PostForm("redirect"); strings.HasPrefix(v, "/admin/") {
		return v
	}
	return fallback
}

func setAdminCookie(c *gin.Context, name, value string, maxAge int) {
	c.SetSameSite(http.SameSiteLaxMode)
	forwardedProto := strings.TrimSpace(strings.Split(c.GetHeader("X-Forwarded-Proto"), ",")[0])
	secure := c.Request.TLS != nil || strings.EqualFold(forwardedProto, "https")
	c.SetCookie(name, value, maxAge, "/admin", "", secure, true)
}

func generateCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
