package lullaby

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/internal/sources"
	"github.com/windrunner21/lullaby/songs"
)

// radioAnswer is YouTube Music's answer for a radio of these videos, the
// first being the song itself, with only what's read.
func radioAnswer(ids ...string) string {
	var items []string
	for i, id := range ids {
		items = append(items, fmt.Sprintf(`{"playlistPanelVideoRenderer": {"videoId": %q, "title": {"runs": [{"text": "Song %d"}]},
			"shortBylineText": {"runs": [{"text": "Artist %d"}]}, "lengthText": {"runs": [{"text": "3:2%d"}]},
			"thumbnail": {"thumbnails": [{"url": "https://yt3.googleusercontent.com/c%d=w544-h544-l90-rj", "width": 544}]}}}`, id, i, i, i, i))
	}
	return `{"contents": {"singleColumnMusicWatchNextResultsRenderer": {"tabbedRenderer": {"watchNextTabbedResultsRenderer": {"tabs": [
		{"tabRenderer": {"content": {"musicQueueRenderer": {"content": {"playlistPanelRenderer": {"contents": [` + strings.Join(items, ",") + `]}}}}}}]}}}}}`
}

// radioEngine is an engine whose YouTube Music answers radios with answer,
// once release is closed, and counts the radios it's asked for.
func radioEngine(answer func() (int, string), release <-chan struct{}) (*Engine, *atomic.Int32) {
	var asked atomic.Int32
	client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/next") {
			return answer200(`{}`), nil // the visitor ID
		}
		asked.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		status, body := answer()
		resp := answer200(body)
		resp.StatusCode = status
		return resp, nil
	})}
	e := &Engine{ctx: context.Background(), yt: sources.NewYouTube(client, "https://api.invalid", "https://music.invalid"),
		suggested: make(map[string]suggestion)}
	return e, &asked
}

func TestRadio(t *testing.T) {
	release := make(chan struct{})
	e, asked := radioEngine(func() (int, string) {
		return http.StatusOK, radioAnswer("ZqSlV5LmrTg", "AdEKgwUqPKI", "yEA3qaB0dH8", "UPeS_V1pVZk")
	}, release)
	snooze := songs.Song{ID: "ZqSlV5LmrTg", Link: songs.WatchLink("ZqSlV5LmrTg"), Title: "Snooze"}

	// The app asks for a song's radio, then twice more while it's on its
	// way, and the first one gives up: the request goes on for the others.
	impatient, cancel := context.WithCancel(context.Background())
	type answer struct {
		found []songs.Song
		err   error
	}
	results := make(chan answer, 3)
	ask := func(ctx context.Context) {
		found, err := e.Radio(ctx, snooze)
		results <- answer{found, err}
	}
	go ask(impatient)
	for asked.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	go ask(context.Background())
	go ask(context.Background())
	time.Sleep(20 * time.Millisecond)
	cancel()
	if a := <-results; !errors.Is(a.err, context.Canceled) {
		t.Errorf("the one who gave up got %v", a.err)
	}
	close(release)
	var found []songs.Song
	for range 2 {
		a := <-results
		if a.err != nil {
			t.Fatal(a.err)
		}
		found = a.found
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked for the radio %d times, want once for everyone", n)
	}
	if len(found) != 3 || found[0].ID != "AdEKgwUqPKI" {
		t.Fatalf("radio = %+v, want the three songs after Snooze", found)
	}

	// Picked, a song plays with YouTube Music's name.
	if s, ok := e.recall(found[0].Link); !ok || s.Title != "Song 1" {
		t.Errorf("recall = %+v, %v; want the radio's song remembered like a suggestion", s, ok)
	}
	// Asked again, by its link alone, it's remembered, and what the app did
	// to its copy doesn't change it.
	found[0].Title = "changed"
	again, err := e.Radio(context.Background(), songs.Song{Link: snooze.Link})
	if err != nil || len(again) != 3 || again[0].Title != "Song 1" {
		t.Errorf("Radio again = %+v, %v; want the same three songs", again, err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("asked again for a radio found a moment ago: %d requests", n)
	}

	// An old one is asked for again.
	e.mu.Lock()
	e.radios[snooze.ID] = search{found: e.radios[snooze.ID].found, at: time.Now().Add(-searchedFor - time.Minute)}
	e.mu.Unlock()
	if _, err := e.Radio(context.Background(), snooze); err != nil {
		t.Fatal(err)
	}
	if n := asked.Load(); n != 2 {
		t.Errorf("a radio past SearchMemory wasn't asked for again: %d requests", n)
	}
}

func TestRadioNeedsAYouTubeVideo(t *testing.T) {
	release := make(chan struct{})
	close(release)
	e, asked := radioEngine(func() (int, string) { return http.StatusOK, radioAnswer() }, release)
	soundcloud := songs.Song{Link: "https://soundcloud.com/someone/a-song", Title: "A song"}
	if _, err := e.Radio(context.Background(), soundcloud); !errors.Is(err, ErrNoRadio) {
		t.Errorf("a SoundCloud song's radio: %v, want ErrNoRadio", err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("asked YouTube Music %d times for a song it has no radio for", n)
	}
}

func TestRadioFailureIsntRemembered(t *testing.T) {
	release := make(chan struct{})
	close(release)
	var status atomic.Int32
	status.Store(http.StatusServiceUnavailable)
	e, asked := radioEngine(func() (int, string) { return int(status.Load()), radioAnswer("ZqSlV5LmrTg", "AdEKgwUqPKI") }, release)
	snooze := songs.Song{ID: "ZqSlV5LmrTg"}
	if _, err := e.Radio(context.Background(), snooze); err == nil {
		t.Fatal("a refused radio gave no error")
	}
	status.Store(http.StatusOK)
	if found, err := e.Radio(context.Background(), snooze); err != nil || len(found) != 1 {
		t.Errorf("after a failure, Radio = %v, %v; want it asked for again", found, err)
	}
	if n := asked.Load(); n != 2 {
		t.Errorf("asked %d times, want twice", n)
	}
}

func TestRadiosAreBounded(t *testing.T) {
	e := &Engine{radios: make(map[string]search)}
	start := time.Now()
	for i := range maxRadios + 10 {
		e.keepRadioLocked(fmt.Sprint("video", i), []songs.Song{{Title: "Snooze"}})
		r := e.radios[fmt.Sprint("video", i)]
		r.at = start.Add(time.Duration(i) * time.Second)
		e.radios[fmt.Sprint("video", i)] = r
	}
	if len(e.radios) != maxRadios {
		t.Errorf("remembering %d radios, want %d", len(e.radios), maxRadios)
	}
	if _, ok := e.radios["video0"]; ok {
		t.Error("the oldest radio is still remembered")
	}
	if _, ok := e.radios[fmt.Sprint("video", maxRadios+9)]; !ok {
		t.Error("the newest radio was forgotten")
	}
}
