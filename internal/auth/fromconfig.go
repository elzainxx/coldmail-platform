package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// Option adjusts the Options FromConfig builds. The service passes none: they are for tests and for the harness, which owns a fake issuer. WithAllowInsecureHTTP in particular is named by
// no production code (TestAllowInsecureHTTP_NoProductionCodeSetsIt fails if any non-test file outside this package mentions it).
type Option func(*Options)

// WithAllowInsecureHTTP lets the JWKS URL be an http URL to a loopback or private host (Options.AllowInsecureHTTP).
func WithAllowInsecureHTTP() Option { return func(o *Options) { o.AllowInsecureHTTP = true } }

// WithHTTPClient sets the client the key set is fetched with (Options.HTTPClient).
func WithHTTPClient(c *http.Client) Option { return func(o *Options) { o.HTTPClient = c } }

// WithNow sets the clock (Options.Now).
func WithNow(now func() time.Time) Option { return func(o *Options) { o.Now = now } }

// optionVars maps the Options field New refuses to the environment variable that sets it.
var optionVars = map[string]string{
	"HMACSecret": config.VarJWTSecret,
	"JWKSURL":    config.VarJWKSURL,
	"Issuer":     config.VarJWTIssuer,
	"Audience":   config.VarJWTAudience,
	"Leeway":     config.VarJWTLeewaySec,
}

// FromConfig builds the Verifier the configuration describes: PLATFORM_JWT_SECRET (HS256), PLATFORM_JWKS_URL (ES256 and RS256), PLATFORM_JWT_ISSUER, PLATFORM_JWT_AUDIENCE and
// PLATFORM_JWT_LEEWAY_SEC. It returns (nil, nil) when neither key source is set: the service starts and the job API is not served. A combination New refuses (an http JWKS URL, for
// instance) comes back as a *config.Error naming the variable, which the process turns into exit status 2; the message never carries a value.
func FromConfig(cfg config.Config, opts ...Option) (*Verifier, error) {
	if cfg.JWTSecret == "" && cfg.JWKSURL == "" {
		return nil, nil
	}
	o := Options{
		HMACSecret: []byte(cfg.JWTSecret.Reveal()),
		JWKSURL:    cfg.JWKSURL,
		Issuer:     cfg.JWTIssuer,
		Audience:   cfg.JWTAudience,
		Leeway:     cfg.JWTLeeway,
	}
	if o.Leeway == 0 {
		o.Leeway = NoLeeway // the dial's 0 means none; Options' zero value means the default
	}
	for _, opt := range opts {
		opt(&o)
	}
	v, err := New(o)
	if err != nil {
		var oe *OptionError
		if errors.As(err, &oe) {
			if name, ok := optionVars[oe.Field]; ok {
				return nil, &config.Error{Problems: []config.Problem{{Name: name, Reason: oe.Reason}}}
			}
		}
		return nil, err
	}
	return v, nil
}
