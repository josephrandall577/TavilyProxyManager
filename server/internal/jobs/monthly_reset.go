package jobs

import (
	"context"
	"log/slog"
	"time"

	"tavily-proxy/server/internal/services"
)

func StartMonthlyReset(ctx context.Context, keys *services.KeyService, logger *slog.Logger) {
	go func() {
		for {
			now := time.Now()
			nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
			timer := time.NewTimer(time.Until(nextMidnight))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if nextMidnight.Day() == 1 {
					if err := keys.ResetAllUsage(context.Background()); err != nil {
						logger.Error("monthly reset failed", "err", err)
					} else {
						logger.Info("monthly quota reset completed")
					}
				}
			}
		}
	}()
}
