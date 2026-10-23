package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFetchHLS(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	// Three seconds in one-second AAC pieces of fragmented MP4 after a header
	// piece: how SoundCloud sends songs.
	dir := t.TempDir()
	out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:a", "aac", "-f", "hls", "-hls_time", "1", "-hls_segment_type", "fmp4", "-hls_playlist_type", "vod",
		filepath.Join(dir, "song.m3u8")).CombinedOutput()
	if err != nil {
		t.Fatalf("make a test playlist: %v: %s", err, out)
	}
	var mu sync.Mutex
	asked := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path] = r.Header.Get("User-Agent")
		mu.Unlock()
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer srv.Close()

	// ffmpeg starts first, as it does while a song is looked up.
	s, err := Start(context.Background(), ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	pieces, err := FetchHLS(context.Background(), srv.Client(), Source{URL: srv.URL + "/song.m3u8", Header: http.Header{"User-Agent": {"test-agent"}}})
	if err != nil {
		t.Fatal(err)
	}
	s.Feed(pieces)
	n := play(t, s)
	s.Close()
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 148 || n > 156 {
		t.Errorf("%d frames, want about 150 for three seconds", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 5 {
		t.Errorf("asked for %d files, want the playlist, the header piece and three pieces", len(asked))
	}
	for path, agent := range asked {
		if agent != "test-agent" {
			t.Errorf("%s was asked for as %q, want the site's headers on every request", path, agent)
		}
	}
}

func TestParsePlaylist(t *testing.T) {
	base, _ := url.Parse("https://cdn.example/song/aac/playlist.m3u8?expires=1")
	const pieces = "#EXTINF:10.0,\npiece0.m4s\n#EXTINF:10.0,\nhttps://other.example/piece1.m4s\n"
	tests := []struct {
		name    string
		text    string
		want    []string
		variant string
		simple  bool
	}{
		{"finished", "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MAP:URI=\"init.mp4\"\n" + pieces + "#EXT-X-ENDLIST\n",
			[]string{"https://cdn.example/song/aac/init.mp4", "https://cdn.example/song/aac/piece0.m4s", "https://other.example/piece1.m4s"}, "", true},
		{"no encryption said out loud", "#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n" + pieces + "#EXT-X-ENDLIST\n",
			[]string{"https://cdn.example/song/aac/piece0.m4s", "https://other.example/piece1.m4s"}, "", true},
		{"playlist of playlists", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=96000\nlow/playlist.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=256000\nhigh/playlist.m3u8\n",
			nil, "https://cdn.example/song/aac/low/playlist.m3u8", true},
		{"live", "#EXTM3U\n" + pieces, nil, "", false},
		{"encrypted", "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n" + pieces + "#EXT-X-ENDLIST\n", nil, "", false},
		{"byte ranges", "#EXTM3U\n#EXTINF:10.0,\n#EXT-X-BYTERANGE:1000@0\nsong.mp4\n#EXT-X-ENDLIST\n", nil, "", false},
	}
	for _, tt := range tests {
		got, variant, err := parsePlaylist(tt.text, base)
		if errors.Is(err, ErrNotSimple) == tt.simple {
			t.Errorf("%s: error = %v", tt.name, err)
			continue
		}
		if strings.Join(got, " ") != strings.Join(tt.want, " ") || variant != tt.variant {
			t.Errorf("%s: pieces %q, variant %q; want %q, %q", tt.name, got, variant, tt.want, tt.variant)
		}
	}
	if _, _, err := parsePlaylist("<html>not a playlist</html>", base); err == nil {
		t.Error("a web page passed as a playlist")
	}
}

func TestGetPiecesInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/0" {
			time.Sleep(100 * time.Millisecond) // the first piece is the slowest
		}
		io.WriteString(w, "piece"+r.URL.Path)
	}))
	defer srv.Close()
	var links []string
	for i := range 7 {
		links = append(links, fmt.Sprintf("%s/%d", srv.URL, i))
	}
	var got bytes.Buffer
	err := getPieces(context.Background(), srv.Client(), nil, links, func(b []byte) error {
		got.Write(b)
		got.WriteByte(' ')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "piece/0 piece/1 piece/2 piece/3 piece/4 piece/5 piece/6 "; got.String() != want {
		t.Errorf("got %q, want the pieces in order", got.String())
	}
}

func TestFetchHLSLeavesLiveToFFmpeg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXTINF:2.0,\nlive0.ts\n#EXTINF:2.0,\nlive1.ts\n")
	}))
	defer srv.Close()
	if _, err := FetchHLS(context.Background(), srv.Client(), Source{URL: srv.URL + "/live.m3u8"}); !errors.Is(err, ErrNotSimple) {
		t.Errorf("a live playlist: %v, want ErrNotSimple", err)
	}
}
