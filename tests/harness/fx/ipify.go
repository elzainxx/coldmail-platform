package main

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"time"
)

// The "what is my IP" endpoint. The verifier node's host audit asks https://api.ipify.org for its own address (truemail 3.3.1, audit/ip.rb: Net::HTTP.get through the
// OS resolver, TLS verified against Ruby's store, which the harness points at the run's own CA through SSL_CERT_FILE) and then requires the verifier domain's A record
// to equal that body, byte for byte, and a PTR for it to name the verifier domain. So the body is the CALLER'S address as this server sees it (the node's own address on
// the fixture network, nothing translates it), with NO trailing newline: a newline would make the A-record comparison fail and the audit red.

type ipifyServer struct {
	st  *state
	srv *http.Server
	ln  net.Listener
}

func newIpifyServer(st *state, ln net.Listener, cert tls.Certificate) *ipifyServer {
	s := &ipifyServer{st: st, ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ip := hostOf(r.RemoteAddr)
		st.logIpify(ipifyEntry{Remote: ip, Answered: ip})
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, ip)
	})
	s.srv = &http.Server{
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		ErrorLog:          nil,
	}
	return s
}

func (s *ipifyServer) serve() {
	go func() { _ = s.srv.ServeTLS(s.ln, "", "") }() // ends with http.ErrServerClosed at close(); a handshake error is the client's (the node's TLS trust), logged by net/http
}

func (s *ipifyServer) close() { _ = s.srv.Close() }
