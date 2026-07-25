package httpserver

import (
	"embed"
	"net/http"
	"time"

	"tavily-proxy/server/internal/config"
	"tavily-proxy/server/internal/services"

	"log/slog"
)

type Dependencies struct {
	Config           config.Config
	EmbeddedPublic   embed.FS
	MasterKeyService *services.MasterKeyService
	SettingsService  *services.SettingsService
	KeyService       *services.KeyService
	QuotaSyncService *services.QuotaSyncService
	QuotaSyncJob     *services.QuotaSyncJobService
	LogService       *services.LogService
	StatsService     *services.StatsService
	CacheService     *services.CacheService
	TavilyProxy      *services.TavilyProxy
	Logger           *slog.Logger
}

func New(deps Dependencies) *http.Server {
	handler := NewRouter(deps)
	return &http.Server{
		Addr:              deps.Config.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}
