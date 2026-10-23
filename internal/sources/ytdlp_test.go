package sources

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// yt-dlp's answer for Snooze, trimmed to the fields that are read and a few
// that aren't, saved on 2026-10-22 with the link's IP address taken out.
const ytdlpSnooze = `{
	"id": "LDY_XyxBu8A", "title": "SZA - Snooze (Official Video)", "uploader": "SZA", "channel": "SZA",
	"duration": 281, "webpage_url": "https://www.youtube.com/watch?v=LDY_XyxBu8A", "extractor_key": "Youtube",
	"format_id": "251", "protocol": "https", "acodec": "opus", "vcodec": "none", "ext": "webm",
	"filesize": 4767222, "abr": 135.837, "asr": 48000, "available_at": 1792696076,
	"thumbnail": "https://i.ytimg.com/vi/LDY_XyxBu8A/maxresdefault.jpg",
	"http_headers": {
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
		"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "en-us,en;q=0.5",
		"Sec-Fetch-Mode": "navigate"
	},
	"url": "https://rr2---sn-test.googlevideo.com/videoplayback?expire=1792710000&itag=251&source=youtube"
}`

// fakeYTDLP answers every run with out and err, and remembers the arguments.
func fakeYTDLP(bun, cacheDir, out string, err error) (*YTDLP, *[]string) {
	var got []string
	y := NewYTDLP("/usr/bin/yt-dlp", bun, cacheDir)
	y.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		got = append([]string{name}, args...)
		return []byte(out), err
	}
	return y, &got
}

func TestYTDLPLookup(t *testing.T) {
	y, args := fakeYTDLP("/opt/bun", "/srv/bot/yt-dlp", ytdlpSnooze, nil)
	song, err := y.Lookup(context.Background(), "https://youtu.be/LDY_XyxBu8A")
	if err != nil {
		t.Fatal(err)
	}

	if song.ID != "LDY_XyxBu8A" || song.Link != "https://www.youtube.com/watch?v=LDY_XyxBu8A" {
		t.Errorf("ID, Link = %q, %q", song.ID, song.Link)
	}
	if song.Title != "SZA - Snooze (Official Video)" || song.Artist != "SZA" || song.Length != 281*time.Second {
		t.Errorf("song = %+v", song)
	}
	a := song.Audio
	if a.Size != 4767222 || !strings.Contains(a.URL, "itag=251") {
		t.Errorf("Audio = %q, %d bytes; want itag 251", a.URL, a.Size)
	}
	if !strings.Contains(a.Header.Get("User-Agent"), "Chrome/149") || a.Header.Get("Sec-Fetch-Mode") != "navigate" {
		t.Errorf("audio headers = %v, want yt-dlp's", a.Header)
	}
	if !a.Expires.Equal(time.Unix(1792710000, 0)) {
		t.Errorf("Expires = %v, want the link's expire", a.Expires)
	}
	if !a.Ready.Equal(time.Unix(1792696076, 0)) {
		t.Errorf("Ready = %v, want available_at", a.Ready)
	}

	want := []string{"/usr/bin/yt-dlp", "--ignore-config", "--dump-single-json", "--no-playlist", "--flat-playlist", "--quiet", "--format", "bestaudio/best", "--no-warnings",
		"--no-js-runtimes", "--js-runtimes", "bun:/opt/bun", "--cache-dir", "/srv/bot/yt-dlp", "--", "https://youtu.be/LDY_XyxBu8A"}
	if !slices.Equal(*args, want) {
		t.Errorf("ran %q\nwant %q", *args, want)
	}
}

func TestYTDLPSignedIn(t *testing.T) {
	y, args := fakeYTDLP("/opt/bun", "", ytdlpSnooze, nil)
	signedIn := y.SignedIn("/srv/bot/youtube-cookies.txt")
	if _, err := signedIn.Lookup(context.Background(), "https://youtu.be/LDY_XyxBu8A"); err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(*args, "--cookies"); i < 0 || (*args)[i+1] != "/srv/bot/youtube-cookies.txt" {
		t.Errorf("ran %q, want the cookies", *args)
	}
	if slices.Contains(*args, "--no-warnings") {
		t.Errorf("ran %q, want warnings, which say when the cookies stop working", *args)
	}
	if _, err := y.Lookup(context.Background(), "https://youtu.be/LDY_XyxBu8A"); err != nil || slices.Contains(*args, "--cookies") {
		t.Errorf("the yt-dlp it came from ran %q, want it still signed out", *args)
	}

	// A lookup waiting for its turn gives up when it's canceled.
	signedIn.turn <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := signedIn.Lookup(ctx, "https://youtu.be/LDY_XyxBu8A"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting for a turn: %v, want the deadline", err)
	}
}

func TestYTDLPOldCookies(t *testing.T) {
	// What yt-dlp prints when the cookies were opened in a browser again,
	// followed by the bot check, since it's signed out after all.
	script := `echo "WARNING: [youtube] The provided YouTube account cookies are no longer valid. They have likely been rotated in the browser as a security measure." >&2
echo "ERROR: [youtube] LDY_XyxBu8A: Sign in to confirm you’re not a bot." >&2; exit 1`
	if _, err := run(context.Background(), "sh", "-c", script); !errors.Is(err, errOldCookies) {
		t.Errorf("error = %v, want errOldCookies", err)
	}
}

func TestYTDLPPlaylistLink(t *testing.T) {
	// A playlist comes back as one record, its songs not looked up.
	y, _ := fakeYTDLP("", "", `{"_type": "playlist", "id": "PL1", "title": "Mix", "entries": [{"_type": "url", "url": "https://example.com/1"}]}`, nil)
	if _, err := y.Lookup(context.Background(), "https://example.com/playlist/1"); !errors.Is(err, songs.ErrPlaylist) {
		t.Errorf("error = %v, want ErrPlaylist", err)
	}
}

func TestYTDLPWithoutBunOrCache(t *testing.T) {
	y, args := fakeYTDLP("", "", ytdlpSnooze, nil)
	if _, err := y.Lookup(context.Background(), "https://youtu.be/LDY_XyxBu8A"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(*args, "--js-runtimes") || slices.Contains(*args, "--cache-dir") || slices.Contains(*args, "--no-cache-dir") {
		t.Errorf("ran %q, want yt-dlp's own JavaScript runtime and its own cache", *args)
	}
}

func TestYTDLPOtherSite(t *testing.T) {
	soundcloud := `{"id": "1234567", "extractor_key": "Soundcloud", "webpage_url": "https://soundcloud.com/sza/snooze",
		"title": "Snooze", "artist": "SZA", "uploader": "SZA Official", "duration": 201.5,
		"protocol": "https", "acodec": "mp3", "ext": "mp3", "url": "https://cf-media.sndcdn.com/abc.mp3"}`
	y, _ := fakeYTDLP("", "", soundcloud, nil)
	song, err := y.Lookup(context.Background(), "https://soundcloud.com/sza/snooze")
	if err != nil {
		t.Fatal(err)
	}
	if song.ID != "" || song.Artist != "SZA" || song.Length != 201500*time.Millisecond {
		t.Errorf("song = %+v, want no YouTube ID, the artist over the uploader, and not Opus", song)
	}
	if !song.Audio.Expires.IsZero() {
		t.Errorf("Expires = %v, want unknown", song.Audio.Expires)
	}
}

func TestYTDLPPlaylist(t *testing.T) {
	// The shape of SoundCloud's answer on 2026-10-23: HLS is the only format.
	soundcloud := `{"id": "526764561", "extractor_key": "Soundcloud", "webpage_url": "https://soundcloud.com/someone/a-song",
		"title": "A Song", "uploader": "Someone", "duration": 128.125, "format_id": "hls_aac_96k",
		"protocol": "m3u8_native", "acodec": "mp4a.40.2", "ext": "m4a", "abr": 96,
		"url": "https://playback.media-streaming.soundcloud.cloud/abc/aac_96k/playlist.m3u8?expires=1792700000",
		"http_headers": {"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"}}`
	y, _ := fakeYTDLP("", "", soundcloud, nil)
	song, err := y.Lookup(context.Background(), "https://soundcloud.com/someone/a-song")
	if err != nil {
		t.Fatal(err)
	}
	if !song.Audio.Playlist || song.Length != 128125*time.Millisecond {
		t.Errorf("Audio = %+v, Length %v; want a playlist for ffmpeg to read", song.Audio, song.Length)
	}
}

func TestYTDLPBotCheck(t *testing.T) {
	// What yt-dlp printed on 2026-10-23, when YouTube flagged the address.
	script := `echo "ERROR: [youtube] MX7FSAnRPug: Sign in to confirm you’re not a bot. Use --cookies-from-browser or --cookies for the authentication." >&2; exit 1`
	if _, err := run(context.Background(), "sh", "-c", script); !errors.Is(err, songs.ErrBotCheck) {
		t.Errorf("error = %v, want ErrBotCheck", err)
	}
	if _, err := run(context.Background(), "sh", "-c", `echo "ERROR: Unsupported URL" >&2; exit 1`); errors.Is(err, songs.ErrBotCheck) || err == nil {
		t.Errorf("another failure = %v, want a plain error", err)
	}
}

func TestYTDLPErrors(t *testing.T) {
	broken := errors.New("yt-dlp: exit status 1: ERROR: [youtube] LDY_XyxBu8A: Video unavailable")
	dash := `{"id": "abc", "protocol": "http_dash_segments", "url": "https://example.com/manifest.mpd"}`
	tests := []struct {
		name string
		out  string
		err  error
	}{
		{"yt-dlp fails", "", broken},
		{"DASH pieces", dash, nil},
		{"not JSON", "Usage: yt-dlp [OPTIONS] URL", nil},
	}
	for _, tt := range tests {
		y, _ := fakeYTDLP("", "", tt.out, tt.err)
		if _, err := y.Lookup(context.Background(), "https://youtu.be/LDY_XyxBu8A"); err == nil {
			t.Errorf("%s: no error", tt.name)
		} else if tt.err != nil && !errors.Is(err, tt.err) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, tt.err)
		}
	}
}
