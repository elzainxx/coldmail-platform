package app

import (
	"log/slog"

	"github.com/coldmail-platform/platform/internal/auth"
	"github.com/coldmail-platform/platform/internal/config"
)

// apiDeps is what the job API (Phase 4, chunk B14b) is built from. This chunk puts in it the one dependency that exists so far, the verifier of Supabase access tokens; the API chunk takes
// it from here and adds what it needs beside it.
type apiDeps struct {
	// Auth verifies the bearer token of a request. nil means neither PLATFORM_JWT_SECRET nor PLATFORM_JWKS_URL is set: the job API is not served.
	Auth *auth.Verifier
}

// newAPIDeps builds the verifier from the configuration at start-up, so that an invalid combination stops the process before anything is served (exit status 2, naming the variable, never a
// value), and says in ONE log line which key sources are on. The line carries booleans, the audience and the leeway: never a secret, a key or the URL.
func newAPIDeps(cfg config.Config, log *slog.Logger) (apiDeps, error) {
	v, err := auth.FromConfig(cfg)
	if err != nil {
		return apiDeps{}, err
	}
	if v == nil {
		log.Info("job API authentication is not configured", "set", config.VarJWTSecret+" or "+config.VarJWKSURL, "effect", "the job API is not served")
		return apiDeps{}, nil
	}
	s := v.Summary()
	log.Info("job API authentication configured",
		"hs256", s.HMAC, "jwks", s.JWKS, "issuer_pinned", s.IssuerPinned, "audience", s.Audience, "leeway", s.Leeway.String())
	return apiDeps{Auth: v}, nil
}
