package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	relayLogs *relay.LogService
	authSvc   *auth.Service
	marketSvc *market.Service
	jwtMgr    *auth.JWTManager
	templates *template.Template
}

// NewHandler creates an admin handler using templates embedded in the binary.
func NewHandler(db *gorm.DB, authSvc *auth.Service, marketSvc *market.Service, jwtMgr *auth.JWTManager) (*Handler, error) {
	tmpl, err := parseAdminTemplates()
	if err != nil {
		return nil, err
	}
	return &Handler{
		db:        db,
		relayLogs: relay.NewLogService(db),
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
		protected.GET("/relay", h.RelayProviders)
		protected.GET("/relay/dashboard", h.RelayDashboard)
		protected.GET("/relay/logs", h.RelayLogs)
		protected.GET("/relay/new", h.NewRelayProviderForm)
		protected.POST("/relay/new", h.CreateRelayProvider)
		protected.GET("/relay/:id/edit", h.EditRelayProviderForm)
		protected.POST("/relay/:id/edit", h.UpdateRelayProvider)
		protected.POST("/relay/:id/toggle", h.ToggleRelayProvider)
		protected.POST("/relay/:id/delete", h.DeleteRelayProvider)
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

// --- Relay providers ---

type relayProviderView struct {
	Provider   database.RelayProvider
	ModelsText string
	ModelCount int
	Categories string
	MaskedKey  string
}

type relayModelFormRow struct {
	Index            int
	ModelID          string
	DisplayName      string
	Description      string
	Category         string
	SupportsVision   bool
	SupportsThinking bool
	SupportsTools    bool
	MaxTokens        string
	Temperature      string
	TopP             string
	PresencePenalty  string
	FrequencyPenalty string
	Seed             string
	Stop             string
	User             string
	DebugSSE         bool
	Enabled          bool
}

func (h *Handler) RelayProviders(c *gin.Context) {
	var providers []database.RelayProvider
	h.db.Preload("Entries").Order("updated_at DESC").Find(&providers)
	views := make([]relayProviderView, 0, len(providers))
	for _, provider := range providers {
		models, _ := relayModelRowsFromProvider(provider)
		views = append(views, relayProviderView{
			Provider:   provider,
			ModelsText: relayModelSummary(models),
			ModelCount: len(models),
			Categories: relayCategorySummary(models),
			MaskedKey:  maskAPIKey(provider.APIKey),
		})
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
		"ModelRows":        defaultRelayModelRows(nil),
		"Error":            c.Query("error"),
	}))
}

func (h *Handler) CreateRelayProvider(c *gin.Context) {
	apiFormat, err := parseRelayAPIFormat(c.PostForm("apiFormat"))
	if err != nil {
		h.redirectRelayNewWithError(c, err.Error())
		return
	}
	models, err := parseRelayModelForm(c)
	if err != nil {
		h.redirectRelayNewWithError(c, "模型列表格式错误")
		return
	}
	config := parseRelayProviderConfig(c)
	if err := validateRelayProviderForm(apiFormat, strings.TrimSpace(c.PostForm("endpoint")), strings.TrimSpace(c.PostForm("apiKey")), config, models); err != nil {
		h.redirectRelayNewWithError(c, err.Error())
		return
	}
	provider := database.RelayProvider{
		Name:      strings.TrimSpace(c.PostForm("name")),
		Endpoint:  strings.TrimRight(strings.TrimSpace(c.PostForm("endpoint")), "/"),
		APIKey:    strings.TrimSpace(c.PostForm("apiKey")),
		APIFormat: apiFormat,
		Config:    relay.EncodeProviderConfig(config),
		Enabled:   c.PostForm("enabled") == "on",
	}
	if provider.Name == "" {
		h.redirectRelayNewWithError(c, "名称必填")
		return
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&provider).Error; err != nil {
			return err
		}
		return replaceRelayModelsTx(tx, provider.ID, models)
	}); err != nil {
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
	models, _ := relayModelRowsFromProvider(provider)
	config := relay.DecodeProviderConfig(provider.Config)
	if config.AppID == "" {
		config.AppID = legacyProviderAppID(provider)
	}
	h.render(c, "relay_edit.html", h.pageData(c, "relay", map[string]interface{}{
		"Title":            "编辑中转上游",
		"Action":           "/admin/relay/" + c.Param("id") + "/edit",
		"Provider":         provider,
		"Name":             provider.Name,
		"Endpoint":         provider.Endpoint,
		"APIFormat":        provider.APIFormat,
		"ModelRows":        defaultRelayModelRows(models),
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
	models, err := parseRelayModelForm(c)
	if err != nil {
		h.redirectRelayEditWithError(c, "模型列表格式错误")
		return
	}
	config := parseRelayProviderConfig(c)
	provider.Name = strings.TrimSpace(c.PostForm("name"))
	provider.Endpoint = strings.TrimRight(strings.TrimSpace(c.PostForm("endpoint")), "/")
	provider.APIFormat = apiFormat
	provider.Enabled = c.PostForm("enabled") == "on"
	if key := strings.TrimSpace(c.PostForm("apiKey")); key != "" {
		provider.APIKey = key
	}
	provider.Config = relay.EncodeProviderConfig(config)
	if provider.Name == "" {
		h.redirectRelayEditWithError(c, "名称必填")
		return
	}
	if err := validateRelayProviderForm(apiFormat, provider.Endpoint, provider.APIKey, config, models); err != nil {
		h.redirectRelayEditWithError(c, err.Error())
		return
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		provider.Models = ""
		if err := tx.Save(&provider).Error; err != nil {
			return err
		}
		return replaceRelayModelsTx(tx, provider.ID, models)
	}); err != nil {
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
	if err := h.db.Preload("Entries").First(&provider, "id = ?", id).Error; err != nil {
		c.Status(http.StatusNotFound)
		return database.RelayProvider{}, false
	}
	return provider, true
}

func relayModelsJSON(text string) (string, error) {
	lines := strings.Split(text, "\n")
	models := make([]string, 0, len(lines))
	seen := map[string]struct{}{}
	for _, line := range lines {
		model := strings.TrimSpace(line)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	raw, err := json.Marshal(models)
	return string(raw), err
}

func relayModelsText(raw string) string {
	var models []string
	if err := json.Unmarshal([]byte(raw), &models); err != nil {
		return ""
	}
	return strings.Join(models, "\n")
}

func relayModelRowsFromProvider(provider database.RelayProvider) ([]relayModelFormRow, error) {
	if len(provider.Entries) > 0 {
		rows := make([]relayModelFormRow, 0, len(provider.Entries))
		for i, entry := range provider.Entries {
			cap := relay.DecodeCapabilities(entry.Capabilities)
			params := relay.DecodeAdvancedParams(entry.AdvancedParams)
			rows = append(rows, relayModelFormRow{
				Index:            i,
				ModelID:          entry.ModelID,
				DisplayName:      entry.DisplayName,
				Description:      entry.Description,
				Category:         relay.NormalizeCategory(entry.Category),
				SupportsVision:   cap.Vision,
				SupportsThinking: cap.Thinking,
				SupportsTools:    cap.Tools,
				MaxTokens:        intPtrString(params.MaxTokens),
				Temperature:      floatPtrString(params.Temperature),
				TopP:             floatPtrString(params.TopP),
				PresencePenalty:  floatPtrString(params.PresencePenalty),
				FrequencyPenalty: floatPtrString(params.FrequencyPenalty),
				Seed:             intPtrString(params.Seed),
				Stop:             strings.Join(params.Stop, "\n"),
				User:             stringPtrString(params.User),
				DebugSSE:         params.DebugSSE,
				Enabled:          entry.Enabled,
			})
		}
		return rows, nil
	}
	models, err := relay.DecodeModels(provider.Models)
	if err != nil {
		return nil, err
	}
	rows := make([]relayModelFormRow, 0, len(models))
	for i, model := range models {
		rows = append(rows, relayModelFormRow{Index: i, ModelID: model, Category: relay.CategoryChat, Enabled: true})
	}
	return rows, nil
}

func defaultRelayModelRows(rows []relayModelFormRow) []relayModelFormRow {
	rows = append(rows, relayModelFormRow{Index: len(rows), Category: relay.CategoryChat, Enabled: true})
	for i := range rows {
		rows[i].Index = i
		if rows[i].Category == "" {
			rows[i].Category = relay.CategoryChat
		}
	}
	return rows
}

func parseRelayModelForm(c *gin.Context) ([]database.RelayModel, error) {
	ids := c.PostFormArray("modelId")
	if len(ids) == 0 && strings.TrimSpace(c.PostForm("models")) != "" {
		legacy, err := relay.DecodeModels(mustRelayModelsJSON(c.PostForm("models")))
		if err != nil {
			return nil, err
		}
		models := make([]database.RelayModel, 0, len(legacy))
		for _, model := range legacy {
			models = append(models, database.RelayModel{ModelID: model, Category: relay.CategoryChat, Capabilities: relay.EncodeCapabilities(relay.ModelCapabilities{}), AdvancedParams: relay.EncodeAdvancedParams(relay.ModelAdvancedParams{}), Enabled: true})
		}
		return models, nil
	}
	models := make([]database.RelayModel, 0, len(ids))
	seen := map[string]struct{}{}
	for i, id := range ids {
		modelID := strings.TrimSpace(id)
		if modelID == "" {
			continue
		}
		if _, ok := seen[modelID]; ok {
			return nil, errors.New("模型 ID 不能重复")
		}
		seen[modelID] = struct{}{}
		params, err := parseAdvancedParams(c, i)
		if err != nil {
			return nil, err
		}
		models = append(models, database.RelayModel{
			ModelID:        modelID,
			DisplayName:    indexedPostForm(c, "displayName", i),
			Description:    indexedPostForm(c, "description", i),
			Category:       relay.NormalizeCategory(indexedPostForm(c, "category", i)),
			Capabilities:   relay.EncodeCapabilities(relay.ModelCapabilities{Vision: c.PostForm("supportsVision_"+strconv.Itoa(i)) == "on", Thinking: c.PostForm("supportsThinking_"+strconv.Itoa(i)) == "on", Tools: c.PostForm("supportsTools_"+strconv.Itoa(i)) == "on"}),
			AdvancedParams: relay.EncodeAdvancedParams(params),
			Enabled:        c.PostForm("modelEnabled_"+strconv.Itoa(i)) == "on",
		})
	}
	return models, nil
}

func mustRelayModelsJSON(text string) string {
	raw, err := relayModelsJSON(text)
	if err != nil {
		return "[]"
	}
	return raw
}

func replaceRelayModelsTx(tx *gorm.DB, providerID int64, models []database.RelayModel) error {
	if err := tx.Model(&database.RelayProvider{}).Where("id = ?", providerID).Update("models", "").Error; err != nil {
		return err
	}
	if err := tx.Where("provider_id = ?", providerID).Delete(&database.RelayModel{}).Error; err != nil {
		return err
	}
	for i := range models {
		models[i].ProviderID = providerID
		if err := tx.Create(&models[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

func parseAdvancedParams(c *gin.Context, i int) (relay.ModelAdvancedParams, error) {
	var params relay.ModelAdvancedParams
	if v := strings.TrimSpace(indexedPostForm(c, "maxTokens", i)); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return params, err
		}
		params.MaxTokens = &parsed
	}
	if v, err := parseOptionalFloat(indexedPostForm(c, "temperature", i)); err != nil {
		return params, err
	} else {
		params.Temperature = v
	}
	if v, err := parseOptionalFloat(indexedPostForm(c, "topP", i)); err != nil {
		return params, err
	} else {
		params.TopP = v
	}
	if v, err := parseOptionalFloat(indexedPostForm(c, "presencePenalty", i)); err != nil {
		return params, err
	} else {
		params.PresencePenalty = v
	}
	if v, err := parseOptionalFloat(indexedPostForm(c, "frequencyPenalty", i)); err != nil {
		return params, err
	} else {
		params.FrequencyPenalty = v
	}
	if v := strings.TrimSpace(indexedPostForm(c, "seed", i)); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return params, err
		}
		params.Seed = &parsed
	}
	if v := strings.TrimSpace(indexedPostForm(c, "stop", i)); v != "" {
		for _, stop := range strings.Split(v, "\n") {
			if stop = strings.TrimSpace(stop); stop != "" {
				params.Stop = append(params.Stop, stop)
			}
		}
	}
	if v := strings.TrimSpace(indexedPostForm(c, "user", i)); v != "" {
		params.User = &v
	}
	params.DebugSSE = c.PostForm("debugSse_"+strconv.Itoa(i)) == "on"
	return params, nil
}

func validateRelayModelForm(apiFormat string, models []database.RelayModel) error {
	for _, model := range models {
		if !relay.SupportsCategory(apiFormat, model.Category) {
			return errors.New("API Type 与模型分类不匹配")
		}
		params := relay.DecodeAdvancedParams(model.AdvancedParams)
		if params.MaxTokens != nil && *params.MaxTokens <= 0 {
			return errors.New("Max Tokens 必须大于 0")
		}
		if params.TopP != nil && (*params.TopP < 0 || *params.TopP > 1) {
			return errors.New("Top P 必须在 0 到 1 之间")
		}
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

func validateRelayProviderForm(apiFormat, endpoint, apiKey string, config relay.ProviderConfig, models []database.RelayModel) error {
	u, err := url.ParseRequestURI(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("Endpoint 必须是有效的 HTTP(S) URL")
	}
	if apiFormat != relay.APIFormatOllama && strings.TrimSpace(apiKey) == "" {
		return errors.New("API Key 必填")
	}
	if isVivoAppIDAPI(apiFormat) && strings.TrimSpace(config.AppID) == "" {
		return errors.New("VIVO OCR/LASR 必须填写 AppID")
	}
	return validateRelayModelForm(apiFormat, models)
}

func legacyProviderAppID(provider database.RelayProvider) string {
	for _, model := range provider.Entries {
		params := relay.DecodeAdvancedParams(model.AdvancedParams)
		if params.AppID != nil && strings.TrimSpace(*params.AppID) != "" {
			return strings.TrimSpace(*params.AppID)
		}
		if params.User != nil && strings.TrimSpace(*params.User) != "" {
			return strings.TrimSpace(*params.User)
		}
	}
	return ""
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

func indexedPostForm(c *gin.Context, key string, i int) string {
	values := c.PostFormArray(key)
	if i < 0 || i >= len(values) {
		return ""
	}
	return strings.TrimSpace(values[i])
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

func relayModelSummary(rows []relayModelFormRow) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ModelID != "" {
			parts = append(parts, row.ModelID+" ("+row.Category+")")
		}
	}
	return strings.Join(parts, "\n")
}

func relayCategorySummary(rows []relayModelFormRow) string {
	seen := map[string]struct{}{}
	categories := make([]string, 0, 4)
	for _, row := range rows {
		category := relay.NormalizeCategory(row.Category)
		if _, ok := seen[category]; ok {
			continue
		}
		seen[category] = struct{}{}
		categories = append(categories, category)
	}
	return strings.Join(categories, ", ")
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

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "••••"
	}
	return key[:4] + "••••" + key[len(key)-4:]
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
	c.SetCookie(name, value, maxAge, "/admin", "", c.Request.TLS != nil, true)
}

func generateCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
