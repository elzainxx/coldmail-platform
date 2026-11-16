package entitlements

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// MaxErrorBytes caps every error text the entitlement flow stores (platform.m3_sync.last_error) or reports.
const MaxErrorBytes = 500

const redacted = "[redacted]"

var (
	// scheme://userinfo@host: the userinfo is replaced whatever it holds.
	reUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s@]*@`)
	// A bearer credential, whether or not the header name is present. (Eight characters or more: a shorter
	// word after "bearer" is prose, not a key; the exact-secret pass above is the wall that does not guess.)
	reBearer = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=\-]{8,}`)
	// name=value pairs whose name says "secret" (a query string, a form, a key=value log fragment).
	reNamedSecret = regexp.MustCompile(`(?i)\b(access_token|refresh_token|id_token|token|api[_-]?key|apikey|key|password|passwd|pwd|secret|authorization|auth|signature|sig)=[^&\s"';,]+`)
)

// scrubError renders err for storage and for a log line: the exact secrets the caller knows are removed first
// (in the three spellings a URL can carry), then every URL userinfo, bearer credential and secret-looking
// name=value pair, then control characters (a NUL cannot be stored; a newline forges a log line) and invalid
// UTF-8 are replaced, and the result is cut to MaxErrorBytes bytes on a character boundary.
//
// It is the defence in depth beneath the module-3 client, which is expected not to put the token in an error
// at all: this function is why a client bug cannot put it in the database or the log. It returns "" for nil.
func scrubError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	return scrubText(err.Error(), secrets...)
}

// scrubText is scrubError for a string.
func scrubText(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		for _, form := range []string{sec, url.QueryEscape(sec), url.PathEscape(sec)} {
			if form != "" {
				s = strings.ReplaceAll(s, form, redacted)
			}
		}
	}
	s = reUserinfo.ReplaceAllString(s, "${1}"+redacted+"@")
	s = reBearer.ReplaceAllString(s, "${1} "+redacted)
	s = reNamedSecret.ReplaceAllStringFunc(s, func(m string) string {
		return m[:strings.IndexByte(m, '=')+1] + redacted
	})
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return capBytes(strings.TrimSpace(s), MaxErrorBytes)
}

// capBytes returns s unchanged when it fits, and otherwise a prefix cut on a character boundary that ends in
// "..." and is at most max bytes in all.
func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const tail = "..."
	cut := max - len(tail)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + tail
}

// errClass names the class of an infrastructure error (a database or network failure) WITHOUT its text: the text
// of a connection error carries the host, the login and the database name, which have no business in a log line
// (internal/dsnscrub exists for the same reason). The unclassified case names the Go type of the innermost
// error, which is safe and enough to find the cause.
func errClass(err error) string {
	var (
		pe     *pgconn.PgError
		ce     *pgconn.ConnectError
		dns    *net.DNSError
		ne     net.Error
		certEr *tls.CertificateVerificationError
		unk    x509.UnknownAuthorityError
	)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &pe):
		if pe.ConstraintName != "" {
			return fmt.Sprintf("database error %s (constraint %s)", pe.Code, pe.ConstraintName)
		}
		return "database error " + pe.Code
	case errors.As(err, &dns):
		return "dns lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(strings.ToLower(err.Error()), "refused"):
		return "connection refused"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case errors.As(err, &certEr), errors.As(err, &unk):
		return "tls certificate rejected"
	case errors.As(err, &ce):
		return "database connection failed"
	}
	root := err
	for {
		next := errors.Unwrap(root)
		if next == nil {
			break
		}
		root = next
	}
	return fmt.Sprintf("error (%T)", root)
}
