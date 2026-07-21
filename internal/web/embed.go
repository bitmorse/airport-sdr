package web

import (
	"embed"
	"html/template"
	"net/http"
	"strings"
)

// The embeddable player.
//
// Embedding is done with an iframe rather than by opening the API up
// cross-origin, and that is the whole design. A frame keeps its own origin, so
// code inside it is same-origin with the receiver: the websocket's origin check
// passes untouched and the JSON API needs no CORS headers. An embedder gets the
// low-latency path without a single security control being relaxed.
//
// The player is deliberately listen-only. One tuner serves every listener, so a
// visitor to someone else's site must not be able to retune it.

//go:embed embed.html.tmpl
var embedTemplateFS embed.FS

var embedTemplate = template.Must(template.ParseFS(embedTemplateFS, "embed.html.tmpl"))

// EmbedOptions controls who may frame the player.
type EmbedOptions struct {
	// AllowedOrigins are the sites permitted to frame the player. Empty
	// disables embedding entirely; a single "*" permits any site.
	AllowedOrigins []string
	Width          int
	Height         int
}

// Enabled reports whether any site may embed the player.
func (e EmbedOptions) Enabled() bool { return len(e.AllowedOrigins) > 0 }

// allowsAny reports whether the allowlist is the wildcard.
func (e EmbedOptions) allowsAny() bool {
	return len(e.AllowedOrigins) == 1 && e.AllowedOrigins[0] == "*"
}

// allows reports whether origin may frame the player and be messaged.
func (e EmbedOptions) allows(origin string) bool {
	if origin == "" {
		return false
	}
	if e.allowsAny() {
		return true
	}
	for _, allowed := range e.AllowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// frameAncestors renders the CSP directive that decides which sites the browser
// will render the frame for. This, not the origin check, is what actually stops
// an unlisted site embedding the player.
func (e EmbedOptions) frameAncestors() string {
	if e.allowsAny() {
		return "frame-ancestors *"
	}
	return "frame-ancestors " + strings.Join(e.AllowedOrigins, " ")
}

// embedPage is the data the template renders.
type embedPage struct {
	Channel   string
	Group     string
	Frequency float64
	AudioRate int
	// Origin has already been checked against the allowlist. It is empty when
	// the caller supplied none or supplied one that is not permitted, and the
	// player then sends no messages at all.
	Origin string
	Muted  string
	Theme  string
	OEmbed string
}

func (s *Server) handleEmbed(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Embed.Enabled() {
		http.NotFound(w, r)
		return
	}
	ch, ok := s.byName[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	origin := r.URL.Query().Get("origin")
	if !s.opts.Embed.allows(origin) {
		origin = ""
	}

	page := embedPage{
		Channel:   ch.Name,
		Group:     ch.Group,
		Frequency: ch.Freq,
		AudioRate: ch.AudioRate,
		Origin:    origin,
		Muted:     boolParam(r, "muted"),
		Theme:     themeParam(r),
		OEmbed:    s.oembedDiscoveryURL(r, ch.Name),
	}

	w.Header().Set("Content-Security-Policy", s.opts.Embed.frameAncestors())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := embedTemplate.Execute(w, page); err != nil {
		// The response is already partly written; nothing useful to send.
		return
	}
}

func boolParam(r *http.Request, name string) string {
	if r.URL.Query().Get(name) == "1" {
		return "1"
	}
	return "0"
}

// themeParam constrains the theme to known values, so an arbitrary string never
// reaches the page.
func themeParam(r *http.Request) string {
	switch r.URL.Query().Get("theme") {
	case "dark":
		return "dark"
	case "light":
		return "light"
	default:
		return "auto"
	}
}

// externalURL rebuilds the URL a browser used to reach us.
//
// A reverse proxy such as Tailscale Serve terminates TLS and forwards plain
// http, so the scheme has to come from the forwarded header. Without this the
// oEmbed response would advertise http:// URLs on an https deployment.
func externalURL(r *http.Request, path string) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}

	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	return scheme + "://" + host + path
}
