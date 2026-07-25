package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"tavily-proxy/server/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type StatsService struct {
	db *gorm.DB
}

func NewStatsService(db *gorm.DB) *StatsService {
	return &StatsService{db: db}
}

type Stats struct {
	TotalQuota     int64 `json:"total_quota"`
	TotalUsed      int64 `json:"total_used"`
	TotalRemaining int64 `json:"total_remaining"`
	KeyCount       int64 `json:"key_count"`
	ActiveKeyCount int64 `json:"active_key_count"`
	TodayRequests  int64 `json:"today_requests"`
}

type TimeSeriesSeries struct {
	Name string  `json:"name"`
	Data []int64 `json:"data"`
}

type TimeSeries struct {
	Granularity string             `json:"granularity"`
	Labels      []string           `json:"labels"`
	Series      []TimeSeriesSeries `json:"series"`
}

func (s *StatsService) Get(ctx context.Context) (Stats, error) {
	var totalQuota int64
	var totalUsed int64
	var keyCount int64
	var activeKeyCount int64

	if err := s.db.WithContext(ctx).Model(&models.APIKey{}).Count(&keyCount).Error; err != nil {
		return Stats{}, err
	}
	if err := s.db.WithContext(ctx).Model(&models.APIKey{}).Where("is_active = ? AND is_invalid = ? AND used_quota < total_quota", true, false).Count(&activeKeyCount).Error; err != nil {
		return Stats{}, err
	}

	type agg struct {
		TotalQuota int64
		TotalUsed  int64
	}
	var a agg
	if err := s.db.WithContext(ctx).
		Model(&models.APIKey{}).
		Select("COALESCE(SUM(total_quota),0) AS total_quota, COALESCE(SUM(used_quota),0) AS total_used").
		Scan(&a).Error; err != nil {
		return Stats{}, err
	}
	totalQuota = a.TotalQuota
	totalUsed = a.TotalUsed

	now := time.Now()
	todayBucket := now.Format("2006-01-02")
	var todayRequests int64
	{
		var rs models.RequestStat
		err := s.db.WithContext(ctx).First(&rs, "granularity = ? AND bucket = ? AND endpoint = ?", "day", todayBucket, "").Error
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return Stats{}, err
			}
		} else {
			todayRequests = rs.Count
		}
	}

	totalRemaining := totalQuota - totalUsed
	if totalRemaining < 0 {
		totalRemaining = 0
	}

	return Stats{
		TotalQuota:     totalQuota,
		TotalUsed:      totalUsed,
		TotalRemaining: totalRemaining,
		KeyCount:       keyCount,
		ActiveKeyCount: activeKeyCount,
		TodayRequests:  todayRequests,
	}, nil
}

func (s *StatsService) TimeSeries(ctx context.Context, granularity string) (TimeSeries, error) {
	now := time.Now()

	var (
		start       time.Time
		points      int
		step        func(time.Time) time.Time
		bucketKey   func(time.Time) string
		labelFormat func(time.Time) string
	)

	switch granularity {
	case "", "hour":
		granularity = "hour"
		// Last 24 hours, inclusive.
		start = time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location()).Add(-23 * time.Hour)
		points = 24
		step = func(t time.Time) time.Time { return t.Add(time.Hour) }
		bucketKey = func(t time.Time) string { return t.Format("2006-01-02 15:00") }
		labelFormat = func(t time.Time) string { return t.Format("01-02 15:00") }
	case "day":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -29)
		points = 30
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
		bucketKey = func(t time.Time) string { return t.Format("2006-01-02") }
		labelFormat = func(t time.Time) string { return t.Format("01-02") }
	case "month":
		firstOfThisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		start = firstOfThisMonth.AddDate(0, -11, 0)
		points = 12
		step = func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }
		bucketKey = func(t time.Time) string { return t.Format("2006-01") }
		labelFormat = func(t time.Time) string { return t.Format("2006-01") }
	default:
		return TimeSeries{}, fmt.Errorf("invalid granularity: %s", granularity)
	}

	startBucket := bucketKey(start)
	totalCounts, err := s.bucketCountsFromStats(ctx, granularity, "", startBucket)
	if err != nil {
		return TimeSeries{}, err
	}
	searchCounts, err := s.bucketCountsFromStats(ctx, granularity, "/search", startBucket)
	if err != nil {
		return TimeSeries{}, err
	}

	labels := make([]string, 0, points)
	totalSeries := make([]int64, 0, points)
	searchSeries := make([]int64, 0, points)

	t := start
	for i := 0; i < points; i++ {
		key := bucketKey(t)
		labels = append(labels, labelFormat(t))
		totalSeries = append(totalSeries, totalCounts[key])
		searchSeries = append(searchSeries, searchCounts[key])
		t = step(t)
	}

	return TimeSeries{
		Granularity: granularity,
		Labels:      labels,
		Series: []TimeSeriesSeries{
			{Name: "All Requests", Data: totalSeries},
			{Name: "Search", Data: searchSeries},
		},
	}, nil
}

func (s *StatsService) bucketCountsFromStats(ctx context.Context, granularity, endpoint, startBucket string) (map[string]int64, error) {
	type row struct {
		Bucket string `gorm:"column:bucket"`
		Count  int64  `gorm:"column:count"`
	}

	var rows []row
	if err := s.db.WithContext(ctx).
		Model(&models.RequestStat{}).
		Select("bucket, count").
		Where("granularity = ? AND endpoint = ? AND bucket >= ?", granularity, endpoint, startBucket).
		Order("bucket").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Bucket] = r.Count
	}
	return out, nil
}

func (s *StatsService) RecordRequest(ctx context.Context, endpoint string, occurredAt time.Time) error {
	loc := occurredAt.Location()
	hour := time.Date(occurredAt.Year(), occurredAt.Month(), occurredAt.Day(), occurredAt.Hour(), 0, 0, 0, loc)
	day := time.Date(occurredAt.Year(), occurredAt.Month(), occurredAt.Day(), 0, 0, 0, 0, loc)
	month := time.Date(occurredAt.Year(), occurredAt.Month(), 1, 0, 0, 0, 0, loc)

	updatedAt := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := upsertIncrement(tx, "hour", hour.Format("2006-01-02 15:00"), "", 1, updatedAt); err != nil {
			return err
		}
		if err := upsertIncrement(tx, "day", day.Format("2006-01-02"), "", 1, updatedAt); err != nil {
			return err
		}
		if err := upsertIncrement(tx, "month", month.Format("2006-01"), "", 1, updatedAt); err != nil {
			return err
		}

		if endpoint != "/search" {
			return nil
		}
		if err := upsertIncrement(tx, "hour", hour.Format("2006-01-02 15:00"), "/search", 1, updatedAt); err != nil {
			return err
		}
		if err := upsertIncrement(tx, "day", day.Format("2006-01-02"), "/search", 1, updatedAt); err != nil {
			return err
		}
		return upsertIncrement(tx, "month", month.Format("2006-01"), "/search", 1, updatedAt)
	})
}

func upsertIncrement(db *gorm.DB, granularity, bucket, endpoint string, inc int64, updatedAt time.Time) error {
	stat := models.RequestStat{
		Granularity: granularity,
		Bucket:      bucket,
		Endpoint:    endpoint,
		Count:       inc,
		UpdatedAt:   updatedAt,
	}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "granularity"}, {Name: "bucket"}, {Name: "endpoint"}},
		DoUpdates: clause.Assignments(map[string]any{
			"count":      gorm.Expr("count + ?", inc),
			"updated_at": updatedAt,
		}),
	}).Create(&stat).Error
}

func (s *StatsService) BackfillFromLogsIfEmpty(ctx context.Context) error {
	var existing int64
	if err := s.db.WithContext(ctx).Model(&models.RequestStat{}).Count(&existing).Error; err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}

	type row struct {
		Bucket string `gorm:"column:bucket"`
		Count  int64  `gorm:"column:count"`
	}

	type spec struct {
		granularity string
		expr        string
	}

	specs := []spec{
		{granularity: "hour", expr: "strftime('%Y-%m-%d %H:00', created_at, 'localtime')"},
		{granularity: "day", expr: "strftime('%Y-%m-%d', created_at, 'localtime')"},
		{granularity: "month", expr: "strftime('%Y-%m', created_at, 'localtime')"},
	}

	updatedAt := time.Now()

	for _, sp := range specs {
		var allRows []row
		if err := s.db.WithContext(ctx).
			Model(&models.RequestLog{}).
			Select(sp.expr + " as bucket, COUNT(*) as count").
			Group("bucket").
			Order("bucket").
			Scan(&allRows).Error; err != nil {
			return err
		}

		var searchRows []row
		if err := s.db.WithContext(ctx).
			Model(&models.RequestLog{}).
			Select(sp.expr+" as bucket, COUNT(*) as count").
			Where("endpoint = ?", "/search").
			Group("bucket").
			Order("bucket").
			Scan(&searchRows).Error; err != nil {
			return err
		}

		if len(allRows) > 0 {
			stats := make([]models.RequestStat, 0, len(allRows))
			for _, r := range allRows {
				stats = append(stats, models.RequestStat{
					Granularity: sp.granularity,
					Bucket:      r.Bucket,
					Endpoint:    "",
					Count:       r.Count,
					UpdatedAt:   updatedAt,
				})
			}
			if err := s.db.WithContext(ctx).CreateInBatches(stats, 500).Error; err != nil {
				return err
			}
		}

		if len(searchRows) > 0 {
			stats := make([]models.RequestStat, 0, len(searchRows))
			for _, r := range searchRows {
				stats = append(stats, models.RequestStat{
					Granularity: sp.granularity,
					Bucket:      r.Bucket,
					Endpoint:    "/search",
					Count:       r.Count,
					UpdatedAt:   updatedAt,
				})
			}
			if err := s.db.WithContext(ctx).CreateInBatches(stats, 500).Error; err != nil {
				return err
			}
		}
	}

	return nil
}
