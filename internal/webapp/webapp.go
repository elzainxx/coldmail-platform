// Package webapp serves the built web app (Phase 6, chunk A2; Docs/phase-6-spec.md decision 1): the one Go service answers the app and the API from one origin, so there is no CORS and no second
// server. It is switched on by PLATFORM_WEB_DIR (internal/config, web.go); with the dial unset nothing in this package is built and the service behaves exactly as it did.
//
// WHAT IT SERVES. A GET or HEAD of a path that is not the platform's own (internal/httpapi decides: `/v1/`, `/u/`, `/healthz` and `/readyz` are never offered to this package):
//
//   - `/config.json`: {"supabase_url","anon_key"}, the two PUBLIC dials the browser signs in with (an anon key is a publishable key, shown to every visitor by design). It shadows a file of that name;
//   - a path with a file extension: that file of the directory, when it is a regular file under it, and nothing else (a missing file is the platform's own 404, never index.html: a missing script that
//     answered HTML would be a script error with no cause);
//   - every other path (no extension, a trailing slash, the root): `index.html`, so the app's own router can take the URL. A directory is never listed.
//
// WHAT IT NEVER SERVES. A path with a `..` or `.` segment, an empty segment in the middle, a backslash or a control character; a segment that starts with a dot (`.env`, `.git/`, `.well-known/`); a
// directory; and anything that is not inside the directory: the files are opened through os.Root, which refuses a symbolic link whose target is outside it (a link to a file inside it is served: it is
// still inside). Every refusal is `Serve` returning false, which the caller answers with its own 404 envelope, so a probe learns nothing about what exists.
//
// THE HEADERS. Every answer of this package carries the same five (headers.go), built once: the Content-Security-Policy of decision 1 with the Supabase origin of PLATFORM_PUBLIC_SUPABASE_URL in
// connect-src, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY` and a Permissions-Policy that denies the camera, the microphone and geolocation. They are set
// before anything is decided, so the platform's 404 for a path this package did not serve has them too. Cache-Control: `no-store` on index.html, on /config.json and on every fallback (and so by
// default); `public, max-age=31536000, immutable` on a file under /assets/ (the build gives those a content hash in the name); `no-cache` on any other file.
//
// WHAT IT DOES NOT DO. It has no state: the files are read at each request (a re-deployed directory is picked up), it never writes, and it logs nothing (the access log of internal/httpapi records the
// path and the status). It does not compress, and it does not answer any method but GET and HEAD (the caller's catch-all answers those exactly as it always did).
package webapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/coldmail-platform/platform/internal/config"
)

// Options are what the app is built from: the three dials, as the configuration loaded them.
type Options struct {
	Dir         string // PLATFORM_WEB_DIR
	SupabaseURL string // PLATFORM_PUBLIC_SUPABASE_URL
	AnonKey     string // PLATFORM_PUBLIC_SUPABASE_ANON_KEY
}

// App serves the built web app. It holds nothing that changes after New, so it is safe for concurrent use.
type App struct {
	root       *os.Root
	headers    map[string]string // the five headers every answer carries, built once
	configJSON []byte            // the body of /config.json, built once
}

// New opens the directory and builds the headers and /config.json. Anything it cannot serve with is a *config.Error that names the dial (the service then exits with status 2) and never a value:
// the directory missing, not a directory or without an index.html, an address or a key the dials' own rules refuse (they are judged here again, so that a caller that did not come through
// config.Load cannot put a character into the policy that the rules keep out).
func New(o Options) (*App, error) {
	var problems []config.Problem
	bad := func(name, reason string) { problems = append(problems, config.Problem{Name: name, Reason: reason}) }

	full, origin, ok := config.ParsePublicURL(o.SupabaseURL)
	if !ok {
		if o.SupabaseURL == "" {
			bad(config.VarPublicSupabaseURL, "is required when "+config.VarWebDir+" is set: the browser needs the Supabase address to sign in")
		} else {
			bad(config.VarPublicSupabaseURL, "must be an http or https URL with a host name, an optional port and an optional path, no credentials, query or fragment")
		}
	}
	if !config.ValidAnonKey(o.AnonKey) {
		if o.AnonKey == "" {
			bad(config.VarPublicSupabaseAnonKey, "is required when "+config.VarWebDir+" is set: the browser needs the Supabase public key to sign in")
		} else {
			bad(config.VarPublicSupabaseAnonKey, "must be printable ASCII with no spaces")
		}
	}
	var root *os.Root
	if o.Dir == "" {
		bad(config.VarWebDir, "must name the directory of the built web app")
	} else {
		var err error
		if root, err = os.OpenRoot(o.Dir); err != nil {
			bad(config.VarWebDir, "must name an existing directory that the service can read")
		} else if fi, err := root.Stat("index.html"); err != nil || !fi.Mode().IsRegular() {
			_ = root.Close()
			root = nil
			bad(config.VarWebDir, "must hold an index.html: it is the directory the web app was built into (web/dist)")
		}
	}
	if len(problems) > 0 {
		if root != nil {
			_ = root.Close()
		}
		return nil, &config.Error{Problems: problems}
	}
	body, err := json.Marshal(struct {
		SupabaseURL string `json:"supabase_url"`
		AnonKey     string `json:"anon_key"`
	}{full, o.AnonKey})
	if err != nil { // two strings: not reachable
		_ = root.Close()
		return nil, err
	}
	return &App{root: root, headers: securityHeaders(origin), configJSON: body}, nil
}

// Close releases the directory. The service never needs to: it holds the directory for its whole life.
func (a *App) Close() error { return a.root.Close() }

// Serve answers the request when it is a GET or HEAD it can answer, and says whether it did. When it returns false nothing has been written (only headers have been set, on a GET or HEAD) and the
// caller answers as it would for any path no route took.
func (a *App) Serve(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	h := w.Header()
	for k, v := range a.headers {
		h.Set(k, v)
	}
	h.Set("Cache-Control", "no-store")

	p := r.URL.Path
	if p == "/config.json" {
		h.Set("Content-Type", "application/json; charset=utf-8")
		http.ServeContent(w, r, "config.json", zeroTime, bytes.NewReader(a.configJSON))
		return true
	}
	segs, dir, ok := segments(p)
	if !ok {
		return false
	}
	if len(segs) == 0 || dir || path.Ext(segs[len(segs)-1]) == "" {
		return a.serveIndex(w, r) // the app's own route: the root, a path with a trailing slash, a path with no extension
	}
	name := strings.Join(segs, "/")
	if name == "index.html" {
		return a.serveIndex(w, r)
	}
	return a.serveFile(w, r, name)
}

// maxPath is the longest path this package looks at; anything longer is not a path of the app.
const maxPath = 2048

// zeroTime is the modification time given to http.ServeContent for an answer that must not be revalidated (it sets no Last-Modified and answers no conditional request with a 304).
var zeroTime time.Time

// segments splits a request path into its names. ok is false for a path that must never reach the file system: one that does not start with `/`, is longer than maxPath, holds a control character or a
// backslash, has an empty segment anywhere but the end, has a `.` or `..` segment, or has a segment that starts with a dot. dir is true when the path ends in `/` (the root has no segments and is
// not dir).
func segments(p string) (segs []string, dir, ok bool) {
	if p == "" || p[0] != '/' || len(p) > maxPath {
		return nil, false, false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return nil, false, false
		}
	}
	if p == "/" {
		return nil, false, true
	}
	parts := strings.Split(p[1:], "/")
	if parts[len(parts)-1] == "" {
		dir = true
		parts = parts[:len(parts)-1]
	}
	for _, s := range parts {
		if s == "" || s[0] == '.' { // an empty segment ("//"), ".", ".." and every dotfile
			return nil, false, false
		}
	}
	return parts, dir, true
}

// serveIndex answers with index.html, which is never cached.
func (a *App) serveIndex(w http.ResponseWriter, r *http.Request) bool {
	f, err := a.root.Open("index.html")
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "index.html", zeroTime, f) // no modification time: nothing is revalidated, a fallback is always the current index.html
	return true
}

// serveFile answers with one file of the directory, or returns false (a missing file, a directory, anything that is not a regular file, a link that leaves the directory).
func (a *App) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := a.root.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", contentType(name))
	if strings.HasPrefix(name, "assets/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, fi.ModTime(), f)
	return true
}

// contentTypes are the types of the files a Vite build writes, per extension. The table is the package's own so that a type never depends on the host's mime database (a minimal image has none), and a
// type that is not listed is application/octet-stream, which a browser downloads and never runs (with nosniff).
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".webmanifest": "application/manifest+json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".wasm":        "application/wasm",
}

// contentType is the type of a file by its extension (in any letter case).
func contentType(name string) string {
	if t, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
