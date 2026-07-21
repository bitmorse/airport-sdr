// Package web serves the receiver's audio and status to browsers.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bitmorse/airport-sdr/internal/stream"
	"github.com/coder/websocket"
)

//go:embed static
var staticFiles embed.FS

// keepaliveInterval bounds how long a websocket can stay silent. Between
// transmissions the receiver sends no audio at all, so without this a quiet
// channel would look like a dead connection to proxies and NAT tables.
const keepaliveInterval = 20 * time.Second

// ChannelState is a snapshot of one channel, read live for /api/status.
type ChannelState struct {
	LevelDB     float64
	SquelchOpen bool
}

// ChannelInfo describes one streamable channel.
//
// The DSP types are deliberately absent: the server needs a hub to read from
// and a function to ask for state, nothing more. That keeps this package
// testable without building a receive chain.
type ChannelInfo struct {
	Name string
	// Group is the tuner position that covers this channel. Only channels in
	// the active group carry audio; the rest stay listed but silent.
	Group     string
	Freq      float64
	AudioRate int
	Hub       *stream.Hub
	State     func() ChannelState
}

// GroupInfo describes one tuner position.
type GroupInfo struct {
	Name       string
	CenterFreq float64
	SampleRate float64
	Channels   []string
	Active     bool
}

// Options configures the server.
type Options struct {
	Channels          []ChannelInfo
	SourceDescription string

	// Groups reports the configured tuner positions and which is live. It is
	// called per request because the active group changes at runtime.
	Groups func() []GroupInfo
	// Switch moves the radio to another group. A nil Switch means the receiver
	// cannot retune, which is the case when replaying a capture.
	Switch func(ctx context.Context, group string) error

	// Embed controls which sites, if any, may frame a channel player.
	Embed EmbedOptions

	// MaxListeners caps concurrent audio connections across all channels.
	// Zero means unlimited. Each listener costs buffers and a goroutine, so an
	// unbounded count is a memory-growth path open to anyone who can connect.
	MaxListeners int
}

// Server routes audio and status over HTTP.
type Server struct {
	opts    Options
	byName  map[string]ChannelInfo
	mux     *http.ServeMux
	started time.Time
}

func NewServer(opts Options) (*Server, error) {
	if len(opts.Channels) == 0 {
		return nil, errors.New("the server needs at least one channel")
	}

	s := &Server{
		opts:    opts,
		byName:  make(map[string]ChannelInfo, len(opts.Channels)),
		mux:     http.NewServeMux(),
		started: time.Now(),
	}
	for _, ch := range opts.Channels {
		s.byName[ch.Name] = ch
	}

	s.mux.Handle("GET /static/", http.FileServerFS(staticFiles))
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /api/channels", s.handleChannels)
	s.mux.HandleFunc("GET /api/groups", s.handleGroups)
	s.mux.HandleFunc("POST /api/groups/{name}/activate", s.handleActivate)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /ws/audio/{name}", s.handleWebSocket)
	s.mux.HandleFunc("GET /stream/{name}", s.handleWAV)
	s.mux.HandleFunc("GET /embed/{name}", s.handleEmbed)
	s.mux.HandleFunc("GET /oembed", s.handleOEmbed)
	return s, nil
}

// Handler returns the HTTP handler for the whole server.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	body, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body) // the client has gone if this fails; nothing to do
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	type channelJSON struct {
		Name      string  `json:"name"`
		Group     string  `json:"group"`
		Freq      float64 `json:"freq"`
		AudioRate int     `json:"audio_rate"`
		Active    bool    `json:"active"`
	}

	active := s.activeGroup()
	out := make([]channelJSON, 0, len(s.opts.Channels))
	for _, ch := range s.opts.Channels {
		out = append(out, channelJSON{
			Name: ch.Name, Group: ch.Group, Freq: ch.Freq, AudioRate: ch.AudioRate,
			// With no groups configured every channel is always live.
			Active: active == "" || ch.Group == active,
		})
	}
	writeJSON(w, out)
}

// activeGroup names the live group, or "" when the receiver has no groups.
func (s *Server) activeGroup() string {
	if s.opts.Groups == nil {
		return ""
	}
	for _, g := range s.opts.Groups() {
		if g.Active {
			return g.Name
		}
	}
	return ""
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	type groupJSON struct {
		Name       string   `json:"name"`
		CenterFreq float64  `json:"center_freq"`
		SampleRate float64  `json:"sample_rate"`
		Channels   []string `json:"channels"`
		Active     bool     `json:"active"`
	}

	out := []groupJSON{}
	if s.opts.Groups != nil {
		for _, g := range s.opts.Groups() {
			out = append(out, groupJSON{
				Name: g.Name, CenterFreq: g.CenterFreq, SampleRate: g.SampleRate,
				Channels: g.Channels, Active: g.Active,
			})
		}
	}
	writeJSON(w, out)
}

// handleActivate retunes the radio to another group.
//
// One tuner serves everyone, so this changes what every listener hears. That is
// inherent to a single-radio receiver rather than something the API can hide.
func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if s.opts.Switch == nil || s.opts.Groups == nil {
		http.Error(w, "this receiver cannot switch groups", http.StatusNotImplemented)
		return
	}
	if !s.hasGroup(name) {
		http.Error(w, "no such group", http.StatusNotFound)
		return
	}

	if err := s.opts.Switch(r.Context(), name); err != nil {
		slog.Warn("group switch refused", "group", name, "err", err)
		// The radio declined: the receiver is still running on its previous
		// group, so this is a conflict rather than a server fault.
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	slog.Info("group switched by request", "group", name, "remote", r.RemoteAddr)
	s.handleStatus(w, r)
}

func (s *Server) hasGroup(name string) bool {
	for _, g := range s.opts.Groups() {
		if g.Name == name {
			return true
		}
	}
	return false
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	type channelJSON struct {
		Name        string  `json:"name"`
		Freq        float64 `json:"freq"`
		LevelDB     float64 `json:"level_db"`
		SquelchOpen bool    `json:"squelch_open"`
		Listeners   int     `json:"listeners"`
		Dropped     uint64  `json:"dropped"`
	}

	channels := make([]channelJSON, 0, len(s.opts.Channels))
	for _, ch := range s.opts.Channels {
		state := ChannelState{}
		if ch.State != nil {
			state = ch.State()
		}
		channels = append(channels, channelJSON{
			Name: ch.Name, Freq: ch.Freq,
			LevelDB: state.LevelDB, SquelchOpen: state.SquelchOpen,
			Listeners: ch.Hub.Listeners(), Dropped: ch.Hub.Dropped(),
		})
	}

	writeJSON(w, struct {
		Source      string        `json:"source"`
		ActiveGroup string        `json:"active_group"`
		UptimeS     float64       `json:"uptime_s"`
		Channels    []channelJSON `json:"channels"`
	}{
		Source:      s.opts.SourceDescription,
		ActiveGroup: s.activeGroup(),
		UptimeS:     time.Since(s.started).Seconds(),
		Channels:    channels,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("write json response", "err", err)
	}
}

// atCapacity reports whether the receiver already has as many listeners as it
// is willing to serve.
//
// The count is a sum across hubs rather than a reserved slot, so simultaneous
// requests can overshoot slightly. That is deliberate: the cap exists to bound
// growth, not to be exact, and taking a lock on the audio path to make it exact
// would cost more than the overshoot.
func (s *Server) atCapacity() bool {
	if s.opts.MaxListeners <= 0 {
		return false
	}
	total := 0
	for _, ch := range s.opts.Channels {
		total += ch.Hub.Listeners()
	}
	return total >= s.opts.MaxListeners
}

func (s *Server) refuseIfFull(w http.ResponseWriter, r *http.Request) bool {
	if !s.atCapacity() {
		return false
	}
	slog.Warn("refusing listener: at capacity",
		"max", s.opts.MaxListeners, "remote", r.RemoteAddr)
	http.Error(w, "the receiver is at its listener limit", http.StatusServiceUnavailable)
	return true
}

// handleWebSocket streams mu-law frames to a browser.
//
// This is the low-latency path: raw companded audio with no container, decoded
// by a few lines of JavaScript into an AudioWorklet. Nothing is buffered beyond
// the hub's few frames, so a listener hears the channel about as soon as the
// DSP produces it.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.byName[r.PathValue("name")]
	if !ok {
		http.Error(w, "no such channel", http.StatusNotFound)
		return
	}
	if s.refuseIfFull(w, r) {
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin is the default; the receiver is meant to be reached
		// directly or through a reverse proxy, not embedded in other sites.
		CompressionMode: websocket.CompressionDisabled, // audio is already compact
	})
	if err != nil {
		slog.Debug("websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow() //nolint:errcheck // best effort on the way out

	sub := ch.Hub.Subscribe()
	defer sub.Close()

	slog.Info("listener connected", "channel", ch.Name, "remote", r.RemoteAddr)
	defer slog.Info("listener disconnected", "channel", ch.Name, "remote", r.RemoteAddr)

	ctx := conn.CloseRead(r.Context()) // notices the client going away

	// Nothing is sent while the squelch is shut. An airband channel is silent
	// well over 90% of the time, so this alone cuts average bandwidth by around
	// ten times -- the same saving a codec's discontinuous transmission gives,
	// obtained from information the receiver already has. The client hears a
	// gap, which is exactly what silence should sound like.
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-keepalive.C:
			// A connection that sends nothing for minutes looks dead to
			// intermediaries, so ping through the quiet stretches.
			if err := pingWithin(ctx, conn); err != nil {
				return
			}

		case frame, ok := <-sub.Frames():
			if !ok {
				return
			}
			if ch.State != nil && !ch.State().SquelchOpen {
				continue // nothing on air: send nothing
			}
			if err := writeFrame(ctx, conn, frame); err != nil {
				return
			}
		}
	}
}

func pingWithin(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Ping(ctx)
}

func writeFrame(ctx context.Context, conn *websocket.Conn, frame []byte) error {
	// A listener that cannot absorb a frame within this window is beyond what
	// dropping can help, so the connection is dropped instead.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageBinary, frame)
}

// handleWAV streams the channel as an open-ended WAV, so anything that can play
// a URL — VLC, a phone's lock screen, a bare <audio> tag — can listen without
// any JavaScript at all.
func (s *Server) handleWAV(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("name"), ".wav")
	ch, ok := s.byName[name]
	if !ok {
		http.Error(w, "no such channel", http.StatusNotFound)
		return
	}
	if s.refuseIfFull(w, r) {
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "no-store")

	wav, err := stream.NewWAVWriter(w, ch.AudioRate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flush(w)

	sub := ch.Hub.Subscribe()
	defer sub.Close()

	slog.Info("wav listener connected", "channel", ch.Name, "remote", r.RemoteAddr)
	defer slog.Info("wav listener disconnected", "channel", ch.Name, "remote", r.RemoteAddr)

	// The hub carries mu-law, so expand it back to linear for the container.
	// The reused scratch slice keeps this off the allocator on a path that runs
	// for as long as someone is listening.
	pcm := make([]float32, 0, 4096)
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-sub.Frames():
			if !ok {
				return
			}
			pcm = pcm[:0]
			for _, b := range frame {
				pcm = append(pcm, float32(stream.DecodeULaw(b))/32768)
			}
			if err := wav.Write(pcm); err != nil {
				return
			}
			flush(w)
		}
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ListenAndServe runs the server until the context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: these responses are audio streams that are meant
		// to stay open indefinitely.
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown) // already stopping; nothing useful to report
	}()

	slog.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
