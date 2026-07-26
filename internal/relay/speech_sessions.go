package relay

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/lynai/backend/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errSpeechCapacity = errors.New("speech session capacity reached")

const speechCapacityLockID int64 = 0x4c796e5350656563

type speechSession struct {
	UserID          int64
	Candidate       Candidate
	AppID           string
	UpstreamAudioID string
	TaskID          string
}

type speechSessionStore struct {
	db           *gorm.DB
	ttl          time.Duration
	perUserLimit int64
	globalLimit  int64
}

func newSpeechSessionStore(db *gorm.DB, ttl time.Duration, perUserLimit, globalLimit int) *speechSessionStore {
	return &speechSessionStore{db: db, ttl: ttl, perUserLimit: int64(perUserLimit), globalLimit: int64(globalLimit)}
}

func (s *speechSessionStore) reserve(id, rawUserID string, candidate *Candidate, appID *string) error {
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err != nil {
		return err
	}
	now := time.Now()
	reservationTTL := s.ttl
	if reservationTTL > 10*time.Minute {
		reservationTTL = 10 * time.Minute
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", speechCapacityLockID).Error; err != nil {
				return err
			}
		}
		credentialQuery := tx
		if tx.Dialector.Name() == "postgres" {
			credentialQuery = credentialQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var credential database.RelayProviderCredential
		if err := credentialQuery.First(&credential, "id = ? AND provider_id = ? AND enabled = ?", candidate.Credential.ID, candidate.Provider.ID, true).Error; err != nil {
			return err
		}
		candidate.Credential = credential
		candidate.Config = MergeProviderConfig(DecodeProviderConfig(candidate.Provider.Config), DecodeProviderConfig(credential.Config))
		*appID = candidateAppID(*candidate)
		if *appID == "" {
			return errors.New("vivo_lasr requires AppID")
		}
		if err := tx.Where("expires_at <= ?", now).Delete(&database.RelaySpeechSession{}).Error; err != nil {
			return err
		}
		var global, perUser int64
		if err := tx.Model(&database.RelaySpeechSession{}).Where("expires_at > ?", now).Count(&global).Error; err != nil {
			return err
		}
		if global >= s.globalLimit {
			return errSpeechCapacity
		}
		if err := tx.Model(&database.RelaySpeechSession{}).Where("user_id = ? AND expires_at > ?", userID, now).Count(&perUser).Error; err != nil {
			return err
		}
		if perUser >= s.perUserLimit {
			return errSpeechCapacity
		}
		row := database.RelaySpeechSession{
			ID: id, UserID: userID, ProviderID: candidate.Provider.ID, BindingID: candidate.Binding.ID,
			CredentialID: candidate.Credential.ID, ModelID: candidate.Model.ModelID, UpstreamModel: candidate.Binding.UpstreamModel,
			Endpoint: candidate.Provider.Endpoint, APIFormat: candidate.Provider.APIFormat, ConfigSnapshot: EncodeProviderConfig(candidate.Config),
			AppID: *appID, ExpiresAt: now.Add(reservationTTL), CreatedAt: now, UpdatedAt: now,
		}
		return tx.Create(&row).Error
	})
}

func (s *speechSessionStore) completeReservation(id, rawUserID, upstreamAudioID string) error {
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err != nil {
		return err
	}
	now := time.Now()
	result := s.db.Model(&database.RelaySpeechSession{}).
		Where("id = ? AND user_id = ? AND upstream_audio_id = '' AND expires_at > ?", id, userID, now).
		Updates(map[string]interface{}{"upstream_audio_id": upstreamAudioID, "expires_at": now.Add(s.ttl), "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (s *speechSessionStore) get(id, rawUserID string) (speechSession, bool) {
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err != nil {
		return speechSession{}, false
	}
	now := time.Now()
	var row database.RelaySpeechSession
	if err := s.db.Where("id = ? AND user_id = ? AND upstream_audio_id <> '' AND expires_at > ?", id, userID, now).First(&row).Error; err != nil {
		return speechSession{}, false
	}
	result := s.db.Model(&database.RelaySpeechSession{}).
		Where("id = ? AND user_id = ? AND expires_at > ?", id, userID, now).
		Updates(map[string]interface{}{"expires_at": now.Add(s.ttl), "updated_at": now})
	if result.Error != nil || result.RowsAffected == 0 {
		return speechSession{}, false
	}
	var credential database.RelayProviderCredential
	if err := s.db.First(&credential, "id = ? AND provider_id = ?", row.CredentialID, row.ProviderID).Error; err != nil {
		return speechSession{}, false
	}
	candidate := Candidate{
		Model:      database.RelayModel{ModelID: row.ModelID, Category: CategorySpeech},
		Binding:    database.RelayModelBinding{ID: row.BindingID, ProviderID: row.ProviderID, UpstreamModel: row.UpstreamModel},
		Provider:   database.RelayProvider{ID: row.ProviderID, Endpoint: row.Endpoint, APIFormat: row.APIFormat, Config: row.ConfigSnapshot},
		Credential: credential, Config: DecodeProviderConfig(row.ConfigSnapshot),
	}
	return speechSession{UserID: userID, Candidate: candidate, AppID: row.AppID, UpstreamAudioID: row.UpstreamAudioID, TaskID: row.TaskID}, true
}

func (s *speechSessionStore) setTaskID(id, rawUserID, taskID string) bool {
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now()
	result := s.db.Model(&database.RelaySpeechSession{}).
		Where("id = ? AND user_id = ? AND upstream_audio_id <> '' AND expires_at > ?", id, userID, now).
		Updates(map[string]interface{}{"task_id": taskID, "expires_at": now.Add(s.ttl), "updated_at": now})
	return result.Error == nil && result.RowsAffected == 1
}

func (s *speechSessionStore) delete(id, rawUserID string) {
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err == nil {
		_ = s.db.Delete(&database.RelaySpeechSession{}, "id = ? AND user_id = ?", id, userID).Error
	}
}

func (s *speechSessionStore) deleteReservation(id, rawUserID string) {
	userID, err := strconv.ParseInt(rawUserID, 10, 64)
	if err == nil {
		_ = s.db.Delete(&database.RelaySpeechSession{}, "id = ? AND user_id = ? AND upstream_audio_id = ''", id, userID).Error
	}
}

func (s *speechSessionStore) deleteExpired(now time.Time) error {
	if err := s.db.Where("expires_at <= ?", now).Delete(&database.RelaySpeechSession{}).Error; err != nil {
		return fmt.Errorf("delete expired speech sessions: %w", err)
	}
	return nil
}
