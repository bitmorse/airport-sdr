package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type oembedResponse struct {
	Version      string `json:"version"`
	Type         string `json:"type"`
	ProviderName string `json:"provider_name"`
	Title        string `json:"title"`
	HTML         string `json:"html"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

// oembed requests a document for the given target URL, optionally with headers
// that mimic a reverse proxy.
func oembed(t *testing.T, srv *Server, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/oembed?"+query, nil)
	req.Host = "receiver.example"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeOEmbed(t *testing.T, rec *httptest.ResponseRecorder) oembedResponse {
	t.Helper()
	var doc oembedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	return doc
}

func TestOEmbedReturnsARichDocument(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	rec := oembed(t, srv, "url=http://receiver.example/embed/Tower&format=json", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("oembed = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want json", ct)
	}

	doc := decodeOEmbed(t, rec)
	if doc.Version != "1.0" || doc.Type != "rich" {
		t.Errorf("version/type = %q/%q, want 1.0/rich", doc.Version, doc.Type)
	}
	if doc.Width != 280 || doc.Height != 64 {
		t.Errorf("size = %dx%d, want the configured 280x64", doc.Width, doc.Height)
	}
	if !strings.Contains(doc.HTML, "<iframe") || !strings.Contains(doc.HTML, "/embed/Tower") {
		t.Errorf("html is not an iframe for the channel: %s", doc.HTML)
	}
	if !strings.Contains(doc.Title, "Tower") {
		t.Errorf("title = %q, should name the channel", doc.Title)
	}
}

// The iframe URL has to be absolute, and correct behind a proxy. Tailscale
// Serve terminates TLS and forwards plain http, so trusting r.TLS alone would
// advertise http:// on an https deployment.
func TestOEmbedURLIsAbsoluteAndRespectsForwardedProto(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	rec := oembed(t, srv,
		"url=https://receiver.example/embed/Tower",
		map[string]string{"X-Forwarded-Proto": "https"})

	doc := decodeOEmbed(t, rec)
	if !strings.Contains(doc.HTML, "https://receiver.example/embed/Tower") {
		t.Errorf("iframe src should be an absolute https URL: %s", doc.HTML)
	}
}

func TestOEmbedRejectsXMLFormat(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	rec := oembed(t, srv, "url=http://receiver.example/embed/Tower&format=xml", nil)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("format=xml = %d, want 501", rec.Code)
	}
}

func TestOEmbedRejectsUnknownTargets(t *testing.T) {
	srv := embedServer(t, "https://example.com")

	cases := map[string]string{
		"unknown channel": "url=http://receiver.example/embed/Nowhere",
		"another host":    "url=http://elsewhere.example/embed/Tower",
		"not an embed":    "url=http://receiver.example/api/status",
		"malformed":       "url=%zz",
		"missing":         "format=json",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := oembed(t, srv, query, nil); rec.Code == http.StatusOK {
				t.Errorf("%s returned 200, want a failure", name)
			}
		})
	}
}

func TestOEmbedHonoursMaxDimensions(t *testing.T) {
	srv := embedServer(t, "https://example.com")
	rec := oembed(t, srv,
		"url=http://receiver.example/embed/Tower&maxwidth=200&maxheight=40", nil)

	doc := decodeOEmbed(t, rec)
	if doc.Width > 200 || doc.Height > 40 {
		t.Errorf("size = %dx%d, want it clamped to 200x40", doc.Width, doc.Height)
	}
}

func TestOEmbedIsUnavailableWhenEmbeddingIsOff(t *testing.T) {
	srv := embedServer(t)
	rec := oembed(t, srv, "url=http://receiver.example/embed/Tower", nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("oembed with embedding disabled = %d, want 404", rec.Code)
	}
}

// Consumers find the endpoint from a discovery link on the page itself.
func TestEmbedPageAdvertisesOEmbed(t *testing.T) {
	srv := embedServer(t, "https://example.com")

	req := httptest.NewRequest(http.MethodGet, "/embed/Tower", nil)
	req.Host = "receiver.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `type="application/json+oembed"`) {
		t.Fatal("no oEmbed discovery link on the embed page")
	}
	if !strings.Contains(body, "https://receiver.example/oembed") {
		t.Errorf("discovery href should be absolute and https: %s", body)
	}
}
