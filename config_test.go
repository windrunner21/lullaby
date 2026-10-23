package lullaby

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby/internal/sources"
	"github.com/windrunner21/lullaby/songs"
)

func TestNewNeedsVoiceAndDAVE(t *testing.T) {
	ffmpeg := silentEncoder(t) // a stand-in, found as New looks for ffmpeg
	if _, err := New(Config{DAVE: NewDAVE(), FFmpeg: ffmpeg}); err == nil {
		t.Error("made an engine without a voice manager")
	}
	if _, err := New(Config{Voice: quietManager(), FFmpeg: ffmpeg}); err == nil {
		t.Error("made an engine without DAVE")
	}
	e, err := New(Config{Voice: quietManager(), DAVE: NewDAVE(), FFmpeg: ffmpeg})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(e.ffmpeg) || e.events == nil || e.http == nil || e.log == nil {
		t.Errorf("defaults missing: ffmpeg %q, events %v, http %v, logger %v", e.ffmpeg, e.events, e.http, e.log)
	}
}

// lockedBuffer is a log anyone may write to at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLogsToTheLogger(t *testing.T) {
	var log lockedBuffer
	offline := &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	// AllowPrivateLinks: the stand-in sites answer offline, without a DNS lookup first.
	e, err := New(Config{Voice: quietManager(), DAVE: NewDAVE(), HTTP: offline, AllowPrivateLinks: true,
		FFmpeg: silentEncoder(t), Logger: slog.New(slog.NewTextHandler(&log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	e.Start(context.Background()) // its warm-ups fail, and say so
	for deadline := time.Now().Add(2 * time.Second); !strings.Contains(log.String(), "warm-up failed"); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("nothing logged to the engine's logger: %q", log.String())
		}
	}
}

func TestPlayStopsWaitingWithItsContext(t *testing.T) {
	// The song takes its time to be found while one plays. Whoever asked gives
	// up; the lookup goes on, and the next to ask for it gets it at once.
	var lookups atomic.Int32
	release := make(chan struct{})
	client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/player") {
			return answer200(`{}`), nil
		}
		lookups.Add(1)
		<-release
		return answer200(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"title":"Snooze","lengthSeconds":"202"},` +
			`"streamingData":{"expiresInSeconds":"21600","adaptiveFormats":[{"mimeType":"audio/mp4","bitrate":128000,"url":"https://audio.invalid/a"}]}}`), nil
	})}
	yt := sources.NewYouTube(client, "https://player.invalid", "https://music.invalid")
	e := &Engine{ctx: context.Background(), players: map[snowflake.ID]*player{1: {playing: &Track{}}},
		finder: sources.NewFinder(yt, nil, nil, nil), events: app{}}
	link := songs.WatchLink("aaaaaaaaaaa")

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := e.Play(ctx, Request{GuildID: 1, ChannelID: 100, Query: link})
		result <- err
	}()
	for lookups.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Play = %v, want it to stop waiting with its context", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Play kept waiting after its context was canceled")
	}

	close(release)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if song, err := e.find(context.Background(), link); err == nil && song.Title == "Snooze" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lookup given up on never finished")
		}
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("asked YouTube %d times, want the lookup given up on to be the one kept", n)
	}
}
