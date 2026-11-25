package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The caps of a request, from Docs/jobs-api.md: a create may carry a whole list (64 MiB), every other route a small JSON object or nothing (64 KiB), and a CSV create carries its campaign in
// the query (64 KiB in all). A body larger than its cap is a 413 `payload_too_large`, whether the client said so in Content-Length or only showed it by sending more.
const (
	maxCreateBody int64 = 64 << 20
	maxSmallBody  int64 = 64 << 10
	maxCSVQuery         = 64 << 10
)

// The windows a request has to send its body in and a client has to take a streamed answer in, per page. The server has no read or write timeout of its own (a download of a whole list is
// long); these are per request, so a client that stalls cannot hold a connection (and, for a create, up to 64 MiB of buffer) for ever. They are variables only so that a test can shorten them;
// nothing else assigns them.
var (
	createBodyWindow = 2 * time.Minute
	smallBodyWindow  = 15 * time.Second
	csvPageWindow    = time.Minute
)

// extendReadDeadline gives the request d to deliver its body and returns the function that ends the window: the caller defers it, so that once the body has been read the connection has no read
// deadline left (the server watches the connection for a client that goes away, and a deadline that ran out while the store was still working would end the request in the middle of it). A writer
// that cannot take a deadline (a test's recorder) is left alone.
func extendReadDeadline(w http.ResponseWriter, d time.Duration) (done func()) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(d))
	return func() { _ = rc.SetReadDeadline(time.Time{}) }
}

// mediaType is the lower-cased media type of the request's Content-Type ("" when there is none or it does not parse).
func mediaType(r *http.Request) string {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return mt
}

// wantMedia refuses (415) a body that is not of the media type, and says whether the request may go on.
func (a *API) wantMedia(w http.ResponseWriter, r *http.Request, want ...string) bool {
	got := mediaType(r)
	for _, m := range want {
		if got == m {
			return true
		}
	}
	a.refuseRequest(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMedia, "send the body as "+strings.Join(want, " or ")+" with that Content-Type")
	return false
}

// tooBig refuses a request whose Content-Length already says it is over the cap, before any of it is read.
func (a *API) tooBig(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if r.ContentLength > limit {
		a.fail(w, r, &http.MaxBytesError{Limit: limit})
		return true
	}
	return false
}

// drain reads and discards a body the route does not use, up to the small cap, so that the cap holds for every route and a client that sends a megabyte to /cancel is told so.
func (a *API) drain(w http.ResponseWriter, r *http.Request) bool {
	if a.tooBig(w, r, maxSmallBody) {
		return false
	}
	defer extendReadDeadline(w, smallBodyWindow)()
	if _, err := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxSmallBody)); err != nil {
		a.badBody(w, r, err)
		return false
	}
	return true
}

var (
	plainName = regexp.MustCompile(`^[A-Za-z0-9_]{1,40}$`)
	fieldPath = regexp.MustCompile(`^[A-Za-z0-9_.]{1,80}$`)
)

// badBody answers a body that could not be read as what the route wants: over the cap is 413, anything else is a 400 `bad_request` whose reason names what is wrong in words of the API's own
// (never the client's text, except an unknown field's name when it is a plain identifier, which is what a developer needs to see).
func (a *API) badBody(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		a.fail(w, r, err)
	case errors.Is(err, io.EOF):
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body is empty")
	case errors.As(err, &syn):
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body is not valid JSON (byte "+strconv.FormatInt(syn.Offset, 10)+")")
	case errors.Is(err, io.ErrUnexpectedEOF):
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body is not valid JSON (it ends too soon)")
	case errors.As(err, &typ):
		field := typ.Field
		if field == "" || !fieldPath.MatchString(field) {
			field = "a member"
		}
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, field+" has the wrong type")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name, _ := strconv.Unquote(strings.TrimPrefix(err.Error(), "json: unknown field "))
		if plainName.MatchString(name) {
			a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body has a member the API does not know: "+name)
		} else {
			a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body has a member the API does not know")
		}
	default:
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body could not be read")
	}
}

// readJSON decodes the request's body, which must be application/json no larger than limit, into dst: one JSON value, no member dst does not name (at any depth), nothing after it. It
// answers the failure itself and says whether the request may go on.
func (a *API) readJSON(w http.ResponseWriter, r *http.Request, limit int64, window time.Duration, dst any) bool {
	if !a.wantMedia(w, r, "application/json") || a.tooBig(w, r, limit) {
		return false
	}
	defer extendReadDeadline(w, window)()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		a.badBody(w, r, err)
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.badBody(w, r, err)
			return false
		}
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the body holds something after the JSON value")
		return false
	}
	return true
}

// query returns the request's query parameters after checking that every name is one the route knows and that none is repeated: an unknown parameter is a 400, because a parameter that
// is ignored is a parameter a client believes in (`user_id` above all). An empty value is the same as an absent one.
func (a *API) query(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, bool) {
	vals, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the query string is not valid")
		return nil, false
	}
	known := map[string]bool{}
	for _, n := range allowed {
		known[n] = true
	}
	for name, vs := range vals {
		switch {
		case !known[name]:
			if plainName.MatchString(name) {
				a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the query has a parameter this route does not know: "+name)
			} else {
				a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the query has a parameter this route does not know")
			}
			return nil, false
		case len(vs) > 1:
			a.refuseRequest(w, r, http.StatusBadRequest, codeBadRequest, "the query repeats the parameter "+name)
			return nil, false
		}
	}
	return vals, true
}

// intParam reads an integer parameter in lo..hi; present is false for an absent or empty one. A value that is not an integer, or is outside the range, is refused (400 validation_failed).
func (a *API) intParam(w http.ResponseWriter, r *http.Request, vals url.Values, name string, lo, hi int64) (n int64, present, ok bool) {
	raw := vals.Get(name)
	if raw == "" {
		return 0, false, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < lo || n > hi {
		a.refuseRequest(w, r, http.StatusBadRequest, codeValidationFailed, name+" must be a whole number from "+strconv.FormatInt(lo, 10)+" to "+strconv.FormatInt(hi, 10))
		return 0, false, false
	}
	return n, true, true
}
