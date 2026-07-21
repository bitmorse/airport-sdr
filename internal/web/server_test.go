package web

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitmorse/airport-sdr/internal/stream"
	"github.com/coder/websocket"
)

func testServer(t *testing.T) (*Server, *stream.Hub) {
	t.Helper()
	hub := stream.NewHub(8, 160)

	srv, err := NewServer(Options{
		Channels: []ChannelInfo{{
			Name: "Tower", Freq: 118_100_000, AudioRate: 8000, Hub: hub,
			State: func() ChannelState { return ChannelState{LevelDB: -42, SquelchOpen: true} },
		}},
		SourceDescription: "test source",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, hub
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestServerRequiresAtLeastOneChannel(t *testing.T) {
	if _, err := NewServer(Options{}); err == nil {
		t.Fatal("a server with no channels should be an error")
	}
}

func TestIndexIsServed(t *testing.T) {
	srv, _ := testServer(t)
	rec := get(t, srv, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "<html") && !strings.Contains(rec.Body.String(), "<!doctype") {
		t.Error("body does not look like an HTML document")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	srv, _ := testServer(t)
	if rec := get(t, srv, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

func TestChannelsAPI(t *testing.T) {
	srv, _ := testServer(t)
	rec := get(t, srv, "/api/channels")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/channels = %d, want 200", rec.Code)
	}

	var got []struct {
		Name      string  `json:"name"`
		Freq      float64 `json:"freq"`
		AudioRate int     `json:"audio_rate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("got %d channels, want 1", len(got))
	}
	if got[0].Name != "Tower" || got[0].Freq != 118_100_000 || got[0].AudioRate != 8000 {
		t.Errorf("channel = %+v, want Tower at 118.1 MHz / 8000 Hz", got[0])
	}
}

func TestStatusAPIReportsChannelState(t *testing.T) {
	srv, hub := testServer(t)
	sub := hub.Subscribe()
	defer sub.Close()

	rec := get(t, srv, "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}

	var got struct {
		Source   string `json:"source"`
		Channels []struct {
			Name        string  `json:"name"`
			LevelDB     float64 `json:"level_db"`
			SquelchOpen bool    `json:"squelch_open"`
			Listeners   int     `json:"listeners"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}

	if got.Source != "test source" {
		t.Errorf("source = %q, want %q", got.Source, "test source")
	}
	if len(got.Channels) != 1 {
		t.Fatalf("got %d channels, want 1", len(got.Channels))
	}
	if c := got.Channels[0]; c.LevelDB != -42 || !c.SquelchOpen || c.Listeners != 1 {
		t.Errorf("status = %+v, want level -42, squelch open, 1 listener", c)
	}
}

// --- WAV streaming ----------------------------------------------------------

func TestWAVStreamServesAValidHeader(t *testing.T) {
	srv, hub := testServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		ts.URL+"/stream/Tower.wav", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", ct)
	}

	// Publish something so the handler has a reason to flush the header.
	go func() {
		for i := 0; i < 20; i++ {
			hub.Publish(make([]byte, 160))
			time.Sleep(5 * time.Millisecond)
		}
	}()

	header := make([]byte, 44)
	if _, err := io.ReadFull(resp.Body, header); err != nil {
		t.Fatalf("read header: %v", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		t.Errorf("not a WAV header: % x", header[:12])
	}
	if rate := binary.LittleEndian.Uint32(header[24:]); rate != 8000 {
		t.Errorf("sample rate = %d, want 8000", rate)
	}
}

func TestStreamUnknownChannelIs404(t *testing.T) {
	srv, _ := testServer(t)
	if rec := get(t, srv, "/stream/Nowhere.wav"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown channel = %d, want 404", rec.Code)
	}
}

// --- websocket audio --------------------------------------------------------

func TestWebSocketDeliversAudioFrames(t *testing.T) {
	srv, hub := testServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/audio/Tower"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	go func() {
		for i := 0; i < 100; i++ {
			frame := make([]byte, 160)
			for j := range frame {
				frame[j] = 0xFF // mu-law silence
			}
			hub.Publish(frame)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Errorf("message type = %v, want binary", typ)
	}
	if len(data) != 160 {
		t.Errorf("frame is %d bytes, want 160", len(data))
	}
}

// Airband is silent well over 90% of the time. Sending encoded silence for all
// of it is pure waste, and the squelch already knows when there is nothing to
// send, so the websocket goes quiet between transmissions. This is the same
// saving a codec's discontinuous transmission would buy, without the codec.
func TestWebSocketSendsNothingWhileSquelchIsClosed(t *testing.T) {
	hub := stream.NewHub(8, 160)
	var open atomic.Bool

	srv, err := NewServer(Options{
		Channels: []ChannelInfo{{
			Name: "Tower", Freq: 118_100_000, AudioRate: 8000, Hub: hub,
			State: func() ChannelState { return ChannelState{SquelchOpen: open.Load()} },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/audio/Tower"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForListener(t, hub)
	stopPublishing := publishRepeatedly(hub)
	defer close(stopPublishing)

	// One long-lived reader: cancelling a Read would tear down the connection,
	// so silence is asserted by watching a channel rather than by timing out a
	// read and then trying to reuse the socket.
	frames := make(chan int, 4)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			frames <- len(data)
		}
	}()

	select {
	case n := <-frames:
		t.Fatalf("received %d bytes of audio while the squelch was closed", n)
	case <-time.After(400 * time.Millisecond):
	}

	open.Store(true)
	select {
	case <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("no audio arrived after the squelch opened")
	}
}

// The WAV endpoint carries its own timeline, so it must keep emitting samples
// even while squelched. A gap there would stall the player rather than simply
// being silent.
func TestWAVKeepsFlowingWhileSquelchIsClosed(t *testing.T) {
	hub := stream.NewHub(8, 160)
	srv, err := NewServer(Options{
		Channels: []ChannelInfo{{
			Name: "Tower", Freq: 118_100_000, AudioRate: 8000, Hub: hub,
			State: func() ChannelState { return ChannelState{SquelchOpen: false} },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/stream/Tower.wav", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	stopPublishing := publishRepeatedly(hub)
	defer close(stopPublishing)

	// Header plus at least one block of audio must arrive despite the squelch.
	buf := make([]byte, 44+160)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("wav stalled while squelched: %v", err)
	}
}

func waitForListener(t *testing.T, hub *stream.Hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hub.Listeners() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.Listeners() == 0 {
		t.Fatal("listener never attached")
	}
}

func publishRepeatedly(hub *stream.Hub) chan struct{} {
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				hub.Publish(make([]byte, 160))
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	return stop
}

func TestWebSocketUnknownChannelIsRejected(t *testing.T) {
	srv, _ := testServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/audio/Nowhere"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected a websocket for an unknown channel to be refused")
	}
}

// A listener disconnecting must be noticed, or the hub would accumulate dead
// subscribers for the lifetime of the process.
func TestWebSocketDisconnectReleasesSubscriber(t *testing.T) {
	srv, hub := testServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/audio/Tower"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for hub.Listeners() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Listeners() != 1 {
		t.Fatalf("listeners = %d after connect, want 1", hub.Listeners())
	}

	conn.Close(websocket.StatusNormalClosure, "")

	deadline = time.Now().Add(5 * time.Second)
	for hub.Listeners() > 0 && time.Now().Before(deadline) {
		hub.Publish(make([]byte, 160))
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Listeners() != 0 {
		t.Errorf("listeners = %d after disconnect, want 0", hub.Listeners())
	}
}
