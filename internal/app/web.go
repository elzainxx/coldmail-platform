package app

import (
	"log/slog"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/httpapi"
	"github.com/coldmail-platform/platform/internal/webapp"
)

// newWebApp builds the web app (Phase 6 A2) when PLATFORM_WEB_DIR is set, and nothing when it is not: the nil interface and a no-op close, so that the handler serves no app and every request is
// answered exactly as it was before the dial existed. It runs at start-up, before the database pool is opened: a directory that is missing or has no index.html, or an address or key the browser
// cannot be given, is a *config.Error (exit status 2) that names the dial, never a value. The one log line says the app is served and where its dial is, and holds neither the directory, the address nor
// the key.
func newWebApp(cfg config.Config, log *slog.Logger) (httpapi.WebHandler, func(), error) {
	if cfg.WebDir == "" {
		return nil, func() {}, nil
	}
	a, err := webapp.New(webapp.Options{Dir: cfg.WebDir, SupabaseURL: cfg.PublicSupabaseURL, AnonKey: cfg.PublicSupabaseAnonKey})
	if err != nil {
		return nil, nil, err
	}
	log.Info("web app is served from this process", "set", config.VarWebDir, "config_json", true)
	return a, func() { _ = a.Close() }, nil
}
