package app

import (
	"log/slog"
	"testing"

	"github.com/coldmail-platform/platform/internal/config"
	"github.com/coldmail-platform/platform/internal/jobs"
)

// Phase 5 S2: the service's job store renders through the template renderer (merge fields, spintax, the generated text part, the footer; Docs/templates.md). jobs.New keeps the identity renderer for the
// tests that want it, so the production wiring is the one place that chooses otherwise, and this test is what stops it from quietly going back: a store built by newJobStore is a template store, and the
// control, a store built by jobs.New with no option, is not.
func TestJobStore_TheServiceRendersThroughTheTemplateRenderer(t *testing.T) {
	pool := lazyPool(t)
	cfg, err := config.Load(envOf(base("postgres://platform_app:x@h/db")))
	if err != nil {
		t.Fatal(err)
	}
	st, err := newJobStore(pool, newMoneyStore(pool, cfg), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if !st.TemplatesEnabled() {
		t.Fatal("the service's job store does not render through the template renderer: a campaign would be sent as written, braces and all")
	}
	plain, err := jobs.New(pool, newMoneyStore(pool, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if plain.TemplatesEnabled() {
		t.Fatal("a store built with no option renders through templates: the control cannot fail")
	}
}
