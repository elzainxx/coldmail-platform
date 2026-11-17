package modclient

// Observation is one response the Client received, handed to Options.Observe after the body was fully read and BEFORE the response is judged. It is what the live contract drift
// gate (Phase 3 B6) records in the harness: the real answers of the real modules to the calls the adapters really make, so that no test hand-copies a response.
//
// It is for tests. Path is the path as sent (with the real ids in it), so a production caller must not log an Observation; Route is the stable label of the call
// ("m2.report"), the string a typed error names. Nothing of the request's body, query or headers is in it, and no credential: a response body never carries the platform's tokens.
type Observation struct {
	Module Module
	// Route is the request's route label (Request.Route), the same string the typed errors name. It is NOT the method-and-template a RouteContract names ("GET /v1/jobs/{id}"):
	// Method and Path say which route that is.
	Route string
	// Method and Path are the request's method and path as sent (Request.Method, Request.Path): no base URL prefix and no query.
	Method string
	Path   string
	Status int
	// Body is a copy of the response body (the observer may keep it and change it; the client's own copy is untouched), at most Options.MaxBodyBytes bytes long.
	Body []byte
	// Truncated is true when the module sent more than MaxBodyBytes: Body is its first MaxBodyBytes bytes and the response is judged as an oversize body.
	Truncated bool
}

// observe delivers the response to Options.Observe. It runs in the caller's goroutine, costs nothing when no observer is set, and never lets a panic of the observer reach the
// client: the observer is test instrumentation and must not be able to change what a call returns.
func (c *Client) observe(p *prepared, raw *rawResponse) {
	if c.opts.Observe == nil {
		return
	}
	body := raw.body
	if raw.oversize {
		body = raw.head
	}
	obs := Observation{Module: c.module, Route: p.route, Method: p.method, Path: p.path, Status: raw.status, Truncated: raw.oversize}
	if len(body) > 0 {
		obs.Body = append([]byte(nil), body...)
	}
	defer func() { _ = recover() }()
	c.opts.Observe(obs)
}
