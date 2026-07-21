package web

import (
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// oEmbed (https://oembed.com) is the discovery standard for embedding: a
// consumer such as WordPress or Discourse fetches a pasted URL, finds the
// discovery link on the page, and asks this endpoint for the markup to use.
//
// Only JSON is served. The spec permits a provider to support one format and
// answer 501 for the other.

const oembedPath = "/oembed"

// oembedDiscoveryURL is the absolute URL advertised by an embed page.
func (s *Server) oembedDiscoveryURL(r *http.Request, channel string) string {
	target := externalURL(r, "/embed/"+url.PathEscape(channel))
	return externalURL(r, oembedPath) + "?url=" + url.QueryEscape(target) + "&format=json"
}

func (s *Server) handleOEmbed(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Embed.Enabled() {
		http.NotFound(w, r)
		return
	}
	if format := r.URL.Query().Get("format"); format != "" && format != "json" {
		http.Error(w, "only format=json is supported", http.StatusNotImplemented)
		return
	}

	ch, ok := s.channelFromEmbedURL(r, r.URL.Query().Get("url"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	width, height := s.opts.Embed.Width, s.opts.Embed.Height
	width = clampDimension(r, "maxwidth", width)
	height = clampDimension(r, "maxheight", height)

	src := externalURL(r, "/embed/"+url.PathEscape(ch.Name))
	iframe := fmt.Sprintf(
		`<iframe src="%s" width="%d" height="%d" allow="autoplay" style="border:0"></iframe>`,
		html.EscapeString(src), width, height)

	writeJSON(w, struct {
		Version      string `json:"version"`
		Type         string `json:"type"`
		ProviderName string `json:"provider_name"`
		ProviderURL  string `json:"provider_url"`
		Title        string `json:"title"`
		HTML         string `json:"html"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
	}{
		Version:      "1.0",
		Type:         "rich",
		ProviderName: "airport-sdr",
		ProviderURL:  externalURL(r, "/"),
		Title:        fmt.Sprintf("%s — %.3f MHz", ch.Name, ch.Freq/1e6),
		HTML:         iframe,
		Width:        width,
		Height:       height,
	})
}

// channelFromEmbedURL resolves the target URL a consumer asked about. It must
// be one of our own embed URLs: a foreign host or an unrelated path is not
// something this provider can describe.
func (s *Server) channelFromEmbedURL(r *http.Request, target string) (ChannelInfo, bool) {
	if target == "" {
		return ChannelInfo{}, false
	}
	u, err := url.Parse(target)
	if err != nil {
		return ChannelInfo{}, false
	}
	if u.Host != "" && !strings.EqualFold(u.Host, r.Host) {
		return ChannelInfo{}, false
	}

	name, found := strings.CutPrefix(u.Path, "/embed/")
	if !found || name == "" {
		return ChannelInfo{}, false
	}
	decoded, err := url.PathUnescape(name)
	if err != nil {
		return ChannelInfo{}, false
	}

	ch, ok := s.byName[decoded]
	return ch, ok
}

// clampDimension applies a consumer's maxwidth or maxheight if it asked for one.
func clampDimension(r *http.Request, param string, value int) int {
	raw := r.URL.Query().Get(param)
	if raw == "" {
		return value
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 || limit >= value {
		return value
	}
	return limit
}
