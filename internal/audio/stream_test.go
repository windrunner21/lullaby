package audio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ffmpegPath returns ffmpeg's path, or skips the test without it.
func ffmpegPath(t *testing.T) string {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg isn't installed")
	}
	return path
}

// tone has ffmpeg make one second of a 440 Hz tone, encoded with args.
func tone(t *testing.T, ffmpeg string, args ...string) []byte {
	t.Helper()
	args = append([]string{"-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1"}, args...)
	out, err := exec.Command(ffmpeg, append(args, "pipe:1")...).Output()
	if err != nil {
		t.Fatalf("make a test song: %v", err)
	}
	return out
}

// play takes every frame out of s the way Discord would, without the 20 ms
// between them, and returns how many there were.
func play(t *testing.T, s *Stream) int {
	t.Helper()
	n := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		f, err := s.ProvideOpusFrame()
		switch {
		case err == io.EOF:
			<-s.Done()
			return n
		case err != nil:
			t.Fatal(err)
		case f == nil:
			time.Sleep(time.Millisecond)
		default:
			n++
		}
	}
	t.Fatal("the song never ended")
	return 0
}

func TestStream(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	tests := []struct {
		name  string
		input []string
	}{
		// What the fast path gives: AAC in fragmented MP4.
		{"AAC", []string{"-c:a", "aac", "-f", "mp4", "-movflags", "frag_keyframe+empty_moov"}},
		// What yt-dlp gives for YouTube: Opus in WebM.
		{"Opus", []string{"-c:a", "libopus", "-f", "webm"}},
		// Opus in 60 ms frames, as other sites can give: Discord gets 20 ms
		// ones all the same, or it would play three times too fast.
		{"Opus in 60 ms frames", []string{"-c:a", "libopus", "-frame_duration", "60", "-f", "webm"}},
	}
	for _, tt := range tests {
		s, err := Start(context.Background(), ffmpeg)
		if err != nil {
			t.Fatal(err)
		}
		s.Feed(io.NopCloser(bytes.NewReader(tone(t, ffmpeg, tt.input...))))
		n := play(t, s)
		s.Close()

		if err := s.Err(); err != nil {
			t.Errorf("%s: %v", tt.name, err)
		}
		// One second is 50 frames of 20 ms. AAC adds about 50 ms of its own
		// at the start, and encoders pad the last frame.
		if n < 49 || n > 54 {
			t.Errorf("%s: %d frames, want about 50", tt.name, n)
		}
		if played := s.Played(); played != time.Duration(n)*frame {
			t.Errorf("%s: Played = %v, want %v", tt.name, played, time.Duration(n)*frame)
		}
	}
}

func TestStreamErrors(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	song := tone(t, ffmpeg, "-c:a", "aac", "-f", "mp4", "-movflags", "frag_keyframe+empty_moov")
	tests := []struct {
		name  string
		audio io.Reader
		want  string
	}{
		{"not a song", strings.NewReader("this is not a song"), "ffmpeg"},
		{"download fails halfway", io.MultiReader(bytes.NewReader(song[:len(song)/2]), failing{}), "download"},
	}
	for _, tt := range tests {
		s, err := Start(context.Background(), ffmpeg)
		if err != nil {
			t.Fatal(err)
		}
		s.Feed(io.NopCloser(tt.audio))
		play(t, s)
		s.Close()
		if err := s.Err(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: Err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}
}

func TestStreamBrokenOutput(t *testing.T) {
	// An "ffmpeg" that writes something that isn't Ogg, then keeps writing.
	fake := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'this is not an ogg page, not at all'\nexec cat /dev/zero\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Start(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("waited forever for an ffmpeg nobody reads anymore")
	}
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "Ogg") {
		t.Errorf("Err = %v, want it to say the output wasn't Ogg", err)
	}
}

// failing is a download that breaks.
type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestStreamNeverMakesDiscordWait(t *testing.T) {
	s, err := Start(context.Background(), ffmpegPath(t))
	if err != nil {
		t.Fatal(err)
	}
	// A download that hasn't sent anything yet.
	audio, _ := io.Pipe()
	s.Feed(audio)

	start := time.Now()
	f, err := s.ProvideOpusFrame()
	if f != nil || err != nil || time.Since(start) > 5*time.Millisecond {
		t.Errorf("ProvideOpusFrame = %v, %v after %v; want nothing, right away", f, err, time.Since(start))
	}

	s.Close()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ffmpeg kept running after Close")
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err after Close = %v, want nil", err)
	}
	if _, err := audio.Read(make([]byte, 1)); err == nil {
		t.Error("the download is still open after Close")
	}
}

func TestOpenPlaylist(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	// A playlist of half-second AAC pieces, the way SoundCloud sends songs.
	dir := t.TempDir()
	out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "aac", "-f", "hls", "-hls_time", "0.5", "-hls_playlist_type", "vod", filepath.Join(dir, "song.m3u8")).CombinedOutput()
	if err != nil {
		t.Fatalf("make a test playlist: %v: %s", err, out)
	}
	var agents sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agents.Store(r.URL.Path, r.Header.Get("User-Agent"))
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer srv.Close()

	s, err := Open(context.Background(), ffmpeg, Source{URL: srv.URL + "/song.m3u8", Header: http.Header{"User-Agent": {"test-agent"}}})
	if err != nil {
		t.Fatal(err)
	}
	n := play(t, s)
	s.Close()
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 49 || n > 56 {
		t.Errorf("%d frames, want about 50", n)
	}
	pieces := 0
	agents.Range(func(path, agent any) bool {
		if agent != "test-agent" {
			t.Errorf("%s was asked for as %q, want the site's headers on every request", path, agent)
		}
		if strings.HasSuffix(path.(string), ".ts") {
			pieces++
		}
		return true
	})
	if pieces < 2 {
		t.Errorf("ffmpeg fetched %d pieces, want the playlist's 2", pieces)
	}
}

func TestOpenOnlyFollowsTheWeb(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	os.WriteFile(secret, []byte("nobody should hear this"), 0o600)
	playlist := "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\nfile://" + secret + "\n#EXT-X-ENDLIST\n"
	os.WriteFile(filepath.Join(dir, "sneaky.m3u8"), []byte(playlist), 0o600)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()

	s, err := Open(context.Background(), ffmpeg, Source{URL: srv.URL + "/sneaky.m3u8"})
	if err != nil {
		t.Fatal(err)
	}
	if n := play(t, s); n != 0 {
		t.Errorf("played %d frames from a playlist pointing at a local file", n)
	}
	s.Close()
}

func TestStreamStopsWhenClosed(t *testing.T) {
	ffmpeg := ffmpegPath(t)
	s, err := Start(context.Background(), ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	s.Feed(io.NopCloser(bytes.NewReader(tone(t, ffmpeg, "-c:a", "aac", "-f", "mp4", "-movflags", "frag_keyframe+empty_moov"))))
	<-s.Done() // every frame is made and waiting
	if f, _ := s.ProvideOpusFrame(); f == nil {
		t.Fatal("no frame from a finished encode")
	}
	s.Close()
	if f, err := s.ProvideOpusFrame(); f != nil || err != io.EOF {
		t.Errorf("after Close: %d bytes, %v; want io.EOF and none of the waiting frames", len(f), err)
	}
}

func TestStartWithoutFFmpeg(t *testing.T) {
	if _, err := Start(context.Background(), "/nonexistent/ffmpeg"); err == nil {
		t.Error("Start with a missing ffmpeg worked")
	}
}
