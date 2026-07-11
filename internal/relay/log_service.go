package relay

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
)

const LogRetention = 7 * 24 * time.Hour

type LogSummary struct {
	Total         int64
	Success       int64
	Failed        int64
	ActiveUsers   int64
	AverageMS     float64
	RequestBytes  int64
	ResponseBytes int64
}

type UserLogSummary struct {
	UserID        int64
	Username      string
	Total         int64
	Success       int64
	Failed        int64
	AverageMS     float64
	RequestBytes  int64
	ResponseBytes int64
	LastCalledAt  string
}

type NamedLogSummary struct {
	Name      string
	Total     int64
	Failed    int64
	AverageMS float64
}

type TrendPoint struct {
	Bucket    string
	Total     int64
	Success   int64
	Failed    int64
	AverageMS float64
}

type LogDashboard struct {
	Range     string
	Since     time.Time
	Summary   LogSummary
	Users     []UserLogSummary
	Providers []NamedLogSummary
	Models    []NamedLogSummary
	APITypes  []NamedLogSummary
	Trend     []TrendPoint
}

type LogFilter struct {
	Range     string
	UserID    string
	Username  string
	Provider  string
	APIType   string
	ModelID   string
	Operation string
	Protocol  string
	Result    string
	Page      int
	PageSize  int
}

type LogPage struct {
	Logs  []database.RelayRequestLog
	Total int64
	Page  int
	Pages int
}

type LogService struct {
	db    *gorm.DB
	queue chan database.RelayRequestLog
	once  sync.Once
}

func NewLogService(db *gorm.DB) *LogService {
	return &LogService{db: db, queue: make(chan database.RelayRequestLog, 256)}
}

func (s *LogService) Record(entry database.RelayRequestLog) error {
	return s.db.Create(&entry).Error
}

// Enqueue records a relay call without blocking the response path. A full
// queue drops the metadata entry rather than delaying the user's request.
func (s *LogService) Enqueue(entry database.RelayRequestLog) bool {
	s.once.Do(func() { go s.run() })
	select {
	case s.queue <- entry:
		return true
	default:
		return false
	}
}

func (s *LogService) DeleteExpired(now time.Time) error {
	return s.db.Where("created_at < ?", now.Add(-LogRetention)).Delete(&database.RelayRequestLog{}).Error
}

func (s *LogService) run() {
	cleanup := time.NewTicker(time.Hour)
	defer cleanup.Stop()
	for {
		select {
		case entry := <-s.queue:
			if err := s.Record(entry); err != nil {
				log.Printf("relay log write: %v", err)
			}
		case now := <-cleanup.C:
			if err := s.DeleteExpired(now); err != nil {
				log.Printf("relay log cleanup: %v", err)
			}
		}
	}
}

func LogRange(value string, now time.Time) (string, time.Time) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "today":
		local := now.In(time.Local)
		return "today", time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	case "24h":
		return "24h", now.Add(-24 * time.Hour)
	default:
		return "7d", now.Add(-LogRetention)
	}
}

func (s *LogService) Dashboard(rangeValue string, now time.Time) (LogDashboard, error) {
	rangeValue, since := LogRange(rangeValue, now)
	result := LogDashboard{Range: rangeValue, Since: since}
	base := s.db.Model(&database.RelayRequestLog{}).Where("created_at >= ?", since)
	if err := s.summaryQuery(base, &result.Summary); err != nil {
		return result, err
	}
	if err := base.Select(`user_id, MAX(username) AS username, COUNT(*) AS total,
		SUM(CASE WHEN http_status >= 200 AND http_status < 400 THEN 1 ELSE 0 END) AS success,
		SUM(CASE WHEN http_status < 200 OR http_status >= 400 THEN 1 ELSE 0 END) AS failed,
		COALESCE(AVG(duration_ms), 0) AS average_ms, COALESCE(SUM(request_bytes), 0) AS request_bytes,
		COALESCE(SUM(response_bytes), 0) AS response_bytes, CAST(MAX(created_at) AS TEXT) AS last_called_at`).
		Group("user_id").Order("total DESC").Limit(20).Scan(&result.Users).Error; err != nil {
		return result, err
	}
	if err := s.namedSummary(base, "provider_name", &result.Providers); err != nil {
		return result, err
	}
	if err := s.namedSummary(base, "model_id", &result.Models); err != nil {
		return result, err
	}
	if err := s.namedSummary(base, "api_type", &result.APITypes); err != nil {
		return result, err
	}
	return result, s.trend(since, rangeValue, &result.Trend)
}

func (s *LogService) Summary(rangeValue string, now time.Time) (LogSummary, error) {
	_, since := LogRange(rangeValue, now)
	var result LogSummary
	err := s.summaryQuery(s.db.Model(&database.RelayRequestLog{}).Where("created_at >= ?", since), &result)
	return result, err
}

func (s *LogService) summaryQuery(base *gorm.DB, target *LogSummary) error {
	return base.Select(`COUNT(*) AS total,
		SUM(CASE WHEN http_status >= 200 AND http_status < 400 THEN 1 ELSE 0 END) AS success,
		SUM(CASE WHEN http_status < 200 OR http_status >= 400 THEN 1 ELSE 0 END) AS failed,
		COUNT(DISTINCT user_id) AS active_users,
		COALESCE(AVG(duration_ms), 0) AS average_ms,
		COALESCE(SUM(request_bytes), 0) AS request_bytes,
		COALESCE(SUM(response_bytes), 0) AS response_bytes`).Scan(target).Error
}

func (s *LogService) namedSummary(base *gorm.DB, column string, target *[]NamedLogSummary) error {
	return base.Where(column + " <> ''").Select(column + ` AS name, COUNT(*) AS total,
		SUM(CASE WHEN http_status < 200 OR http_status >= 400 THEN 1 ELSE 0 END) AS failed,
		COALESCE(AVG(duration_ms), 0) AS average_ms`).Group(column).Order("total DESC").Limit(10).Scan(target).Error
}

func (s *LogService) trend(since time.Time, rangeValue string, target *[]TrendPoint) error {
	bucket := "strftime('%Y-%m-%d', created_at)"
	if rangeValue != "7d" {
		bucket = "strftime('%Y-%m-%d %H:00', created_at)"
	}
	if s.db.Dialector.Name() == "postgres" {
		bucket = "to_char(date_trunc('day', created_at), 'YYYY-MM-DD')"
		if rangeValue != "7d" {
			bucket = "to_char(date_trunc('hour', created_at), 'YYYY-MM-DD HH24:00')"
		}
	}
	return s.db.Model(&database.RelayRequestLog{}).Where("created_at >= ?", since).
		Select(bucket + ` AS bucket, COUNT(*) AS total,
			SUM(CASE WHEN http_status >= 200 AND http_status < 400 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN http_status < 200 OR http_status >= 400 THEN 1 ELSE 0 END) AS failed,
			COALESCE(AVG(duration_ms), 0) AS average_ms`).
		Group(bucket).Order("bucket ASC").Scan(target).Error
}

func (s *LogService) List(filter LogFilter, now time.Time) (LogPage, error) {
	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize < 1 || filter.PageSize > 100 {
		filter.PageSize = 50
	}
	_, since := LogRange(filter.Range, now)
	query := s.db.Model(&database.RelayRequestLog{}).Where("created_at >= ?", since)
	if id, err := strconv.ParseInt(strings.TrimSpace(filter.UserID), 10, 64); err == nil && id != 0 {
		query = query.Where("user_id = ?", id)
	}
	if filter.Username != "" {
		query = query.Where("username LIKE ?", "%"+strings.TrimSpace(filter.Username)+"%")
	}
	if filter.Provider != "" {
		query = query.Where("provider_name = ?", filter.Provider)
	}
	if filter.APIType != "" {
		query = query.Where("api_type = ?", filter.APIType)
	}
	if filter.ModelID != "" {
		query = query.Where("model_id = ?", filter.ModelID)
	}
	if filter.Operation != "" {
		query = query.Where("operation = ?", filter.Operation)
	}
	if filter.Protocol != "" {
		query = query.Where("protocol = ?", filter.Protocol)
	}
	if filter.Result == "success" {
		query = query.Where("http_status >= 200 AND http_status < 400")
	}
	if filter.Result == "failed" {
		query = query.Where("http_status < 200 OR http_status >= 400")
	}
	page := LogPage{Page: filter.Page}
	if err := query.Count(&page.Total).Error; err != nil {
		return page, err
	}
	page.Pages = int((page.Total + int64(filter.PageSize) - 1) / int64(filter.PageSize))
	if page.Pages == 0 {
		page.Pages = 1
	}
	if filter.Page > page.Pages {
		filter.Page = page.Pages
		page.Page = filter.Page
	}
	err := query.Order("created_at DESC").Offset((filter.Page - 1) * filter.PageSize).Limit(filter.PageSize).Find(&page.Logs).Error
	return page, err
}
