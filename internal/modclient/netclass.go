package modclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// A third copy of the transport classifier (internal/probe and internal/m1results each carry theirs): the two older packages are not changed by this chunk. It never returns
// err.Error(): the text of a *url.Error carries the full URL and the text of other errors carries addresses and paths.

// transportFailure is what a transport error means for the caller.
type transportFailure struct {
	kind   Kind
	reason string // a fixed phrase written here
	sent   bool   // false when the failure came before the request could have reached the module
	class  string // the same phrase, as the printed text of the cause
}

// stripURL drops the *url.Error wrapper, whose text carries the request URL, and keeps the error beneath it (errors.Is(err, context.DeadlineExceeded) and net.Error still work).
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

func classifyTransport(err error) transportFailure {
	inner := stripURL(err)
	var (
		dns        *net.DNSError
		certErr    *tls.CertificateVerificationError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		ne         net.Error
		opErr      *net.OpError
		dialFailed = errors.As(inner, &opErr) && opErr.Op == "dial"
	)
	mk := func(k Kind, reason string, sent bool) transportFailure {
		return transportFailure{kind: k, reason: reason, sent: sent, class: reason}
	}
	switch {
	case errors.Is(inner, context.DeadlineExceeded), errors.As(inner, &ne) && ne.Timeout():
		return mk(Timeout, "the request timed out", !dialFailed)
	case errors.Is(inner, context.Canceled):
		return mk(Canceled, "the request was canceled", true)
	case isRefused(inner):
		return mk(Unavailable, "connection refused", false)
	case errors.As(inner, &dns):
		if dns.IsNotFound {
			return mk(Unavailable, "dns lookup failed (no such host)", false)
		}
		return mk(Unavailable, "dns lookup failed", false)
	case isReset(inner):
		return mk(Unavailable, "connection reset", true)
	case errors.As(inner, &certErr), errors.As(inner, &unknownCA), errors.As(inner, &hostErr):
		return mk(Unavailable, "tls certificate rejected", false)
	case errors.Is(inner, io.EOF), errors.Is(inner, io.ErrUnexpectedEOF):
		return mk(Unavailable, "connection closed before a complete response", true)
	}
	root := inner
	for {
		next := errors.Unwrap(root)
		if next == nil {
			break
		}
		root = next
	}
	return mk(Unavailable, fmt.Sprintf("network error (%T)", root), !dialFailed)
}

// Windows reports a refused or reset connection as WSAECONNREFUSED / WSAECONNRESET, which errors.Is(err, syscall.ECONNREFUSED) does not match there.
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
