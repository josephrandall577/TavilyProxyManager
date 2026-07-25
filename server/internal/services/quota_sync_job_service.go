package services

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"tavily-proxy/server/internal/models"
)

type QuotaSyncJobStatus struct {
	ID         string `json:"id"`
	Status     string `json:"status"` // idle|running|completed|error
	Error      string `json:"error,omitempty"`
	IntervalMs int    `json:"interval_ms,omitempty"`

	Total     int `json:"total"`
	Completed int `json:"completed"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`

	Items []QuotaSyncItemResult `json:"items,omitempty"`

	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

type QuotaSyncJobService struct {
	keys *KeyService
	sync *QuotaSyncService

	mu  sync.RWMutex
	job *QuotaSyncJobStatus
}

func NewQuotaSyncJobService(keys *KeyService, sync *QuotaSyncService) *QuotaSyncJobService {
	return &QuotaSyncJobService{keys: keys, sync: sync}
}

func (s *QuotaSyncJobService) Get() QuotaSyncJobStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.job == nil {
		return QuotaSyncJobStatus{Status: "idle"}
	}
	return cloneQuotaSyncJobStatus(*s.job)
}

func (s *QuotaSyncJobService) Start(interval time.Duration) (QuotaSyncJobStatus, bool, error) {
	if interval < 0 {
		interval = 0
	}
	if interval > maxQuotaSyncInterval {
		interval = maxQuotaSyncInterval
	}

	s.mu.Lock()
	if s.job != nil && s.job.Status == "running" {
		job := cloneQuotaSyncJobStatus(*s.job)
		s.mu.Unlock()
		return job, true, nil
	}

	ctx := context.Background()
	keyItems, err := s.keys.List(ctx)
	if err != nil {
		s.mu.Unlock()
		return QuotaSyncJobStatus{}, false, err
	}

	startedAt := time.Now()
	job := &QuotaSyncJobStatus{
		ID:         uuid.NewString(),
		Status:     "running",
		IntervalMs: int(interval.Milliseconds()),
		Total:      len(keyItems),
		Completed:  0,
		Succeeded:  0,
		Failed:     0,
		Items:      make([]QuotaSyncItemResult, len(keyItems)),
		StartedAt:  &startedAt,
		EndedAt:    nil,
	}

	for i, k := range keyItems {
		job.Items[i] = QuotaSyncItemResult{
			ID:     k.ID,
			Alias:  k.Alias,
			Status: "pending",
		}
	}

	s.job = job
	s.mu.Unlock()

	go s.runJob(job.ID, keyItems, interval)

	return cloneQuotaSyncJobStatus(*job), false, nil
}

func (s *QuotaSyncJobService) runJob(jobID string, keyItems []models.APIKey, interval time.Duration) {
	ctx := context.Background()

	for i, k := range keyItems {
		if interval > 0 && i > 0 {
			time.Sleep(interval)
		}

		item := s.sync.syncKey(ctx, k)

		s.mu.Lock()
		if s.job == nil || s.job.ID != jobID {
			s.mu.Unlock()
			return
		}

		s.job.Items[i] = item
		s.job.Completed++
		if item.Status == "ok" {
			s.job.Succeeded++
		} else {
			s.job.Failed++
		}
		s.mu.Unlock()
	}

	endedAt := time.Now()
	s.mu.Lock()
	if s.job != nil && s.job.ID == jobID {
		s.job.Status = "completed"
		s.job.EndedAt = &endedAt
	}
	s.mu.Unlock()
}

func cloneQuotaSyncJobStatus(in QuotaSyncJobStatus) QuotaSyncJobStatus {
	out := in
	if in.Items != nil {
		out.Items = make([]QuotaSyncItemResult, len(in.Items))
		copy(out.Items, in.Items)
	}
	if in.EndedAt != nil {
		t := *in.EndedAt
		out.EndedAt = &t
	}
	if in.StartedAt != nil {
		t := *in.StartedAt
		out.StartedAt = &t
	}
	return out
}
