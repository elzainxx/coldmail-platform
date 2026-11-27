package webapp

// securityHeaders are the headers of every answer of the package, built ONCE from the Supabase origin (`scheme://host[:port]`, already validated by config.ParsePublicURL: a value that could break
// out of a policy source cannot reach here). The policy is the one of Docs/phase-6-spec.md decision 1, written out in full so that a reader can compare it with the spec line by line:
//
//	default-src 'none'      nothing is allowed that is not named below
//	script-src 'self'       only the app's own files run: no inline script, no eval, no other origin
//	style-src 'self' 'unsafe-inline'   inline styles are allowed because the sandboxed preview of a user's HTML needs its own (a srcdoc document inherits the embedder's policy)
//	img-src 'self' https: data:        the app's images, and the images a user's own HTML shows in the preview
//	font-src 'self'
//	connect-src 'self' <origin>        this service (the API) and Supabase Auth, and nothing else
//	frame-src 'self'                   the preview frame
//	base-uri 'none'
//	form-action 'self'
//	frame-ancestors 'none'             nobody frames the app (X-Frame-Options says the same for older browsers)
func securityHeaders(supabaseOrigin string) map[string]string {
	return map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data:; font-src 'self'; " +
			"connect-src 'self' " + supabaseOrigin + "; frame-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
	}
}
