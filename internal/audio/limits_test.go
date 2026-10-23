package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHLSTooBig(t *testing.T) {
	// A playlist's first maxPlaylist bytes are a song; the rest would be cut
	// off without a word, so it's refused instead.
	text := "#EXTM3U\n#EXTINF:1,\nfirst.ts\n#EXT-X-ENDLIST\n#"
	text += strings.Repeat("x", maxPlaylist-len(text)) + "\n#EXTINF:1,\nsecond.ts\n"
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: -1,
			Body: io.NopCloser(strings.NewReader(text))}, nil
	})}
	if _, err := hlsPieces(context.Background(), client, Source{URL: "https://audio.invalid/list.m3u8"}); !errors.Is(err, ErrTooBig) {
		t.Errorf("hlsPieces = %v, want ErrTooBig", err)
	}
	// Said up front, it isn't downloaded at all.
	big := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: maxPiece + 1,
			Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if _, err := getWhole(context.Background(), big, nil, "https://audio.invalid/a.ts", maxPiece); !errors.Is(err, ErrTooBig) {
		t.Errorf("getWhole = %v, want ErrTooBig", err)
	}
}

func TestHLSTooManyPieces(t *testing.T) {
	base, _ := url.Parse("https://audio.invalid/list.m3u8")
	text := "#EXTM3U\n" + strings.Repeat("#EXTINF:1,\nx\n", maxPieces+1) + "#EXT-X-ENDLIST\n"
	if _, _, err := parsePlaylist(text, base); !errors.Is(err, ErrTooBig) {
		t.Errorf("parsePlaylist = %v, want ErrTooBig past %d pieces", err, maxPieces)
	}
}

func TestHLSLimitsHoldForPlaylistsLeftToFFmpeg(t *testing.T) {
	// Encrypted, so it would go to ffmpeg; too many pieces, so it doesn't.
	base, _ := url.Parse("https://audio.invalid/list.m3u8")
	text := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n" + strings.Repeat("#EXTINF:10,\npart.ts\n", maxPieces+1) + "#EXT-X-ENDLIST\n"
	if _, _, err := parsePlaylist(text, base); !errors.Is(err, ErrTooBig) {
		t.Errorf("parsePlaylist = %v, want ErrTooBig before ErrNotSimple", err)
	}
}

func TestHLSLinksHaveABudget(t *testing.T) {
	// Short relative links, each made whole with a long playlist link in
	// front: a tiny playlist would come to megabytes of links.
	base, _ := url.Parse("https://audio.invalid/" + strings.Repeat("a", 12<<10) + "/list.m3u8")
	text := "#EXTM3U\n" + strings.Repeat("x\n", 2000) + "#EXT-X-ENDLIST\n"
	if _, _, err := parsePlaylist(text, base); !errors.Is(err, ErrTooBig) {
		t.Errorf("parsePlaylist = %v, want ErrTooBig for %d KB of links", err, 2000*12)
	}
	long, _ := url.Parse("https://audio.invalid/" + strings.Repeat("a", maxLink) + "/list.m3u8")
	if _, _, err := parsePlaylist("#EXTM3U\nx\n#EXT-X-ENDLIST\n", long); !errors.Is(err, ErrTooBig) {
		t.Errorf("parsePlaylist = %v, want ErrTooBig for a link over %d bytes", err, maxLink)
	}
}

func TestHLSLimitsCountByteRangeHeaders(t *testing.T) {
	// Header pieces with byte ranges send the playlist to ffmpeg, but are
	// counted against the limits first.
	base, _ := url.Parse("https://audio.invalid/list.m3u8")
	long := "#EXTM3U\n#EXT-X-MAP:URI=\"https://audio.invalid/" + strings.Repeat("a", maxLink) + "\",BYTERANGE=\"1@0\"\nx.ts\n#EXT-X-ENDLIST\n"
	if _, _, err := parsePlaylist(long, base); !errors.Is(err, ErrTooBig) {
		t.Errorf("a header piece's link over %d bytes: %v, want ErrTooBig", maxLink, err)
	}
	var many strings.Builder
	many.WriteString("#EXTM3U\n")
	for i := range maxPieces/2 + 1 {
		fmt.Fprintf(&many, "#EXT-X-MAP:URI=\"init%d\",BYTERANGE=\"1@0\"\nx%d.ts\n", i, i)
	}
	many.WriteString("#EXT-X-ENDLIST\n")
	if _, _, err := parsePlaylist(many.String(), base); !errors.Is(err, ErrTooBig) {
		t.Errorf("%d header pieces and pieces: %v, want ErrTooBig", 2*(maxPieces/2+1), err)
	}
}

func TestStreamCloseDropsWaitingFrames(t *testing.T) {
	// A second of a song, all encoded and waiting.
	ffmpeg := ffmpegPath(t)
	s, err := Start(context.Background(), ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	s.Feed(io.NopCloser(bytes.NewReader(tone(t, ffmpeg, "-c:a", "aac", "-f", "adts"))))
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ffmpeg never finished")
	}
	if len(s.frames) == 0 {
		t.Fatal("no frames waiting")
	}
	s.Close()
	for deadline := time.Now().Add(2 * time.Second); len(s.frames) > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d frames still held after Close", len(s.frames))
		}
	}
}

func TestFetchGivesUpOnAQuietServer(t *testing.T) {
	oldQuiet, oldRetries := quietFor, retries
	quietFor, retries = 100*time.Millisecond, []time.Duration{10 * time.Millisecond}
	defer func() { quietFor, retries = oldQuiet, oldRetries }()
	stall := make(chan struct{})
	defer close(stall)
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// Everything in one go, as a server that ignores ranges sends it,
		// then nothing more.
		w.WriteHeader(http.StatusOK)
		w.Write(noise(1000))
		w.(http.Flusher).Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	})
	r := Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})
	defer r.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(r)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a server that went quiet passed for the whole song")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting on a server that went quiet")
	}
}

func TestFetchWaitingForRoomIsntQuiet(t *testing.T) {
	// More than fits ahead, read only after a while: the download waits for
	// room meanwhile, which mustn't count as the server going quiet.
	old := quietFor
	quietFor = 100 * time.Millisecond
	defer func() { quietFor = old }()
	data := noise(ahead + 3*piece)
	srv, _ := serve(t, ranges(data))
	r := Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})
	defer r.Close()
	time.Sleep(500 * time.Millisecond)
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("got %d bytes (%v), want all %d", len(got), err, len(data))
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
