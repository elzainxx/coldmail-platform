package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

// classify names the class of a transport error and must never return its text (the text of a
// *url.Error holds the full URL, credentials included).
func TestClassify_NamesTheClassAndNeverTheText(t *testing.T) {
	const leak = "zzzz-urlpass-zzzz"
	wrap := func(inner error) error {
		return &url.Error{Op: "Get", URL: "http://probeuser:" + leak + "@host:1/path?q=" + leak, Err: inner}
	}
	cases := map[string]struct {
		err  error
		want string
	}{
		"deadline":              {wrap(context.DeadlineExceeded), "timeout"},
		"net timeout":           {wrap(&net.OpError{Op: "dial", Err: timeoutErr{}}), "timeout"},
		"canceled":              {wrap(context.Canceled), "canceled"},
		"refused (errno)":       {wrap(&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), "connection refused"},
		"refused (windows)":     {wrap(&net.OpError{Op: "dial", Err: os.NewSyscallError("connectex", syscall.Errno(10061))}), "connection refused"},
		"refused (text only)":   {wrap(errors.New("dial tcp 10.0.0.1:1: connect: Connection refused")), "connection refused"},
		"dns not found":         {wrap(&net.DNSError{Err: "no such host", Name: "zzzz-host", IsNotFound: true}), "dns lookup failed (no such host)"},
		"dns other":             {wrap(&net.DNSError{Err: "server misbehaving", Name: "zzzz-host"}), "dns lookup failed"},
		"reset (errno)":         {wrap(&net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}), "connection reset"},
		"reset (windows)":       {wrap(&net.OpError{Op: "read", Err: os.NewSyscallError("wsarecv", syscall.Errno(10054))}), "connection reset"},
		"closed early":          {wrap(io.ErrUnexpectedEOF), "connection closed before a complete response"},
		"eof":                   {wrap(io.EOF), "connection closed before a complete response"},
		"tls":                   {wrap(&tls.CertificateVerificationError{Err: errors.New("x509: certificate signed by unknown authority")}), "tls certificate rejected"},
		"unclassified":          {wrap(errors.New("something " + leak)), "network error (*errors.errorString)"},
		"unclassified, no wrap": {errors.New("plain " + leak), "network error (*errors.errorString)"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := classify(c.err)
			if got != c.want {
				t.Fatalf("classify = %q, want %q", got, c.want)
			}
			if strings.Contains(got, leak) || strings.Contains(got, "probeuser") || strings.Contains(got, "host:1") {
				t.Fatalf("classify leaked part of the error text: %q", got)
			}
		})
	}
	// Control: the raw error text DOES carry the credentials, so the assertion above can fail.
	if !strings.Contains(wrap(context.DeadlineExceeded).Error(), leak) {
		t.Fatal("control failed: the raw *url.Error does not contain the URL credentials, so the test proves nothing")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// A plain connect failure is not a timeout, and a timeout is not a refusal: the order of the cases in
// classify must not let one class swallow another.
func TestClassify_ClassesAreDistinct(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	if classify(refused) == "timeout" {
		t.Fatal("a refused connection was classified as a timeout")
	}
	if classify(&net.OpError{Op: "dial", Err: timeoutErr{}}) == "connection refused" {
		t.Fatal("a timeout was classified as a refusal")
	}
}

func TestNormaliseURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://m2:8080":        "http://m2:8080",
		"http://m2:8080/":       "http://m2:8080",
		"https://x.internal/a/": "https://x.internal/a",
		"http://127.0.0.1:9":    "http://127.0.0.1:9",
		"http://[::1]:9/":       "http://[::1]:9",
	} {
		got, why := normaliseURL(in)
		if why != "" || got != want {
			t.Errorf("normaliseURL(%q) = %q, %q; want %q", in, got, why, want)
		}
	}
	for _, in := range []string{"", "m2:8080", "ftp://m2", "http://", "http://u:p@m2", "http://m2?x", "http://m2/#f", "http://m 2"} {
		if got, why := normaliseURL(in); why == "" || got != "" || strings.Contains(why, in) && in != "" {
			t.Errorf("normaliseURL(%q) = %q, %q; want a refusal that does not echo the input", in, got, why)
		}
	}
}
