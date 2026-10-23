package sources

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// A song as SoundCloud's API described one on 2026-10-23, with made-up names
// and links. SERVER stands for the test server.
const soundcloudTrack = `{
	"kind": "track", "id": 526764561, "title": "A Song", "duration": 128125, "full_duration": 128168,
	"permalink_url": "https://soundcloud.com/someone/a-song", "policy": "ALLOW", "streamable": true,
	"artwork_url": "https://i1.sndcdn.com/artworks-000436149384-fjebm6-large.jpg",
	"track_authorization": "auth-token", "monetization_model": "NOT_APPLICABLE",
	"user": {"username": "Someone", "avatar_url": "https://i1.sndcdn.com/avatars-000-large.jpg"},
	"publisher_metadata": null,
	"media": {"transcodings": [
		{"url": "SERVER/media/soundcloud:tracks:526764561/aac_96k/stream/hls", "preset": "aac_96k", "snipped": false, "quality": "lq",
			"format": {"protocol": "hls", "mime_type": "audio/mp4; codecs=\"mp4a.40.2\""}},
		{"url": "SERVER/media/soundcloud:tracks:526764561/abr_sq/stream/hls", "preset": "abr_sq", "snipped": false, "quality": "sq",
			"format": {"protocol": "hls", "mime_type": "audio/mpegurl"}}
	]}
}`

// fakeSoundCloud answers like SoundCloud's website and API.
type fakeSoundCloud struct {
	url      string
	track    string
	key      string // the key the API accepts
	pages    atomic.Int32
	resolved atomic.Int32
}

func newFakeSoundCloud(t *testing.T, track string) (*SoundCloud, *fakeSoundCloud) {
	f := &fakeSoundCloud{track: track, key: "abcdefghijklmnopqrstuvwxyz012345"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return NewSoundCloud(srv.Client(), srv.URL, srv.URL), f
}

func (f *fakeSoundCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		f.pages.Add(1)
		io.WriteString(w, `<html><script crossorigin src="`+f.url+`/assets/0-a.js"></script><script crossorigin src="`+f.url+`/assets/49-b.js"></script></html>`)
	case r.URL.Path == "/assets/49-b.js":
		io.WriteString(w, `(function(){var e={env:"production",client_id:"`+f.key+`",api:"api-v2"};})();`)
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		io.WriteString(w, `console.log("nothing here");`)
	case r.URL.Path == "/short":
		http.Redirect(w, r, "https://soundcloud.com/someone/a-song?utm_source=share", http.StatusFound)
	case r.URL.Query().Get("client_id") != f.key:
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case r.URL.Path == "/resolve":
		f.resolved.Add(1)
		if r.URL.Query().Get("url") == "https://soundcloud.com/someone/gone" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, strings.ReplaceAll(f.track, "SERVER", f.url))
	case strings.HasPrefix(r.URL.Path, "/media/"):
		if r.URL.Query().Get("track_authorization") != "auth-token" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		io.WriteString(w, `{"url": "https://playback.media-streaming.soundcloud.cloud/abc/aac_96k/playlist.m3u8?expires=1792700000"}`)
	default:
		http.NotFound(w, r)
	}
}

func TestSoundCloudLookup(t *testing.T) {
	sc, api := newFakeSoundCloud(t, soundcloudTrack)
	song, err := sc.Lookup(context.Background(), "https://soundcloud.com/someone/a-song")
	if err != nil {
		t.Fatal(err)
	}
	if song.Title != "A Song" || song.Artist != "Someone" || song.Length != 128125*time.Millisecond || song.Link != "https://soundcloud.com/someone/a-song" {
		t.Errorf("song = %+v", song)
	}
	if song.Thumbnail != "https://i1.sndcdn.com/artworks-000436149384-fjebm6-t500x500.jpg" {
		t.Errorf("Thumbnail = %q, want the 500 px artwork", song.Thumbnail)
	}
	if !song.Audio.Playlist || !strings.HasSuffix(strings.Split(song.Audio.URL, "?")[0], "/aac_96k/playlist.m3u8") {
		t.Errorf("Audio = %+v, want the AAC playlist", song.Audio)
	}
	if song.ID != "" {
		t.Errorf("a SoundCloud song has the YouTube ID %q", song.ID)
	}
	if !song.Audio.Expires.Equal(time.Unix(1792700000, 0)) {
		t.Errorf("Expires = %v, want the link's expires", song.Audio.Expires)
	}

	if _, err := sc.Lookup(context.Background(), "https://soundcloud.com/someone/a-song"); err != nil {
		t.Fatal(err)
	}
	if api.pages.Load() != 1 {
		t.Errorf("read the website %d times for 2 songs, want once", api.pages.Load())
	}
}

func TestSoundCloudNewKey(t *testing.T) {
	sc, api := newFakeSoundCloud(t, soundcloudTrack)
	if err := sc.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	// SoundCloud changes its key: the old one is refused once, then the new
	// one is found.
	api.key = "ZYXWVUTSRQPONMLKJIHGFEDCBA987654"
	if _, err := sc.Lookup(context.Background(), "https://soundcloud.com/someone/a-song"); err != nil {
		t.Fatalf("lookup after the key changed: %v", err)
	}
	if api.pages.Load() != 2 {
		t.Errorf("read the website %d times, want twice: once at first, once for the new key", api.pages.Load())
	}
}

func TestSoundCloudShortLink(t *testing.T) {
	_, api := newFakeSoundCloud(t, soundcloudTrack)
	// A client that takes every host, on.soundcloud.com included, to the
	// test server, so nothing leaves this machine.
	addr := strings.TrimPrefix(api.url, "http://")
	local := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	sc := NewSoundCloud(local, api.url, api.url)

	link, err := sc.unshorten(context.Background(), "http://on.soundcloud.com/short")
	if err != nil || link != "https://soundcloud.com/someone/a-song?utm_source=share" {
		t.Errorf("unshorten = %q, %v; want the song's own link", link, err)
	}
	link, err = sc.unshorten(context.Background(), "https://soundcloud.com/someone/a-song")
	if err != nil || link != "https://soundcloud.com/someone/a-song" {
		t.Errorf("a full link became %q (%v)", link, err)
	}
}

func TestSoundCloudErrors(t *testing.T) {
	previewOnly := strings.ReplaceAll(soundcloudTrack, `"snipped": false`, `"snipped": true`)
	playlist := strings.Replace(soundcloudTrack, `"kind": "track"`, `"kind": "playlist"`, 1)
	blocked := strings.Replace(soundcloudTrack, `"policy": "ALLOW"`, `"policy": "BLOCK"`, 1)
	tests := []struct {
		name  string
		track string
		link  string
		want  error
	}{
		{"only a preview", previewOnly, "https://soundcloud.com/someone/a-song", songs.ErrUnplayable},
		{"a playlist", playlist, "https://soundcloud.com/someone/sets/an-album", songs.ErrPlaylist},
		{"blocked here", blocked, "https://soundcloud.com/someone/a-song", songs.ErrUnplayable},
		{"gone", soundcloudTrack, "https://soundcloud.com/someone/gone", songs.ErrUnplayable},
	}
	for _, tt := range tests {
		sc, _ := newFakeSoundCloud(t, tt.track)
		if _, err := sc.Lookup(context.Background(), tt.link); !errors.Is(err, tt.want) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestBestSoundCloudAudio(t *testing.T) {
	tc := func(protocol, mime string, snipped bool) transcoding {
		var t transcoding
		t.URL, t.Snipped, t.Format.Protocol, t.Format.MimeType = protocol+mime, snipped, protocol, mime
		return t
	}
	tests := []struct {
		name string
		have []transcoding
		want string
	}{
		{"a whole file first", []transcoding{tc("hls", "audio/mp4; codecs=\"mp4a.40.2\"", false), tc("progressive", "audio/mpeg", false)}, "progressive"},
		{"AAC over the adaptive one", []transcoding{tc("hls", "audio/mpegurl", false), tc("hls", "audio/mp4; codecs=\"mp4a.40.2\"", false)}, "mp4a"},
		{"never a preview", []transcoding{tc("progressive", "audio/mpeg", true), tc("hls", "audio/mpegurl", false)}, "mpegurl"},
	}
	for _, tt := range tests {
		got, ok := track{Media: struct {
			Transcodings []transcoding `json:"transcodings"`
		}{tt.have}}.bestAudio()
		if !ok || !strings.Contains(got.URL, tt.want) {
			t.Errorf("%s: picked %q", tt.name, got.URL)
		}
	}
}

func TestIsSoundCloud(t *testing.T) {
	tests := map[string]bool{
		"https://soundcloud.com/someone/a-song":    true,
		"https://m.soundcloud.com/someone/a-song":  true,
		"https://on.soundcloud.com/AbCdEf":         true,
		"https://www.soundcloud.com/someone":       true,
		"https://notsoundcloud.com/someone/a-song": false,
		"https://youtu.be/LDY_XyxBu8A":             false,
		"a song":                                   false,
	}
	for in, want := range tests {
		if got := songs.IsSoundCloud(in); got != want {
			t.Errorf("IsSoundCloud(%q) = %v, want %v", in, got, want)
		}
	}
}
