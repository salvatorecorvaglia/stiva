package console_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/console"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func newTestHandler(t *testing.T) *console.Handler {
	t.Helper()
	engine, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("failed to initialize storage engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return console.NewHandler(console.Options{
		Engine:         engine,
		Creds:          auth.NewCredentials("access", "secret"),
		S3Port:         9000,
		Region:         "us-east-1",
		LoginRateLimit: 100,
		APIRateLimit:   1000,
	})
}

// TestCSPForbidsInlineScript guards the fix for the console's stored-XSS
// exposure. The console renders object keys into quoted HTML attributes, so an
// injected on*= handler must not be executable even if escaping regresses.
// 'unsafe-inline' in script-src would make it executable again.
func TestCSPForbidsInlineScript(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	newTestHandler(t).ServeHTTP(w, req)

	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header set")
	}

	var scriptSrc string
	for _, directive := range strings.Split(csp, ";") {
		if d := strings.TrimSpace(directive); strings.HasPrefix(d, "script-src") {
			scriptSrc = d
		}
	}
	if scriptSrc == "" {
		t.Fatalf("CSP has no script-src directive: %q", csp)
	}
	if strings.Contains(scriptSrc, "unsafe-inline") {
		t.Errorf("script-src permits inline scripts, which re-enables injected on*= handlers: %q", scriptSrc)
	}
	if strings.Contains(scriptSrc, "unsafe-eval") {
		t.Errorf("script-src permits eval: %q", scriptSrc)
	}
}

// TestStaticHTMLHasNoInlineScript is the precondition that lets the CSP above
// drop 'unsafe-inline': if someone adds an inline handler or <script> block to
// the page, it will silently stop running rather than fail loudly in CI.
func TestStaticHTMLHasNoInlineScript(t *testing.T) {
	body, err := os.ReadFile("../../internal/console/static/index.html")
	if err != nil {
		t.Fatalf("failed to read index.html: %v", err)
	}
	html := string(body)

	if m := regexp.MustCompile(`(?i)\son(click|change|submit|input|load|error|mouseover|focus|blur|keydown)\s*=`).FindString(html); m != "" {
		t.Errorf("index.html contains an inline event handler (%q); the CSP's script-src 'self' will block it", strings.TrimSpace(m))
	}
	for _, tag := range regexp.MustCompile(`(?is)<script[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("index.html contains an inline <script> block (%q); the CSP's script-src 'self' will block it", tag)
		}
	}
}

// TestEscapeHtmlEscapesQuotes pins the console's escapeHtml to an
// attribute-safe implementation. It previously round-tripped through
// textContent/innerHTML, which escapes only & < > — leaving a double quote in
// an object key free to close the data-key/aria-label attribute it was
// interpolated into and append an event handler.
func TestEscapeHtmlEscapesQuotes(t *testing.T) {
	body, err := os.ReadFile("../../internal/console/static/app.js")
	if err != nil {
		t.Fatalf("failed to read app.js: %v", err)
	}
	js := string(body)

	fn := regexp.MustCompile(`(?s)function escapeHtml\(str\) \{.*?\n    \}`).FindString(js)
	if fn == "" {
		t.Fatal("could not locate escapeHtml in app.js")
	}
	if strings.Contains(fn, "textContent") {
		t.Error("escapeHtml round-trips through textContent/innerHTML, which does not escape quotes")
	}
	for _, want := range []string{"&amp;", "&lt;", "&gt;", "&quot;", "&#39;"} {
		if !strings.Contains(fn, want) {
			t.Errorf("escapeHtml does not produce %s", want)
		}
	}
}
