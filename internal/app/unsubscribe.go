package app

import (
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
	"github.com/coldmail-platform/platform/internal/unsubscribe"
	"github.com/coldmail-platform/platform/internal/unsubscribe/endpoint"
)

// THE UNSUBSCRIBE LINK IN THE SERVICE (Phase 5, chunk S3). Three dials make it (PLATFORM_UNSUBSCRIBE_SECRET, PLATFORM_PUBLIC_BASE_URL, PLATFORM_UNSUBSCRIBE_RATE_PER_MIN) and two things come of them:
//
//   - the link maker the job store is given (jobs.WithUnsubscribe): it writes the link of every item of a send job, and the store refuses to confirm a send job without one;
//   - the public endpoint (internal/unsubscribe/endpoint), mounted beside the job API, NOT behind its authentication.
//
// Both need BOTH of the first two dials. With either unset there is no link maker and no route: a request for /u/{token} is the 404 envelope of any unknown path, a send job cannot be confirmed
// (ErrNotAllowed, jobs.ReasonUnsubscribeNotConfigured) and one line at start-up says which dial is missing. The service still starts: a deployment that only verifies lists needs neither.

// newUnsubscribeSigner builds the signer from the configuration at start-up and says in ONE log line whether the link is configured. It returns nil with no error when it is not (either dial unset). The
// line carries booleans and, when configured, the public address and the rate: never the secret.
func newUnsubscribeSigner(cfg config.Config, log *slog.Logger) (*unsubscribe.Signer, error) {
	secretSet, urlSet := cfg.UnsubscribeSecret != "", cfg.PublicBaseURL != ""
	if !secretSet || !urlSet {
		attrs := []any{"enabled", false, "secret_set", secretSet, "public_base_url_set", urlSet, "set", config.VarUnsubscribeSecret + " and " + config.VarPublicBaseURL,
			"effect", "a send job (module 2 or 3) cannot be confirmed (not_allowed, unsubscribe_not_configured) and the /u/{token} routes are not served"}
		if secretSet != urlSet {
			log.Warn("unsubscribe link is only half configured", attrs...)
		} else {
			log.Info("unsubscribe link is not configured", attrs...)
		}
		return nil, nil
	}
	s, err := unsubscribe.NewSigner([]byte(cfg.UnsubscribeSecret.Reveal()))
	if err != nil {
		// unreachable through config.Load (it holds the same floor); still a configuration error naming the variable, never a value
		return nil, &config.Error{Problems: []config.Problem{{Name: config.VarUnsubscribeSecret, Reason: fmt.Sprintf("must be at least %d bytes", unsubscribe.MinSecretBytes)}}}
	}
	log.Info("unsubscribe link configured", "enabled", true, "public_base_url", cfg.PublicBaseURL, "rate_per_min", cfg.UnsubscribeRatePerMin)
	return s, nil
}

// unsubscribeOptions is the option the job store is built with: its link maker, or nothing when the link is not configured (the store then refuses a send job's confirm).
func (d apiDeps) unsubscribeOptions(cfg config.Config) []jobs.Option {
	if d.Unsub == nil {
		return nil
	}
	return []jobs.Option{jobs.WithUnsubscribe(d.Unsub.LinkMaker(cfg.PublicBaseURL))}
}

// buildUnsubscribeEndpoint makes the public endpoint over the job store (ItemOwner) and the pool (internal/suppress writes the row), when the link is configured; otherwise it makes none.
func (d *apiDeps) buildUnsubscribeEndpoint(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) error {
	if d.Unsub == nil {
		return nil
	}
	ep, err := endpoint.New(endpoint.Config{
		Signer:       d.Unsub,
		Items:        d.Jobs,
		Suppressions: endpoint.SuppressionsOf(pool),
		RatePerMin:   cfg.UnsubscribeRatePerMin,
		Logger:       log,
	})
	if err != nil {
		return fmt.Errorf("build the unsubscribe endpoint: %w", err)
	}
	d.UnsubEndpoint = ep
	log.Info("unsubscribe endpoint mounted", "routes", len(endpoint.Routes()), "rate_per_min", cfg.UnsubscribeRatePerMin)
	return nil
}
