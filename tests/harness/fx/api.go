package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The control API (HTTP, no authentication: the fixture exists only on the harness's internal networks, which publish no port).
//
//	GET  /readyz                  200 once every started role accepts connections, 503 before
//	GET  /api/state               {zones, dns_records, dns_modes, smtp_domains, seq, counters}: seq is the sequence number of the latest log line, the baseline for ?since=
//	POST /api/reset               put the startup tables back, forget every log line and counter
//	GET  /api/dns                 {records: {name: {type: [values]}}, modes: {name: mode}}
//	PUT  /api/dns/modes           {"name": "x.fixture.test", "mode": "servfail|refused|silent|nxdomain|normal"}: the mode applies to the name and everything below it
//	PUT  /api/dns/records         {"name": ..., "type": "A|MX|TXT|PTR", "values": [...]}: REPLACES the record set; no values deletes it
//	GET  /api/dns/queries         ?since=<seq>: the queries answered after that sequence number
//	GET  /api/smtp/domains        {domain: [rules]}
//	PUT  /api/smtp/domains        {"domain": "x.fixture.test", "rules": [{"match": "nx-*", "reply": "550 5.1.1 ..."}, ...]}: REPLACES that domain's rules
//	GET  /api/smtp/sessions       ?since=<seq>&domain=<d>: one entry per RCPT TO the SMTP side answered
//	GET  /api/ipify               the addresses the HTTPS endpoint told its callers
//
// Unknown JSON fields and invalid values are 400 and change nothing. Any other method on these paths is 405, any other path 404.

const maxBodyBytes = 1 << 20

type api struct {
	st    *state
	ready func() bool
}

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !a.ready() {
			http.Error(w, "a listener is not accepting connections yet", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		a.st.mu.RLock()
		out := map[string]any{"zones": a.st.zones, "dns_records": len(a.st.records), "dns_modes": a.st.modes, "smtp_domains": len(a.st.smtp), "seq": a.st.seq}
		a.st.mu.RUnlock()
		out["counters"] = a.st.counts()
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /api/reset", func(w http.ResponseWriter, r *http.Request) {
		a.st.reset()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/dns", func(w http.ResponseWriter, r *http.Request) {
		a.st.mu.RLock()
		out := map[string]any{"records": a.st.records, "modes": a.st.modes}
		b, err := json.Marshal(out)
		a.st.mu.RUnlock()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(b, '\n'))
	})
	mux.HandleFunc("PUT /api/dns/modes", func(w http.ResponseWriter, r *http.Request) {
		var m modeSpec
		if !decodeBody(w, r, &m) {
			return
		}
		if err := a.st.setMode(m.Name, m.Mode); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": normName(m.Name), "mode": m.Mode})
	})
	mux.HandleFunc("PUT /api/dns/records", func(w http.ResponseWriter, r *http.Request) {
		var rec recordSpec
		if !decodeBody(w, r, &rec) {
			return
		}
		if err := a.st.setRecords(rec.Name, rec.Type, rec.Values); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": normName(rec.Name), "type": rec.Type, "values": rec.Values})
	})
	mux.HandleFunc("GET /api/dns/queries", func(w http.ResponseWriter, r *http.Request) {
		since, ok := sinceParam(w, r)
		if !ok {
			return
		}
		a.st.mu.RLock()
		out := []dnsEntry{}
		for _, e := range a.st.dnsLog {
			if e.Seq > since {
				out = append(out, e)
			}
		}
		a.st.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/smtp/domains", func(w http.ResponseWriter, r *http.Request) {
		a.st.mu.RLock()
		out := map[string][]smtpRule{}
		names := make([]string, 0, len(a.st.smtp))
		for d := range a.st.smtp {
			names = append(names, d)
		}
		sort.Strings(names)
		for _, d := range names {
			out[d] = a.st.smtp[d]
		}
		a.st.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("PUT /api/smtp/domains", func(w http.ResponseWriter, r *http.Request) {
		var d smtpDomainSpec
		if !decodeBody(w, r, &d) {
			return
		}
		if err := a.st.setRules(d.Domain, d.Rules); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "domain": normName(d.Domain), "rules": len(d.Rules)})
	})
	mux.HandleFunc("GET /api/smtp/sessions", func(w http.ResponseWriter, r *http.Request) {
		since, ok := sinceParam(w, r)
		if !ok {
			return
		}
		dom := normName(r.URL.Query().Get("domain"))
		a.st.mu.RLock()
		out := []smtpEntry{}
		for _, e := range a.st.smtpLog {
			if e.Seq > since && (dom == "" || e.Domain == dom) {
				out = append(out, e)
			}
		}
		a.st.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/ipify", func(w http.ResponseWriter, r *http.Request) {
		a.st.mu.RLock()
		out := append([]ipifyEntry{}, a.st.ipifyLog...)
		a.st.mu.RUnlock()
		writeJSON(w, http.StatusOK, out)
	})
	return mux
}

func sinceParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	v := strings.TrimSpace(r.URL.Query().Get("since"))
	if v == "" {
		return 0, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, "since must be a non-negative whole number")
		return 0, false
	}
	return n, true
}

// decodeBody reads exactly one JSON object into v; an unknown field, a second value or an oversize body is a 400.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields() // a misspelt field must not read as "no change": it is how a test would silently do nothing
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "the body is not the expected object: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "the body holds more than one JSON value")
		return false
	}
	return true
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
