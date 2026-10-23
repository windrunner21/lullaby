package lullaby

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/internal/audio"
	"github.com/windrunner21/lullaby/internal/netguard"
	"github.com/windrunner21/lullaby/songs"
)

// maxPieceForTests is internal/audio's limit on one piece of a playlist.
const maxPieceForTests = 8 << 20

// offlineEngine is an engine made with New whose sites answer through
// client, offline, with encoder as its ffmpeg and dl as its yt-dlp, if any.
func offlineEngine(t *testing.T, client *http.Client, encoder, dl string) *Engine {
	t.Helper()
	// AllowPrivateLinks: the stand-in sites answer without a DNS lookup first.
	e, err := New(Config{Voice: quietManager(), DAVE: NewDAVE(), HTTP: client, AllowPrivateLinks: true,
		FFmpeg: encoder, YTDLP: dl, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(1) })
	return e
}

func TestPlayStopsWaitingForAPlaylist(t *testing.T) {
	// The song comes as a playlist, slow to arrive, whether it plays at once
	// or goes in the queue to be got ready: whoever asked stops waiting as
	// soon as they give up.
	for _, queued := range []bool{false, true} {
		name := "plays at once"
		if queued {
			name = "goes in the queue"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/song.m3u8" {
					once.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return answer200("#EXTM3U\n#EXTINF:1,\npart\n#EXT-X-ENDLIST\n"), nil
				}
				return answer200("input"), nil
			})}
			dl := filepath.Join(t.TempDir(), "yt-dlp")
			if err := os.WriteFile(dl, []byte("#!/bin/sh\nprintf '%s' '{\"title\":\"test\",\"webpage_url\":\"https://media.invalid/song\",\"url\":\"https://media.invalid/song.m3u8\",\"protocol\":\"m3u8_native\"}'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			e := offlineEngine(t, client, silentEncoder(t), dl)
			if queued {
				p, err := e.join(context.Background(), 1, 100, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				e.mu.Lock()
				p.playing = &Track{e: e, stream: endedStream(t)}
				e.mu.Unlock()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := e.Play(ctx, Request{GuildID: 1, ChannelID: 100, Query: "https://media.invalid/song"})
				done <- err
			}()
			if queued {
				// Queued, the answer doesn't wait for the song to be got ready.
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Play waited for the next song to be got ready")
				}
				return
			}
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("Play failed before the playlist was asked for: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("the playlist was never asked for")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Play = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Play kept waiting for the playlist after its caller gave up")
			}
		})
	}
}

func TestStopsWhenItsLifetimeEnds(t *testing.T) {
	client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/youtubei/v1/player" {
			return answer200(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"title":"test"},"streamingData":{"expiresInSeconds":"21600","adaptiveFormats":[{"mimeType":"audio/mp4","bitrate":128000,"url":"https://audio.invalid/a"}]}}`), nil
		}
		return answer200(`{}`), nil
	})}
	e := offlineEngine(t, client, silentEncoder(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx); err == nil {
		t.Error("Start was taken a second time")
	}
	track, _, err := e.Play(context.Background(), Request{GuildID: 1, ChannelID: 100, Query: songs.WatchLink("aaaaaaaaaaa")})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-track.stream.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the stand-in encoder never finished")
	}

	cancel()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		frame, ferr := track.stream.ProvideOpusFrame()
		_, inVoice := e.Channel(1)
		if frame == nil && ferr == io.EOF && !inVoice {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the engine's lifetime: a frame %v (%v), still in voice %v", frame != nil, ferr, inVoice)
		}
	}
	if _, _, err := e.Play(context.Background(), Request{GuildID: 1, ChannelID: 100, Query: songs.WatchLink("aaaaaaaaaaa")}); !errors.Is(err, ErrStopped) {
		t.Errorf("Play after the engine's lifetime = %v, want ErrStopped", err)
	}
}

func TestPlayWithAContextDoneAlready(t *testing.T) {
	e := offlineEngine(t, &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("asked nothing")
	})}, silentEncoder(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e.Play(ctx, Request{GuildID: 1, ChannelID: 100, Query: songs.WatchLink("aaaaaaaaaaa")}); !errors.Is(err, context.Canceled) {
		t.Errorf("Play = %v, want context.Canceled", err)
	}
	if _, inVoice := e.Channel(1); inVoice {
		t.Error("joined voice for a Play given up on already")
	}
}

func TestNewNeedsFFmpeg(t *testing.T) {
	_, err := New(Config{Voice: quietManager(), DAVE: NewDAVE(), FFmpeg: filepath.Join(t.TempDir(), "no-ffmpeg")})
	if err == nil {
		t.Error("made an engine without ffmpeg")
	}
}

func TestNewClient(t *testing.T) {
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()

	// The app's TLS settings are kept, and only the session cache is added.
	transport := old.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "custom.invalid"}
	http.DefaultTransport = transport
	got := NewClient().Transport.(*http.Transport).TLSClientConfig
	if got.MinVersion != tls.VersionTLS13 || got.ServerName != "custom.invalid" || got.ClientSessionCache == nil {
		t.Errorf("TLS settings %+v, want the app's, with a session cache", got)
	}
	if transport.TLSClientConfig.ClientSessionCache != nil {
		t.Error("NewClient changed the app's own TLS settings")
	}

	// A transport of the app's own, say for tracing, is used as it is.
	wrapped := fakeTransport(func(*http.Request) (*http.Response, error) { return answer200(""), nil })
	http.DefaultTransport = wrapped
	if c := NewClient(); c.Transport == nil {
		t.Error("no transport")
	}
}

func TestRefusedPlaylistIsntLeftToFFmpeg(t *testing.T) {
	// A public playlist sends the guarded client somewhere it refuses: that
	// isn't asked again, and isn't left to ffmpeg, which would follow it.
	var asked atomic.Int32
	e := &Engine{ffmpeg: silentEncoder(t), guardLinks: true,
		http: &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
			asked.Add(1)
			return nil, netguard.ErrPrivate
		})}}
	s, err := audio.Start(context.Background(), e.ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fed, err := e.feed(context.Background(), s, songs.Song{Audio: songs.Audio{URL: "https://8.8.8.8/list.m3u8", Playlist: true}})
	if fed != nil {
		fed.Close()
	}
	if !errors.Is(err, songs.ErrUnplayable) || !errors.Is(err, netguard.ErrPrivate) {
		t.Errorf("feed = stream %v, %v; want ErrUnplayable", fed != nil, err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked %d times, want a refusal taken at once", n)
	}
}

func TestTooBigPieceIsUnplayable(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg isn't installed")
	}
	ended := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := &Engine{ctx: ctx, ffmpeg: ffmpeg, events: app{ended: func(_ *Track, end End) { ended <- end.Err }},
		http: &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
			body := "#EXTM3U\n#EXTINF:10,\npart.ts\n#EXT-X-ENDLIST\n"
			size := int64(len(body))
			if r.URL.Path == "/part.ts" {
				body, size = "", maxPieceForTests+1
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: size,
				Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
	s, err := e.openSong(ctx, songs.Song{Audio: songs.Audio{URL: "https://audio.invalid/list.m3u8", Playlist: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case <-s.Done():
	case <-ctx.Done():
		t.Fatal("ffmpeg never finished")
	}
	e.afterMove(1, &Track{e: e, stream: s}, nil, s, false)
	if err := <-ended; !errors.Is(err, songs.ErrUnplayable) {
		t.Errorf("End.Err = %v, want ErrUnplayable for a piece too big", err)
	}
}

// slowStop is a lifetime whose first AfterFunc, Start's stopAll, waits for
// gate, while contexts made from it end at once: the moment between a song's
// ffmpeg being stopped and stopAll taking its player.
type slowStop struct {
	context.Context
	first atomic.Bool
	gate  chan struct{}
}

func (*slowStop) Value(any) any { return nil }

func (c *slowStop) AfterFunc(fn func()) func() bool {
	if c.first.CompareAndSwap(false, true) {
		return context.AfterFunc(c.Context, func() {
			<-c.gate
			fn()
		})
	}
	return context.AfterFunc(c.Context, fn)
}

func TestStoppingIsntASongEnding(t *testing.T) {
	encoder := silentEncoder(t)
	if err := os.WriteFile(encoder, []byte("#!/bin/sh\ncat \"$0.ogg\"\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := offlineEngine(t, &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}, encoder, "")
	told := make(chan End, 1)
	e.events = app{ended: func(_ *Track, end End) { told <- end }}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &slowStop{Context: base, gate: make(chan struct{})}
	defer close(ctx.gate)
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := e.join(ctx, 1, 100, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := audio.Start(ctx, encoder)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := e.begin(1, p, &Track{e: e, stream: s}, false, nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); s.Played() < 2*time.Second; time.Sleep(time.Millisecond) {
		p.frames.ProvideOpusFrame()
		if time.Now().After(deadline) {
			t.Fatal("no sound")
		}
	}
	cancel() // stops ffmpeg, with the song still going
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ffmpeg went on after the engine's lifetime")
	}
	p.frames.ProvideOpusFrame()
	select {
	case end := <-told:
		t.Errorf("the engine stopping was told as a song ending: %+v", end)
	case <-time.After(200 * time.Millisecond):
	}
}
