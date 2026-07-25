package services

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sort"
	"time"

	"tavily-proxy/server/internal/models"

	"gorm.io/gorm"
)

type KeyService struct {
	db     *gorm.DB
	logger *slog.Logger
}

func NewKeyService(db *gorm.DB, logger *slog.Logger) *KeyService {
	return &KeyService{db: db, logger: logger}
}

func (s *KeyService) List(ctx context.Context) ([]models.APIKey, error) {
	var keys []models.APIKey
	if err := s.db.WithContext(ctx).Order("id desc").Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

func (s *KeyService) Create(ctx context.Context, key, alias string, totalQuota int) (*models.APIKey, error) {
	if totalQuota <= 0 {
		totalQuota = 1000
	}
	record := models.APIKey{
		Key:        key,
		Alias:      alias,
		TotalQuota: totalQuota,
		UsedQuota:  0,
		IsActive:   true,
		IsInvalid:  false,
	}
	if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *KeyService) Get(ctx context.Context, id uint) (*models.APIKey, error) {
	var key models.APIKey
	if err := s.db.WithContext(ctx).First(&key, id).Error; err != nil {
		return nil, err
	}
	return &key, nil
}

type KeyUpdate struct {
	Alias      *string `json:"alias"`
	TotalQuota *int    `json:"total_quota"`
	UsedQuota  *int    `json:"used_quota"`
	IsActive   *bool   `json:"is_active"`
	ResetQuota bool    `json:"reset_quota"`
	SyncUsage  bool    `json:"sync_usage"`
}

func (s *KeyService) Update(ctx context.Context, id uint, upd KeyUpdate) (*models.APIKey, error) {
	var key models.APIKey
	if err := s.db.WithContext(ctx).First(&key, id).Error; err != nil {
		return nil, err
	}
	updates := map[string]any{}
	if upd.Alias != nil {
		updates["alias"] = *upd.Alias
	}
	if upd.TotalQuota != nil && *upd.TotalQuota > 0 {
		updates["total_quota"] = *upd.TotalQuota
	}
	if upd.UsedQuota != nil && *upd.UsedQuota >= 0 {
		updates["used_quota"] = *upd.UsedQuota
	}
	if upd.IsActive != nil {
		updates["is_active"] = gorm.Expr("CASE WHEN is_invalid THEN ? ELSE ? END", false, *upd.IsActive)
	}
	if upd.ResetQuota {
		updates["used_quota"] = 0
	}

	if len(updates) > 0 {
		if err := s.db.WithContext(ctx).Model(&models.APIKey{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return nil, err
		}
		if err := s.db.WithContext(ctx).First(&key, id).Error; err != nil {
			return nil, err
		}
	}
	return &key, nil
}

func (s *KeyService) Delete(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Delete(&models.APIKey{}, id).Error
}

func (s *KeyService) MarkInactive(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Model(&models.APIKey{}).Where("id = ?", id).Update("is_active", false).Error
}

func (s *KeyService) MarkInvalid(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Model(&models.APIKey{}).Where("id = ?", id).Updates(map[string]any{
		"is_active":  false,
		"is_invalid": true,
	}).Error
}

func (s *KeyService) MarkExhausted(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Model(&models.APIKey{}).
		Where("id = ?", id).
		Update("used_quota", gorm.Expr("total_quota")).Error
}

func (s *KeyService) IncrementUsed(ctx context.Context, id uint) error {
	now := time.Now()
	return s.db.WithContext(ctx).Model(&models.APIKey{}).Where("id = ?", id).Updates(map[string]any{
		"used_quota":   gorm.Expr("CASE WHEN used_quota + 1 > total_quota THEN total_quota ELSE used_quota + 1 END"),
		"last_used_at": &now,
	}).Error
}

func (s *KeyService) ResetAllUsage(ctx context.Context) error {
	return s.db.WithContext(ctx).Model(&models.APIKey{}).Where("1 = 1").Update("used_quota", 0).Error
}

func (s *KeyService) SetUsage(ctx context.Context, id uint, used int, total *int) error {
	updates := map[string]any{
		"used_quota": used,
	}
	if total != nil && *total > 0 {
		updates["total_quota"] = *total
		if used > *total {
			updates["used_quota"] = *total
		}
	}
	return s.db.WithContext(ctx).Model(&models.APIKey{}).Where("id = ?", id).Updates(updates).Error
}

func (s *KeyService) Candidates(ctx context.Context) ([]models.APIKey, error) {
	var keys []models.APIKey
	if err := s.db.WithContext(ctx).
		Where("is_active = ? AND is_invalid = ? AND used_quota < total_quota", true, false).
		Find(&keys).Error; err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}

	type scored struct {
		key       models.APIKey
		remaining int
	}
	scoredKeys := make([]scored, 0, len(keys))
	for _, k := range keys {
		scoredKeys = append(scoredKeys, scored{key: k, remaining: k.TotalQuota - k.UsedQuota})
	}

	sort.Slice(scoredKeys, func(i, j int) bool {
		return scoredKeys[i].remaining > scoredKeys[j].remaining
	})

	// Shuffle ties for fairness.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	out := make([]models.APIKey, 0, len(scoredKeys))
	for i := 0; i < len(scoredKeys); {
		j := i + 1
		for j < len(scoredKeys) && scoredKeys[j].remaining == scoredKeys[i].remaining {
			j++
		}
		group := scoredKeys[i:j]
		rng.Shuffle(len(group), func(a, b int) { group[a], group[b] = group[b], group[a] })
		for _, item := range group {
			out = append(out, item.key)
		}
		i = j
	}
	return out, nil
}

func (s *KeyService) FindByID(ctx context.Context, id uint) (*models.APIKey, error) {
	var key models.APIKey
	if err := s.db.WithContext(ctx).First(&key, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &key, nil
}

func (s *KeyService) DeleteInvalid(ctx context.Context) (int64, error) {
	result := s.db.WithContext(ctx).Where("is_invalid = ?", true).Delete(&models.APIKey{})
	return result.RowsAffected, result.Error
}
