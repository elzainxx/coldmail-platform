package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// The control API (HTTP, no authentication: the sink only exists on the harness's private network, which publishes no port).
//
//	GET  /readyz         200 once the SMTP listener is accepting connections, 503 before
//	GET  /api/messages   every accepted message, oldest first: [{seq, at, to[], from, envelope_from, subject, message_id, size, held_ms}]
//	POST /api/reset      forget every message, counter and peak and put the default mode back; held sessions are not released
//	GET  /api/mode       the mode in force
//	POST /api/mode       REPLACE the mode (absent fields take their defaults); unknown fields and invalid values are 400 and change nothing
//	GET  /api/inflight   {inflight, peak, held_total, held[]}: the sessions held in a delayed DATA right now, the most at once, and which
//	GET  /api/rcpt       {"addr": {accepted, rejected, deferred}}: what the sink answered to each recipient at RCPT TO
//
// Any other method on these paths is 405, any other path 404.

const maxBodyBytes = 1 << 20

// APIHandler is the control API.
func (s *Server) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.Ready() {
			http.Error(w, "the SMTP listener is not accepting connections yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		msgs := s.st.Messages()
		if msgs == nil {
			msgs = []Message{}
		}
		writeJSON(w, http.StatusOK, msgs)
	})
	// GET /api/message?seq=N: the raw bytes of one accepted message (dot-stuffing undone, exactly what the SMTP DATA carried), as message/rfc822. Phase 3 close-out D5: the plain-text part
	// travels from the platform's public.job_items through module 2 to the wire, and only the bytes can show the multipart structure. 400 for a seq that is not a number, 404 for one the sink never accepted.
	mux.HandleFunc("GET /api/message", func(w http.ResponseWriter, r *http.Request) {
		seq, err := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
		if err != nil || seq < 1 {
			writeError(w, http.StatusBadRequest, "seq must be the number of an accepted message (see GET /api/messages)")
			return
		}
		raw, ok := s.st.Raw(seq)
		if !ok {
			writeError(w, http.StatusNotFound, "no accepted message has that seq")
			return
		}
		w.Header().Set("Content-Type", "message/rfc822")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("POST /api/reset", func(w http.ResponseWriter, r *http.Request) {
		n := s.st.Reset()
		s.cfg.Logf("api: reset (%d message(s) forgotten, default mode restored)", n)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n})
	})
	mux.HandleFunc("GET /api/mode", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.st.Mode())
	})
	mux.HandleFunc("POST /api/mode", func(w http.ResponseWriter, r *http.Request) {
		m := defaultMode()
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		dec.DisallowUnknownFields() // a misspelt field must not read as "no change": it is the way a test would silently do nothing
		if err := dec.Decode(&m); err != nil {
			writeError(w, http.StatusBadRequest, "the body is not a mode object: "+err.Error())
			return
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "the body holds more than one JSON value")
			return
		}
		if err := m.normalize(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		got := s.st.SetMode(m)
		s.cfg.Logf("api: mode set: reject %d addr(s) with %d, defer %d, data_delay_ms %d (%d recipient(s)), fail_auth %v",
			len(got.RejectRcpt), got.RejectCode, len(got.DeferRcpt), got.DataDelayMs, len(got.DelayRecipients), got.FailAuth)
		writeJSON(w, http.StatusOK, got)
	})
	mux.HandleFunc("GET /api/inflight", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.st.Inflight())
	})
	mux.HandleFunc("GET /api/rcpt", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.st.Rcpt())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "cannot encode the answer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
