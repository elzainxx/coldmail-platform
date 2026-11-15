package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Doer is the one method of *http.Client the checks use, so a test can hand the checks anything.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// MaxBodyBytes is the most of a response body the probe will read. A longer body is a failed check.
const MaxBodyBytes = 1 << 20

// NewClient returns the one client every request goes through: the per-request timeout, no redirect
// followed (the 3xx response is returned as it is, and a 3xx is never the expected status), no proxy
// (the private network is reached directly, whatever HTTP_PROXY says) and no connection reuse (every
// check is a fresh connection, so one check cannot ride on another's).
func NewClient(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// classify names the class of a transport error: "timeout", "connection refused" and so on. It never
// returns err.Error(): the text of a *url.Error carries the full URL, credentials included, and the
// text of other errors carries addresses and paths. The unclassified case names the Go type of the
// innermost error, which is safe and enough to find the cause.
func classify(err error) string {
	var dns *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case isRefused(err):
		return "connection refused"
	case errors.As(err, &dns):
		if dns.IsNotFound {
			return "dns lookup failed (no such host)"
		}
		return "dns lookup failed"
	case isReset(err):
		return "connection reset"
	case errors.As(err, &certErr), errors.As(err, &unknownAuth), errors.As(err, &hostErr):
		return "tls certificate rejected"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed before a complete response"
	}
	root := err
	for {
		next := errors.Unwrap(root)
		if next == nil {
			break
		}
		root = next
	}
	return fmt.Sprintf("network error (%T)", root)
}

// Windows reports a refused or reset connection as WSAECONNREFUSED / WSAECONNRESET, which
// errors.Is(err, syscall.ECONNREFUSED) does not match there, so the numbers are checked as well.
const (
	wsaeConnReset   = 10054
	wsaeConnRefused = 10061
)

func isRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && uintptr(errno) == wsaeConnRefused {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "refused")
}

func isReset(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && uintptr(errno) == wsaeConnReset {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "forcibly closed")
}
