package webapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
)

// The tests of the serving of the built app (Phase 6 A2). Each property has a twin that differs in exactly that property (a request that IS served beside the one that is refused, a second app with other
// dials beside the first), so a test cannot pass because the handler refuses or serves everything. The files are made in a temporary directory; the one that must never be reachable lies OUTSIDE
// the directory the app is given.

const (
	supabaseOrigin = "https://abcdefgh.supabase.example"
	anonKey        = "anon-anon-anon-anon"

	indexBody    = `<!doctype html><html><head><title>ColdmailApp</title></head><body><div id="root"></div><script type="module" src="/assets/app-abc123.js"></script></body></html>`
	jsBody       = `console.log("the app")`
	cssBody      = `body{margin:0}`
	outsideBody  = "TOPSECRET-OUTSIDE-THE-DIRECTORY"
	dotfileBody  = "DOTFILE-SECRET-IN-THE-DIRECTORY"
	hiddenBody   = "HIDDEN-SCRIPT-IN-THE-DIRECTORY"
	shadowedBody = "SHADOWED-FILE-NAMED-CONFIG-JSON"
)

type site struct {
	root string // the temporary directory: holds the secret and the directory the app serves
	dir  string // the directory the app serves
	app  *App
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newSite builds the directory of a Vite build (and a few files a build never writes, which must not be served) and an App over it.
func newSite(t *testing.T) *site {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "site")
	write(t, filepath.Join(root, "secret.txt"), outsideBody)
	write(t, filepath.Join(dir, "index.html"), indexBody)
	write(t, filepath.Join(dir, "assets", "app-abc123.js"), jsBody)
	write(t, filepath.Join(dir, "assets", "app-abc123.css"), cssBody)
	write(t, filepath.Join(dir, "assets", "font-1.woff2"), "woff2-bytes")
	write(t, filepath.Join(dir, "assets", "Logo.PNG"), "png-bytes")
	write(t, filepath.Join(dir, "assets", "blob.xyz"), "unknown-kind")
	write(t, filepath.Join(dir, "assets", ".hidden.js"), hiddenBody)
	write(t, filepath.Join(dir, "favicon.svg"), "<svg/>")
	write(t, filepath.Join(dir, "robots.txt"), "User-agent: *")
	write(t, filepath.Join(dir, "config.json"), shadowedBody)
	write(t, filepath.Join(dir, ".env"), dotfileBody)
	write(t, filepath.Join(dir, "sub.d", "page.html"), "<p>in a directory with an extension</p>")
	a, err := New(Options{Dir: dir, SupabaseURL: supabaseOrigin, AnonKey: anonKey})
	if err != nil {
		t.Fatalf("New over a good directory: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return &site{root: root, dir: dir, app: a}
}

type answer struct {
	served bool
	header http.Header
	body   string
}

// ask gives the handler the request as the catch-all would: the path is set on the URL exactly as written, with no parsing in between, so a hostile path arrives as it would from the mux.
func ask(a *App, method, path string) answer {
	req := httptest.NewRequest(method, "/", nil)
	req.URL.Path, req.URL.RawPath = path, ""
	rec := httptest.NewRecorder()
	served := a.Serve(rec, req)
	return answer{served: served, header: rec.Header(), body: rec.Body.String()}
}

// askTarget is ask for a request target as a client writes it (percent-encoding and all): the URL's Path is the decoded form, which is what the handler is given.
func askTarget(a *App, method, target string) answer {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	served := a.Serve(rec, req)
	return answer{served: served, header: rec.Header(), body: rec.Body.String()}
}

// wantCSP is the policy of Docs/phase-6-spec.md decision 1, written out here independently of the code that builds it.
func wantCSP(origin string) string {
	return "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: data:; font-src 'self'; connect-src 'self' " + origin + "; frame-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

func TestNew_RefusesWhatItCannotServeWithANameOfTheDialAndNeverAValue(t *testing.T) {
	s := newSite(t)
	file := filepath.Join(s.dir, "robots.txt")
	empty := t.TempDir()
	indexIsDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(indexIsDir, "index.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := Options{Dir: s.dir, SupabaseURL: supabaseOrigin, AnonKey: anonKey}
	with := func(f func(*Options)) Options { o := good; f(&o); return o }
	missing := filepath.Join(s.root, "no-such-directory-XYZ123")

	for _, c := range []struct {
		name string
		o    Options
		want []string // the dials the problems name, in order
	}{
		{"a directory that does not exist", with(func(o *Options) { o.Dir = missing }), []string{config.VarWebDir}},
		{"a path that is a file", with(func(o *Options) { o.Dir = file }), []string{config.VarWebDir}},
		{"a directory with no index.html", with(func(o *Options) { o.Dir = empty }), []string{config.VarWebDir}},
		{"an index.html that is a directory", with(func(o *Options) { o.Dir = indexIsDir }), []string{config.VarWebDir}},
		{"no directory at all", with(func(o *Options) { o.Dir = "" }), []string{config.VarWebDir}},
		{"no Supabase address", with(func(o *Options) { o.SupabaseURL = "" }), []string{config.VarPublicSupabaseURL}},
		{"an address that is not http or https", with(func(o *Options) { o.SupabaseURL = "ftp://supabase-XYZ123.example" }), []string{config.VarPublicSupabaseURL}},
		{"an address with credentials", with(func(o *Options) { o.SupabaseURL = "https://user-XYZ123:pw-XYZ123@supabase.example" }), []string{config.VarPublicSupabaseURL}},
		{"an address that would end the policy's source", with(func(o *Options) { o.SupabaseURL = "https://supabase.example;script-src-XYZ123" }), []string{config.VarPublicSupabaseURL}},
		{"an address with a quote in the host", with(func(o *Options) { o.SupabaseURL = "https://supabase.example'unsafe-eval-XYZ123" }), []string{config.VarPublicSupabaseURL}},
		{"no key", with(func(o *Options) { o.AnonKey = "" }), []string{config.VarPublicSupabaseAnonKey}},
		{"a key with a space", with(func(o *Options) { o.AnonKey = "two words-XYZ123" }), []string{config.VarPublicSupabaseAnonKey}},
		{"everything wrong at once", Options{Dir: missing, SupabaseURL: "nope-XYZ123", AnonKey: "bad key-XYZ123"}, []string{config.VarPublicSupabaseURL, config.VarPublicSupabaseAnonKey, config.VarWebDir}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := New(c.o)
			if a != nil || err == nil {
				t.Fatalf("New accepted %+v", c.o)
			}
			var ce *config.Error
			if !errors.As(err, &ce) {
				t.Fatalf("the refusal is %T, want *config.Error (the service exits with status 2): %v", err, err)
			}
			var got []string
			for _, p := range ce.Problems {
				got = append(got, p.Name)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("problems name %v, want %v", got, c.want)
			}
			for _, secret := range []string{c.o.Dir, c.o.SupabaseURL, c.o.AnonKey} {
				if len(secret) >= 5 && strings.Contains(err.Error(), secret) {
					t.Errorf("the message echoes a value (%q): %v", secret, err)
				}
			}
		})
	}
	// the twin of all of them: the good options build an app, and it serves
	if r := ask(s.app, http.MethodGet, "/"); !r.served || r.body != indexBody {
		t.Fatalf("the control app does not serve its index: %+v", r)
	}
}

func TestServe_EveryAnswerCarriesTheSameFiveHeadersAndTheCacheRuleOfItsKind(t *testing.T) {
	s := newSite(t)
	const html, js, css = "text/html; charset=utf-8", "text/javascript; charset=utf-8", "text/css; charset=utf-8"
	const immutable, noStore, noCache = "public, max-age=31536000, immutable", "no-store", "no-cache"
	for _, c := range []struct {
		name, path   string
		served       bool
		ctype, cache string
	}{
		{"the root", "/", true, html, noStore},
		{"index.html asked for by name", "/index.html", true, html, noStore},
		{"the public config", "/config.json", true, "application/json; charset=utf-8", noStore},
		{"a script of the build", "/assets/app-abc123.js", true, js, immutable},
		{"a style sheet of the build", "/assets/app-abc123.css", true, css, immutable},
		{"a font of the build", "/assets/font-1.woff2", true, "font/woff2", immutable},
		{"a file outside /assets/", "/favicon.svg", true, "image/svg+xml", noCache},
		{"a text file outside /assets/", "/robots.txt", true, "text/plain; charset=utf-8", noCache},
		{"a route of the app", "/jobs", true, html, noStore},
		{"a deep route of the app", "/jobs/7/results", true, html, noStore},
		{"a directory, which is never listed", "/assets/", true, html, noStore},
		{"a file that is not there", "/assets/missing.js", false, "", noStore},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+c.name, func(t *testing.T) {
				r := ask(s.app, method, c.path)
				if r.served != c.served {
					t.Fatalf("served = %v, want %v", r.served, c.served)
				}
				h := r.header
				// the five headers: on every answer, and on the path the app declined (which the platform then answers with its own 404)
				if got := h.Values("Content-Security-Policy"); len(got) != 1 || got[0] != wantCSP(supabaseOrigin) {
					t.Errorf("Content-Security-Policy = %q, want exactly %q", got, wantCSP(supabaseOrigin))
				}
				for name, want := range map[string]string{
					"X-Content-Type-Options": "nosniff",
					"Referrer-Policy":        "no-referrer",
					"X-Frame-Options":        "DENY",
					"Permissions-Policy":     "camera=(), microphone=(), geolocation=()",
					"Cache-Control":          c.cache,
				} {
					if got := h.Values(name); len(got) != 1 || got[0] != want {
						t.Errorf("%s = %q, want exactly %q", name, got, want)
					}
				}
				if c.served {
					if got := h.Get("Content-Type"); got != c.ctype {
						t.Errorf("Content-Type = %q, want %q", got, c.ctype)
					}
					if method == http.MethodHead && r.body != "" {
						t.Errorf("HEAD answered a body of %d bytes", len(r.body))
					}
					if method == http.MethodGet && r.body == "" {
						t.Error("GET answered an empty body")
					}
					if n, err := strconv.Atoi(h.Get("Content-Length")); err != nil || n == 0 {
						t.Errorf("Content-Length = %q", h.Get("Content-Length"))
					}
				}
			})
		}
	}

	// the twin that makes the table mean something: another Supabase origin is another policy, and only the origin changes
	other, err := New(Options{Dir: s.dir, SupabaseURL: "http://gotrue:9999/", AnonKey: anonKey})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got := ask(other, http.MethodGet, "/").header.Get("Content-Security-Policy")
	if got != wantCSP("http://gotrue:9999") || strings.Contains(got, "supabase.example") {
		t.Fatalf("the policy of an app with another origin is %q", got)
	}
	// and the table cannot be satisfied by one cache rule for everything: the three rules above each appear on the file that needs them
	if ask(s.app, http.MethodGet, "/assets/app-abc123.js").header.Get("Cache-Control") == ask(s.app, http.MethodGet, "/").header.Get("Cache-Control") {
		t.Fatal("an asset and the index have the same Cache-Control")
	}
}

func TestServe_ContentTypeFollowsTheExtensionAndAnUnknownOneIsNotRunnable(t *testing.T) {
	s := newSite(t)
	for path, want := range map[string]string{
		"/assets/app-abc123.js":  "text/javascript; charset=utf-8",
		"/assets/app-abc123.css": "text/css; charset=utf-8",
		"/assets/font-1.woff2":   "font/woff2",
		"/assets/Logo.PNG":       "image/png", // the extension in another letter case
		"/favicon.svg":           "image/svg+xml",
		"/robots.txt":            "text/plain; charset=utf-8",
		"/assets/blob.xyz":       "application/octet-stream", // not in the table: a download, never a document
	} {
		if r := ask(s.app, http.MethodGet, path); !r.served || r.header.Get("Content-Type") != want || r.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: served %v, Content-Type %q (want %q), nosniff %q", path, r.served, r.header.Get("Content-Type"), want, r.header.Get("X-Content-Type-Options"))
		}
	}
	for ext, want := range map[string]string{".html": "text/html; charset=utf-8", ".mjs": "text/javascript; charset=utf-8", ".json": "application/json; charset=utf-8", ".woff": "font/woff",
		".webmanifest": "application/manifest+json; charset=utf-8", ".ico": "image/x-icon", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".map": "application/json; charset=utf-8"} {
		if got := contentType("x" + ext); got != want {
			t.Errorf("contentType(%q) = %q, want %q", ext, got, want)
		}
	}
	if contentType("x") != "application/octet-stream" || contentType("x.") != "application/octet-stream" {
		t.Error("a name with no extension is not application/octet-stream")
	}
}

func TestServe_ARouteOfTheAppIsTheIndexAndAFileThatIsNotThereIsNotFound(t *testing.T) {
	s := newSite(t)
	for _, p := range []string{"/", "/jobs", "/jobs/", "/jobs/0b000000-0000-4000-8000-000000000001", "/new/send", "/settings", "/some.dir/", "/assets", "/assets/", "/sub.d/", "/v1abc"} {
		r := ask(s.app, http.MethodGet, p)
		if !r.served || r.body != indexBody {
			t.Errorf("GET %s: served %v, body %q; want index.html", p, r.served, r.body)
		}
		for _, leaked := range []string{"favicon.svg", "robots.txt", "sub.d", "blob.xyz", "font-1.woff2"} { // a directory listing would name its files
			if strings.Contains(r.body, leaked) {
				t.Errorf("GET %s lists %s", p, leaked)
			}
		}
	}
	// a path with an extension is a file or nothing: a missing script that answered HTML would be a script error with no cause
	for _, p := range []string{"/assets/nope.js", "/nope.css", "/a/b/c.png", "/jobs.json", "/sub.d", "/assets/app-abc123.js.map"} {
		if r := ask(s.app, http.MethodGet, p); r.served || r.body != "" {
			t.Errorf("GET %s: served %v with %q, want it declined (the platform answers 404)", p, r.served, r.body)
		}
	}
	// only GET and HEAD: every other verb is declined before anything is written, so the platform's own answer for it is exactly what it always was
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		for _, p := range []string{"/", "/jobs", "/config.json", "/assets/app-abc123.js"} {
			if r := ask(s.app, m, p); r.served || len(r.header) != 0 || r.body != "" {
				t.Errorf("%s %s: served %v with headers %v", m, p, r.served, r.header)
			}
		}
	}
	// and the twin: the same paths with GET and HEAD are served
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if r := ask(s.app, m, "/jobs"); !r.served {
			t.Errorf("%s /jobs was not served", m)
		}
	}
}

func TestServe_ANamedFileIsReadAtEachRequestAndAMissingIndexIsNotInvented(t *testing.T) {
	s := newSite(t)
	if r := ask(s.app, http.MethodGet, "/jobs"); r.body != indexBody {
		t.Fatalf("the starting index is %q", r.body)
	}
	write(t, filepath.Join(s.dir, "index.html"), "<!doctype html><title>second build</title>")
	if r := ask(s.app, http.MethodGet, "/jobs"); r.body != "<!doctype html><title>second build</title>" {
		t.Fatalf("a re-deployed index.html was not picked up: %q", r.body)
	}
	if err := os.Remove(filepath.Join(s.dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/jobs", "/index.html"} {
		if r := ask(s.app, http.MethodGet, p); r.served {
			t.Errorf("GET %s was served with no index.html: %q", p, r.body)
		}
	}
	// the files that are still there are, and /config.json needs no file
	if r := ask(s.app, http.MethodGet, "/assets/app-abc123.js"); !r.served || r.body != jsBody {
		t.Errorf("an asset is not served once the index is gone: %+v", r)
	}
	if r := ask(s.app, http.MethodGet, "/config.json"); !r.served || !strings.Contains(r.body, anonKey) {
		t.Errorf("/config.json needs the index: %+v", r)
	}
}

func TestServe_NothingOutsideTheDirectoryAndNoDotfileIsReachable(t *testing.T) {
	s := newSite(t)
	// the control that makes the table mean something: the secret IS where a traversal would reach it, so only the handler's refusal keeps it back
	if b, err := os.ReadFile(filepath.Join(s.dir, "assets", "..", "..", "secret.txt")); err != nil || string(b) != outsideBody {
		t.Fatalf("control failed: the secret is not at the end of the traversal (%q, %v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(s.dir, ".env")); err != nil || string(b) != dotfileBody {
		t.Fatalf("control failed: the dotfile is not there (%q, %v)", b, err)
	}

	hostile := []string{ // as the handler is given them (decoded), written exactly
		"/../secret.txt", "/..", "/../", "/assets/../../secret.txt", "/assets/../index.html", "/./index.html", "/assets/./app-abc123.js",
		"/assets/..\\..\\secret.txt", "/..\\secret.txt", "/assets\\app-abc123.js", "\\..\\secret.txt",
		"//secret.txt", "//", "/assets//app-abc123.js", "/assets/app-abc123.js//",
		"/.env", "/assets/.hidden.js", "/.git/config", "/.git/", "/a/.b/c.js", "/.well-known/x.txt", "/.",
		"/assets/app-abc123.js\x00.png", "/assets/\x00", "/assets/app-abc123.js\n", "/assets/app\x1bc123.js", "/assets/app-abc123.js\x7f",
		"secret.txt", "", "../secret.txt",
		"/" + strings.Repeat("a/", 2000) + "x.js",
	}
	for _, p := range hostile {
		r := ask(s.app, http.MethodGet, p)
		if r.served || r.body != "" {
			t.Errorf("GET %q was served: %q", p, r.body)
		}
		for _, leaked := range []string{outsideBody, dotfileBody, hiddenBody} {
			if strings.Contains(r.body, leaked) {
				t.Errorf("GET %q leaked %q", p, leaked)
			}
		}
	}
	// the same refusals when the request is written the way a client writes it (percent-encoded): the URL parser decodes it, and the handler is given the decoded path
	for _, target := range []string{"/%2e%2e/secret.txt", "/assets/%2e%2e/%2e%2e/secret.txt", "/..%2fsecret.txt", "/assets/..%2f..%2fsecret.txt", "/%2e%2e%2fsecret.txt", "/assets/%2E%2E/secret.txt",
		"/%2eenv", "/assets/%2ehidden.js", "/assets%5c..%5c..%5csecret.txt", "/assets/app-abc123.js%00.png", "/assets/app-abc123.js%0a"} {
		r := askTarget(s.app, http.MethodGet, target)
		if r.served || r.body != "" {
			t.Errorf("GET %s was served: %q", target, r.body)
		}
	}
	// the twins: the same names without the one thing that is wrong with them are served, so the refusals above are about that thing
	for path, want := range map[string]string{"/assets/app-abc123.js": jsBody, "/favicon.svg": "<svg/>", "/robots.txt": "User-agent: *"} {
		if r := ask(s.app, http.MethodGet, path); !r.served || r.body != want {
			t.Errorf("GET %s: served %v, body %q; want %q", path, r.served, r.body, want)
		}
	}
	if r := askTarget(s.app, http.MethodGet, "/assets/app-abc123.js"); !r.served || r.body != jsBody {
		t.Errorf("the encoded twin of the traversal targets is not served: %+v", r)
	}
	// a directory with an extension is not a file, and a file that is not a regular one is not served
	if r := ask(s.app, http.MethodGet, "/sub.d"); r.served {
		t.Errorf("a directory was served: %q", r.body)
	}
}

func TestServe_ASymbolicLinkThatLeavesTheDirectoryIsNotFollowed(t *testing.T) {
	s := newSite(t)
	outsideDir := filepath.Join(s.root, "outside")
	write(t, filepath.Join(outsideDir, "x.txt"), outsideBody)
	// link inside the directory -> its target. The links that stay inside are RELATIVE (os.Root follows a link only when its target is inside the directory and is not an absolute path);
	// the ones that leave are written both ways, absolute and relative, because each is a way out.
	links := map[string]string{
		filepath.Join(s.dir, "assets", "escape-abs.js"): filepath.Join(s.root, "secret.txt"),
		filepath.Join(s.dir, "assets", "escape-rel.js"): filepath.Join("..", "..", "secret.txt"),
		filepath.Join(s.dir, "linkdir-abs.d"):           outsideDir,
		filepath.Join(s.dir, "linkdir-rel.d"):           filepath.Join("..", "outside"),
		filepath.Join(s.dir, "assets", "inside.js"):     "app-abc123.js",
		filepath.Join(s.dir, "assets", "sibling.js"):    filepath.Join("..", "assets", "app-abc123.js"),
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			// a test that cannot run FAILS, it never skips (CLAUDE.md rule 1): a machine that cannot make a symbolic link (an unprivileged Windows) cannot prove this property, and the runner can
			t.Fatalf("cannot create a symbolic link on this machine, so the property cannot be tested here: %v", err)
		}
	}
	for _, p := range []string{"/assets/escape-abs.js", "/assets/escape-rel.js", "/linkdir-abs.d/x.txt", "/linkdir-rel.d/x.txt"} {
		r := ask(s.app, http.MethodGet, p)
		if r.served || strings.Contains(r.body, outsideBody) {
			t.Errorf("GET %s followed a link out of the directory: served %v, %q", p, r.served, r.body)
		}
	}
	// the twins: a link to a file that is inside the directory stays inside, and is served
	for _, p := range []string{"/assets/inside.js", "/assets/sibling.js"} {
		if r := ask(s.app, http.MethodGet, p); !r.served || r.body != jsBody {
			t.Errorf("GET %s: a link to a file inside the directory was refused: %+v", p, r)
		}
	}
}

func TestServe_ConfigJSONIsTheTwoPublicDialsAndShadowsAFileOfThatName(t *testing.T) {
	s := newSite(t)
	r := ask(s.app, http.MethodGet, "/config.json")
	if !r.served {
		t.Fatal("/config.json was not served")
	}
	if want := `{"supabase_url":"` + supabaseOrigin + `","anon_key":"` + anonKey + `"}`; r.body != want {
		t.Fatalf("/config.json = %s, want %s", r.body, want)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(r.body), &m); err != nil || len(m) != 2 || m["supabase_url"] != supabaseOrigin || m["anon_key"] != anonKey {
		t.Fatalf("/config.json is not exactly the two members: %v (%v)", m, err)
	}
	if strings.Contains(r.body, shadowedBody) {
		t.Fatal("a file named config.json in the directory was served instead of the dials")
	}
	// a HEAD says the same without the body; a path that is not exactly /config.json is not it
	if h := ask(s.app, http.MethodHead, "/config.json"); !h.served || h.body != "" || h.header.Get("Content-Length") != strconv.Itoa(len(r.body)) {
		t.Fatalf("HEAD /config.json = %+v", h)
	}
	if other := ask(s.app, http.MethodGet, "/config.json/"); !other.served || other.body != indexBody {
		t.Fatalf("/config.json/ is the app's route, not the config: %+v", other)
	}
	// the twin: another app with other dials gives other values, and a trailing slash of the address is dropped
	a2, err := New(Options{Dir: s.dir, SupabaseURL: "http://gotrue:9999/auth//", AnonKey: "anon-anon-anon-two"})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if got := ask(a2, http.MethodGet, "/config.json").body; got != `{"supabase_url":"http://gotrue:9999/auth","anon_key":"anon-anon-anon-two"}` {
		t.Fatalf("the second app's /config.json = %s", got)
	}
	// the anon key is in the config and NOWHERE else of what the app sends
	for _, p := range []string{"/", "/jobs", "/assets/app-abc123.js"} {
		x := ask(s.app, http.MethodGet, p)
		if strings.Contains(x.body, anonKey) {
			t.Errorf("GET %s carries the key", p)
		}
		for name, vs := range x.header {
			if strings.Contains(strings.Join(vs, " "), anonKey) {
				t.Errorf("GET %s carries the key in %s", p, name)
			}
		}
	}
}
