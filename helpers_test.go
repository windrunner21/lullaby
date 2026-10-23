package lullaby

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby/internal/audio"
)

// quietConn is a voice connection that connects nowhere. disgo's own manager
// keeps it, so its habits, like handing back the server's connection, are
// the real ones.
type quietConn struct {
	voice.Conn
	remove func()
	mu     sync.Mutex
	closed bool
}

func (c *quietConn) Open(context.Context, snowflake.ID, bool, bool) error { return nil }
func (c *quietConn) SetOpusFrameProvider(voice.OpusFrameProvider)         {}

func (c *quietConn) Close(context.Context) {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.remove()
}

func (c *quietConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// quietManager is disgo's voice manager with connections that connect nowhere.
func quietManager() voice.Manager {
	return voice.NewManager(nil, 2, voice.WithConnCreateFunc(func(_, _ snowflake.ID, _ voice.StateUpdateFunc, remove func(), _ ...voice.ConnConfigOpt) voice.Conn {
		return &quietConn{remove: remove}
	}))
}

// app shows nothing of what the player tells it, but calls started and
// ended, if set, with what happened.
type app struct {
	started func(*Track)
	ended   func(*Track, End)
}

func (a app) Started(_ snowflake.ID, t *Track) {
	if a.started != nil {
		a.started(t)
	}
}

func (a app) Ended(_ snowflake.ID, t *Track, end End) {
	if a.ended != nil {
		a.ended(t, end)
	}
}

func (app) Left(snowflake.ID, *Track) {}
func (app) QueueChanged(snowflake.ID) {}

// fakeTransport answers every request with fn.
type fakeTransport func(*http.Request) (*http.Response, error)

func (fn fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func answer200(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

// endedStream is a song whose ffmpeg has stopped already, like one whose
// link broke.
func endedStream(t *testing.T) *audio.Stream {
	t.Helper()
	path, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true command")
	}
	s, err := audio.Start(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("it never stopped")
	}
	return s
}

// silentEncoder is an "ffmpeg" that writes 100 frames of Opus silence at
// once, for tests of what plays when, not of the sound.
func silentEncoder(t *testing.T) string {
	t.Helper()
	var data []byte
	page := func(packet []byte) {
		h := make([]byte, 28)
		copy(h, "OggS")
		h[26], h[27] = 1, byte(len(packet))
		data = append(data, h...)
		data = append(data, packet...)
	}
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 2
	binary.LittleEndian.PutUint32(head[12:], 48000)
	page(head)
	tags := make([]byte, 16)
	copy(tags, "OpusTags")
	page(tags)
	for range 100 {
		page([]byte{0xf8, 0xff, 0xfe})
	}
	path := filepath.Join(t.TempDir(), "encoder")
	if err := os.WriteFile(path+".ogg", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec cat \"$0.ogg\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// bufferedStream is a song from encoder whose frames are all in already.
func bufferedStream(t *testing.T, encoder string) *audio.Stream {
	t.Helper()
	s, err := audio.Start(context.Background(), encoder)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("stand-in encoder stuck")
	}
	return s
}
