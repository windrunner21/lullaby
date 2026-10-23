package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// lookupFor is the longest one yt-dlp lookup may take, waiting for the spare
// account's turn included. On the Pi they took about 5 s, and up to 15 s
// when YouTube was slow.
var lookupFor = 30 * time.Second

// YTDLP runs yt-dlp, which knows YouTube and over a thousand other sites.
// It's the backup: about 5 s on the Pi, where the fast path takes 0.2 s.
type YTDLP struct {
	path     string
	bun      string
	cacheDir string
	cookies  string        // a spare YouTube account's cookies.txt, or empty
	turn     chan struct{} // lets one signed-in lookup run at a time
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewYTDLP runs the yt-dlp at path. bun is Bun's path, which yt-dlp needs to
// run YouTube's JavaScript; it supports Bun up to 1.3.14. Without it, yt-dlp
// uses Deno if that's installed. cacheDir is where yt-dlp keeps what it
// learned about YouTube's JavaScript, or empty for yt-dlp's own cache.
func NewYTDLP(path, bun, cacheDir string) *YTDLP {
	return &YTDLP{path: path, bun: bun, cacheDir: cacheDir, run: run}
}

// SignedIn returns a yt-dlp like y that signs in to YouTube with the cookies
// in the file at cookies, from a spare account. yt-dlp saves the cookies back
// as YouTube renews them, so the file must be writable, and only one
// signed-in lookup runs at a time.
func (y *YTDLP) SignedIn(cookies string) *YTDLP {
	signedIn := *y
	signedIn.cookies = cookies
	signedIn.turn = make(chan struct{}, 1)
	return &signedIn
}

// Lookup finds the song at link and its audio link.
func (y *YTDLP) Lookup(ctx context.Context, link string) (songs.Song, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupFor)
	defer cancel()
	// A playlist comes back as one record with its songs unopened, rather
	// than every song looked up.
	// --ignore-config: what yt-dlp does is up to the engine, not to config
	// files on the machine, which could sign in with a browser's cookies.
	args := []string{"--ignore-config", "--dump-single-json", "--no-playlist", "--flat-playlist", "--quiet", "--format", "bestaudio/best"}
	if y.cookies == "" {
		args = append(args, "--no-warnings")
	} else {
		// yt-dlp warns when the cookies stop working, so warnings stay on.
		args = append(args, "--cookies", y.cookies)
		select {
		case y.turn <- struct{}{}:
			defer func() { <-y.turn }()
		case <-ctx.Done():
			return songs.Song{}, ctx.Err()
		}
	}
	if y.bun != "" {
		args = append(args, "--no-js-runtimes", "--js-runtimes", "bun:"+y.bun)
	}
	if y.cacheDir != "" {
		args = append(args, "--cache-dir", y.cacheDir)
	}
	out, err := y.run(ctx, y.path, append(args, "--", link)...)
	if err != nil {
		return songs.Song{}, err
	}

	var v struct {
		Type       string            `json:"_type"` // "playlist" for a playlist
		ID         string            `json:"id"`
		Extractor  string            `json:"extractor_key"`
		WebpageURL string            `json:"webpage_url"`
		Title      string            `json:"title"`
		Artist     string            `json:"artist"`
		Uploader   string            `json:"uploader"`
		Duration   float64           `json:"duration"`
		Thumbnail  string            `json:"thumbnail"`
		URL        string            `json:"url"`
		Protocol   string            `json:"protocol"`
		Headers    map[string]string `json:"http_headers"`
		Filesize   int64             `json:"filesize"`
		Available  float64           `json:"available_at"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return songs.Song{}, fmt.Errorf("yt-dlp: decode: %w", err)
	}
	if v.Type == "playlist" || v.Type == "multi_video" {
		return songs.Song{}, fmt.Errorf("yt-dlp %s: %w", link, songs.ErrPlaylist)
	}
	playlist := v.Protocol == "m3u8" || v.Protocol == "m3u8_native"
	if v.Protocol != "https" && v.Protocol != "http" && !playlist {
		return songs.Song{}, fmt.Errorf("yt-dlp: can't play %q streams yet", v.Protocol)
	}

	song := songs.Song{
		Link:      v.WebpageURL,
		Title:     v.Title,
		Artist:    v.Artist,
		Length:    time.Duration(v.Duration * float64(time.Second)),
		Thumbnail: v.Thumbnail,
		Audio: songs.Audio{
			URL:      v.URL,
			Header:   http.Header{},
			Size:     v.Filesize,
			Playlist: playlist,
		},
	}
	if v.Extractor == "Youtube" {
		song.ID = v.ID
	}
	if song.Artist == "" {
		song.Artist = artistName(v.Uploader)
	}
	if song.Link == "" {
		song.Link = link
	}
	for k, val := range v.Headers {
		song.Audio.Header.Set(k, val)
	}
	if v.Available > 0 {
		song.Audio.Ready = time.Unix(int64(v.Available), 0)
	}
	// YouTube's links say when they stop working.
	if u, err := url.Parse(v.URL); err == nil {
		if s, err := strconv.ParseInt(u.Query().Get("expire"), 10, 64); err == nil {
			song.Audio.Expires = time.Unix(s, 0)
		}
	}
	return song, nil
}

// errOldCookies is returned when yt-dlp says the account's cookies stopped
// working, which happens when they're opened in a browser again.
var errOldCookies = errors.New("the account's cookies stopped working")

// run runs a program and returns what it printed, or its last error line.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &bounded{max: maxAnswer, over: cancel}
	var stderr stderrTail
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, &stderr
	inGroup(cmd)
	// If something still holds yt-dlp's output after it's stopped, stop
	// waiting for it.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	stopGroup(cmd) // a child it left behind, if it crashed
	if out.overflowed() {
		return nil, fmt.Errorf("yt-dlp: %w: its answer is over %d MB", songs.ErrUnplayable, maxAnswer>>20)
	}
	if err != nil {
		all := strings.TrimSpace(stderr.String())
		last := all[strings.LastIndex(all, "\n")+1:]
		switch {
		case stderr.cookiesStale():
			return nil, fmt.Errorf("yt-dlp: %w", errOldCookies)
		case strings.Contains(last, "not a bot"):
			return nil, fmt.Errorf("yt-dlp: %w", songs.ErrBotCheck)
		}
		return nil, fmt.Errorf("yt-dlp: %w: %s", err, last)
	}
	return out.bytes(), nil
}

// maxAnswer is far more than yt-dlp's answer about one song, which is under
// 1 MB with every format listed.
const maxAnswer = 8 << 20

// bounded keeps what a program writes up to max bytes, and calls over once
// when it writes more.
type bounded struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	max  int
	over func()
	full bool
}

func (b *bounded) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full || b.buf.Len()+len(p) > b.max {
		if !b.full {
			b.full = true
			b.over()
		}
		return 0, errors.New("too much output")
	}
	return b.buf.Write(p)
}

func (b *bounded) overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.full
}

func (b *bounded) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

// stderrTail keeps the end of what a program says on stderr, where its error
// is, and notes when yt-dlp warns, anywhere in it, that the cookies stopped
// working.
type stderrTail struct {
	mu           sync.Mutex
	tail         []byte
	staleCookies bool
}

const (
	tailSize    = 64 << 10
	cookiesGone = "cookies are no longer valid"
)

func (e *stderrTail) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// The warning can be split between two writes.
	edge := e.tail[max(0, len(e.tail)-len(cookiesGone)):]
	if bytes.Contains(append(slices.Clip(edge), p...), []byte(cookiesGone)) {
		e.staleCookies = true
	}
	e.tail = append(e.tail, p...)
	if len(e.tail) > tailSize {
		e.tail = append(e.tail[:0], e.tail[len(e.tail)-tailSize:]...)
	}
	return len(p), nil
}

func (e *stderrTail) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return string(e.tail)
}

func (e *stderrTail) cookiesStale() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.staleCookies
}
