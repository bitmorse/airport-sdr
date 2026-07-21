package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bitmorse/airport-sdr/internal/stream"
)

// embedServer builds a server whose embedding is limited to the given origins.
// No origins means embedding is disabled, which is the default everywhere.
func embedServer(t *testing.T, origins ...string) *Server {
	t.Helper()

	srv, err := NewServer(Options{
		SourceDescription: "test source",
		Channels: []ChannelInfo{
			{Name: "Tower", Group: "Alpha", Freq: 118_100_000,
				AudioRate: 8000, Hub: stream.NewHub(8, 160)},
		},
		Embed: EmbedOptions{AllowedOrigins: origins, Width: 280, Height: 64},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

func getEmbed(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Framing the receiver puts another site's visitors on your radio, so nothing
// is embeddable until an operator lists an origin.
func TestEmbedIsDisabledWithoutAllowedOrigins(t *testing.T) {
	srv := embedServer(t)
	if rec := getEmbed(t, srv, "/embed/Tower"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /embed/Tower with embedding off = %d, want 404", rec.Code)
	}
}

func TestEmbedUnknownChannelIs404(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	if rec := getEmbed(t, srv, "/embed/Nowhere"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel = %d, want 404", rec.Code)
	}
}

func TestEmbedServesThePlayer(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	rec := getEmbed(t, srv, "/embed/Tower")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /embed/Tower = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), `data-channel="Tower"`) {
		t.Error("the page does not carry its channel name")
	}
}

// frame-ancestors is what actually stops an unlisted site rendering the frame;
// the origin check alone would only silence the messaging.
func TestEmbedSetsFrameAncestorsFromConfig(t *testing.T) {
	srv := embedServer(t, "https://a.example", "https://b.example")
	rec := getEmbed(t, srv, "/embed/Tower")

	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors") {
		t.Fatalf("no frame-ancestors directive: %q", csp)
	}
	for _, want := range []string{"https://a.example", "https://b.example"} {
		if !strings.Contains(csp, want) {
			t.Errorf("frame-ancestors is missing %q: %q", want, csp)
		}
	}
}

func TestEmbedWildcardAllowsAnyFramer(t *testing.T) {
	srv := embedServer(t, "*")
	csp := getEmbed(t, srv, "/embed/Tower").Header().Get("Content-Security-Policy")

	if !strings.Contains(csp, "frame-ancestors *") {
		t.Errorf("frame-ancestors = %q, want the wildcard", csp)
	}
}

// The allowlist is checked on the server. The page is handed an already
// validated origin, so a client cannot talk itself into being trusted.
func TestEmbedInjectsOnlyAnAllowedOrigin(t *testing.T) {
	srv := embedServer(t, "https://example.com")

	t.Run("allowed", func(t *testing.T) {
		rec := getEmbed(t, srv, "/embed/Tower?origin=https://example.com")
		if !strings.Contains(rec.Body.String(), `data-origin="https://example.com"`) {
			t.Error("an allowed origin should be injected for postMessage")
		}
	})

	t.Run("not allowed", func(t *testing.T) {
		rec := getEmbed(t, srv, "/embed/Tower?origin=https://evil.example")
		if rec.Code != http.StatusOK {
			t.Fatalf("page should still render, got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "evil.example") {
			t.Error("a disallowed origin must never be injected")
		}
		if !strings.Contains(rec.Body.String(), `data-origin=""`) {
			t.Error("the page should carry an empty origin so it reports origin-not-allowed")
		}
	})

	t.Run("wildcard trusts the caller", func(t *testing.T) {
		any := embedServer(t, "*")
		rec := getEmbed(t, any, "/embed/Tower?origin=https://anyone.example")
		if !strings.Contains(rec.Body.String(), `data-origin="https://anyone.example"`) {
			t.Error("with a wildcard allowlist any origin should be accepted")
		}
	})
}

func TestEmbedPassesThroughPlayerOptions(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	body := getEmbed(t, srv, "/embed/Tower?muted=1&theme=dark").Body.String()

	for _, want := range []string{`data-muted="1"`, `data-theme="dark"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %s", want)
		}
	}
}

// The player never starts on its own. Audio begins only when someone asks for
// it: a click on the button, or a play command from the host page.
func TestEmbedNeverAutoplays(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	body := getEmbed(t, srv, "/embed/Tower?autoplay=1").Body.String()

	if strings.Contains(body, "data-autoplay") {
		t.Error("the page still carries an autoplay setting")
	}
}

// A channel name reaches the page from config, but the query string does not —
// and neither should ever be able to inject markup.
func TestEmbedEscapesInjectedValues(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	rec := getEmbed(t, srv, "/embed/Tower?theme=%22%3E%3Cscript%3Ealert(1)%3C/script%3E")

	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Error("a query parameter was injected into the page unescaped")
	}
}

// The player shows how many people are on the channel, and emits the count so a
// host page can show its own.
func TestEmbedShowsListenerCount(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	body := getEmbed(t, srv, "/embed/Tower").Body.String()

	if !strings.Contains(body, `id="listeners"`) {
		t.Error("the player has nowhere to show the listener count")
	}
}

func TestEmbedLoadsTheSharedPlayer(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	body := getEmbed(t, srv, "/embed/Tower").Body.String()

	// The embed must use the same audio pipeline as the main page rather than
	// carrying a second copy of it.
	if !strings.Contains(body, "/static/player.js") {
		t.Error("the embed page does not load the shared player")
	}
}
