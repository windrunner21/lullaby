package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/windrunner21/lullaby/internal/redact"
	"github.com/windrunner21/lullaby/songs"
)

const (
	// BaseURL is YouTube's own API, the one its apps use.
	BaseURL = "https://youtubei.googleapis.com/youtubei/v1"
	// MusicBaseURL is the same API for YouTube Music, which finds songs best.
	MusicBaseURL = "https://music.youtube.com/youtubei/v1"

	// iosVersion is the iPhone app the fast path pretends to be. YouTube
	// stops accepting old versions now and then; a newer one from
	// youtube-source's Ios.java usually fixes it.
	iosVersion     = "21.32.4"
	iosAgent       = "com.google.ios.youtube/" + iosVersion + " (iPhone16,2; U; CPU iOS 18_1_0 like Mac OS X;)"
	androidVersion = "21.02.35"
	androidAgent   = "com.google.android.youtube/" + androidVersion + " (Linux; U; Android 11) gzip"
	musicVersion   = "1.20240724.00.00"
	browserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
	// musicSongs limits a YouTube Music search to songs.
	musicSongs = "Eg-KAQwIARAAGAAgACgAMABqChADEAQQCRAFEAo="

	// visitorFor is how long one visitor ID is used, as youtube-source does.
	visitorFor = 10 * time.Minute
	// warmVideo is looked up at startup to open the connections. It has been
	// up since 2009.
	warmVideo = "dQw4w9WgXcQ"
	// timeout is how long one request to the API may take.
	timeout = 10 * time.Second
	// maxBody is far more than the biggest answer, a search at about 200 KB.
	maxBody = 8 << 20
)

// YouTube asks YouTube's API for songs the way its iPhone app does, which gets
// plain audio links without running YouTube's JavaScript. It's the fast path:
// about 0.2 s from a link to audio on the Pi. The requests follow
// youtube-source (github.com/lavalink-devs/youtube-source, MIT).
type YouTube struct {
	// Logger is where it logs. Nil is slog.Default().
	Logger *slog.Logger

	http      *http.Client
	base      string
	musicBase string

	mu        sync.Mutex
	visitor   string
	visitorAt time.Time
	renewing  bool // a new visitor ID is on its way
}

// NewYouTube returns a client for the API at base and musicBase, usually
// BaseURL and MusicBaseURL. Downloading the audio with the same client reuses
// its connections.
func NewYouTube(client *http.Client, base, musicBase string) *YouTube {
	return &YouTube{http: client, base: base, musicBase: musicBase}
}

// Warm opens the connections to YouTube's API and its video servers by
// looking up one song and asking for a byte of it, so the first real song
// doesn't wait for them. On the Pi, that song then took 0.35 s instead of 1.7 s.
func (y *YouTube) Warm(ctx context.Context) error {
	song, err := y.Lookup(ctx, warmVideo)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, song.Audio.URL, nil)
	if err != nil {
		return err
	}
	req.Header = song.Audio.Header.Clone()
	req.Header.Set("Range", "bytes=0-0")
	resp, err := y.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10)) // so the connection can be reused
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("youtube audio: %s", resp.Status)
	}
	return nil
}

// Touch opens connections to YouTube's API and YouTube Music's if there are
// none open, so the next search and song don't wait for them: Go closes
// connections unused for a while, and YouTube may close them sooner. It asks
// for nothing but each API's door, which answers 404.
func (y *YouTube) Touch(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wg sync.WaitGroup
	for base, agent := range map[string]string{y.base: iosAgent, y.musicBase: browserAgent} {
		wg.Go(func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/", nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", agent)
			if resp, err := y.http.Do(req); err == nil {
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
}

// Lookup finds the song with the given video ID and its audio link.
func (y *YouTube) Lookup(ctx context.Context, id string) (songs.Song, error) {
	// Like youtube-source, it says it's a player embedded on another site,
	// which YouTube lets play more videos.
	body := map[string]any{
		"context": map[string]any{
			"client":     map[string]any{"clientName": "IOS", "clientVersion": iosVersion, "clientScreen": "EMBED"},
			"user":       map[string]any{"lockedSafetyMode": false},
			"thirdParty": map[string]any{"embedUrl": "https://google.com"},
		},
		"videoId":        id,
		"racyCheckOk":    true,
		"contentCheckOk": true,
		"params":         "2AMB",
	}
	var out playerResponse
	if err := y.do(ctx, y.base, "player", iosAgent, y.visitorID(ctx), body, &out); err != nil {
		return songs.Song{}, err
	}
	return out.song(id, time.Now())
}

// Search finds songs on YouTube Music. The results have no audio yet; Lookup
// gets it for the one that's picked.
func (y *YouTube) Search(ctx context.Context, query string) ([]songs.Song, error) {
	body := map[string]any{
		"context": map[string]any{"client": map[string]any{"clientName": "WEB_REMIX", "clientVersion": musicVersion}},
		"query":   query,
		"params":  musicSongs,
	}
	var out musicSearch
	if err := y.do(ctx, y.musicBase, "search", browserAgent, y.visitorID(ctx), body, &out); err != nil {
		return nil, err
	}
	return out.songs(), nil
}

// visitorID returns a visitor ID for YouTube. The first request waits for
// the first one, which Warm gets at startup. After that, a new one comes
// every 10 minutes in the background, and requests use the one before it
// meanwhile, so no song or search waits a round trip for it.
func (y *YouTube) visitorID(ctx context.Context) string {
	y.mu.Lock()
	visitor, due := y.visitor, time.Since(y.visitorAt) >= visitorFor && !y.renewing
	if due {
		y.renewing = true
	}
	y.mu.Unlock()
	switch {
	case !due:
		return visitor
	case visitor == "":
		return y.renewVisitor(ctx)
	}
	go y.renewVisitor(context.WithoutCancel(ctx))
	return visitor
}

// renewVisitor gets a new visitor ID. Requests still go out without one, so
// a failure is only logged, and waits 10 minutes like a success, so a broken
// endpoint doesn't slow every song.
func (y *YouTube) renewVisitor(ctx context.Context) string {
	body := map[string]any{"context": map[string]any{"client": map[string]any{
		"clientName": "ANDROID", "clientVersion": androidVersion, "androidSdkVersion": 30,
	}}}
	var out struct {
		ResponseContext struct {
			VisitorData string `json:"visitorData"`
		} `json:"responseContext"`
	}
	err := y.do(ctx, y.base, "visitor_id", androidAgent, "", body, &out)

	y.mu.Lock()
	defer y.mu.Unlock()
	y.visitorAt, y.renewing = time.Now(), false
	if err != nil {
		logger(y.Logger).Warn("youtube visitor id failed", redact.Attr(err))
	} else if v := out.ResponseContext.VisitorData; v != "" {
		y.visitor = v
	}
	return y.visitor // an old one is better than none
}

// do sends body to an endpoint of the API at base as the app with the given
// user agent, and decodes the answer into out.
func (y *YouTube) do(ctx context.Context, base, endpoint, agent, visitor string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+endpoint+"?prettyPrint=false", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", agent)
	if visitor != "" {
		req.Header.Set("X-Goog-Visitor-Id", visitor)
	}

	resp, err := y.http.Do(req)
	if err != nil {
		return fmt.Errorf("youtube %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("youtube %s: %s", endpoint, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("youtube %s: decode: %w", endpoint, err)
	}
	return nil
}

type thumbnail struct {
	URL   string `json:"url"`
	Width int    `json:"width"`
}

// largest is the widest thumbnail's link.
func largest(thumbnails []thumbnail) string {
	var best thumbnail
	for _, t := range thumbnails {
		if t.Width >= best.Width {
			best = t
		}
	}
	return best.URL
}

// playerResponse is the part of the player endpoint's answer that's used.
type playerResponse struct {
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	StreamingData struct {
		ExpiresInSeconds string   `json:"expiresInSeconds"`
		AdaptiveFormats  []format `json:"adaptiveFormats"`
	} `json:"streamingData"`
	VideoDetails struct {
		Title         string `json:"title"`
		Author        string `json:"author"`
		LengthSeconds string `json:"lengthSeconds"`
		Thumbnail     struct {
			Thumbnails []thumbnail `json:"thumbnails"`
		} `json:"thumbnail"`
	} `json:"videoDetails"`
}

// format is one way to download a video: one quality of its picture or sound.
type format struct {
	MimeType      string `json:"mimeType"`
	Bitrate       int    `json:"bitrate"`
	URL           string `json:"url"`
	ContentLength string `json:"contentLength"`
	// IsDrc marks a copy with its loudness evened out.
	IsDrc bool `json:"isDrc"`
	// AudioTrack is set on videos with more than one language, like
	// auto-dubbed ones.
	AudioTrack *struct {
		AudioIsDefault bool `json:"audioIsDefault"`
	} `json:"audioTrack"`
}

func (r playerResponse) song(id string, now time.Time) (songs.Song, error) {
	switch s := r.PlayabilityStatus; s.Status {
	case "OK":
	case "ERROR":
		return songs.Song{}, fmt.Errorf("youtube %s: %w: %s", id, songs.ErrUnplayable, s.Reason)
	case "LOGIN_REQUIRED":
		if strings.Contains(s.Reason, "not a bot") {
			return songs.Song{}, fmt.Errorf("youtube %s: %w", id, songs.ErrBotCheck)
		}
		fallthrough
	default:
		// Like LOGIN_REQUIRED for an age check, which yt-dlp may get past.
		return songs.Song{}, fmt.Errorf("youtube %s: %s: %s", id, s.Status, s.Reason)
	}

	audio, ok := bestAudio(r.StreamingData.AdaptiveFormats)
	if !ok {
		return songs.Song{}, fmt.Errorf("youtube %s: %w", id, songs.ErrNoAudio)
	}
	size, _ := strconv.ParseInt(audio.ContentLength, 10, 64)
	d := r.VideoDetails
	seconds, _ := strconv.Atoi(d.LengthSeconds)
	song := songs.Song{
		ID:        id,
		Link:      songs.WatchLink(id),
		Title:     d.Title,
		Artist:    artistName(d.Author),
		Length:    time.Duration(seconds) * time.Second,
		Thumbnail: largest(d.Thumbnail.Thumbnails),
		Audio: songs.Audio{
			URL:    audio.URL,
			Header: http.Header{"User-Agent": {iosAgent}},
			Size:   size,
		},
	}
	if s, err := strconv.Atoi(r.StreamingData.ExpiresInSeconds); err == nil && s > 0 {
		song.Audio.Expires = now.Add(time.Duration(s) * time.Second)
	}
	return song, nil
}

// bestAudio picks the sound with the highest bitrate, in the video's own
// language and loudness.
func bestAudio(formats []format) (format, bool) {
	var best format
	for _, f := range formats {
		if !strings.HasPrefix(f.MimeType, "audio/") || f.URL == "" || f.IsDrc {
			continue
		}
		if f.AudioTrack != nil && !f.AudioTrack.AudioIsDefault {
			continue // a dubbed language
		}
		if f.Bitrate > best.Bitrate {
			best = f
		}
	}
	return best, best.URL != ""
}

// artistName drops what YouTube adds to an artist's channel name, like
// "SZAVEVO" or "SZA - Topic".
func artistName(author string) string {
	name := strings.TrimSuffix(strings.TrimSuffix(author, " - Topic"), "VEVO")
	if name == "" {
		return author
	}
	return name
}

// musicSearch is the part of a YouTube Music search's answer that's used.
type musicSearch struct {
	Contents struct {
		Tabbed struct {
			Tabs []struct {
				TabRenderer struct {
					Content struct {
						SectionList struct {
							Contents []struct {
								Shelf *struct {
									Contents []struct {
										Item musicItem `json:"musicResponsiveListItemRenderer"`
									} `json:"contents"`
								} `json:"musicShelfRenderer"`
							} `json:"contents"`
						} `json:"sectionListRenderer"`
					} `json:"content"`
				} `json:"tabRenderer"`
			} `json:"tabs"`
		} `json:"tabbedSearchResultsRenderer"`
	} `json:"contents"`
}

func (s musicSearch) songs() []songs.Song {
	var songs []songs.Song
	for _, tab := range s.Contents.Tabbed.Tabs {
		for _, section := range tab.TabRenderer.Content.SectionList.Contents {
			if section.Shelf == nil {
				continue
			}
			for _, c := range section.Shelf.Contents {
				if song, ok := c.Item.song(); ok {
					songs = append(songs, song)
				}
			}
		}
	}
	return songs
}

// musicItem is one result of a YouTube Music search.
type musicItem struct {
	PlaylistItemData struct {
		VideoID string `json:"videoId"`
	} `json:"playlistItemData"`
	FlexColumns []struct {
		Column struct {
			Text struct {
				Runs []struct {
					Text string `json:"text"`
				} `json:"runs"`
			} `json:"text"`
		} `json:"musicResponsiveListItemFlexColumnRenderer"`
	} `json:"flexColumns"`
	Thumbnail struct {
		Renderer struct {
			Thumbnail struct {
				Thumbnails []thumbnail `json:"thumbnails"`
			} `json:"thumbnail"`
		} `json:"musicThumbnailRenderer"`
	} `json:"thumbnail"`
}

func (m musicItem) song() (songs.Song, bool) {
	id := m.PlaylistItemData.VideoID
	if _, ok := songs.YouTubeID(songs.WatchLink(id)); !ok || len(m.FlexColumns) < 2 {
		return songs.Song{}, false
	}
	// The second column reads like "SZA • SOS • 3:22": artists, album, length.
	details := strings.Split(m.column(1), " • ")
	return songs.Song{
		ID:        id,
		Link:      songs.WatchLink(id),
		Title:     m.column(0),
		Artist:    details[0],
		Length:    parseLength(details[len(details)-1]),
		Thumbnail: largest(m.Thumbnail.Renderer.Thumbnail.Thumbnails),
	}, true
}

func (m musicItem) column(i int) string {
	var b strings.Builder
	for _, r := range m.FlexColumns[i].Column.Text.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

// parseLength reads a length like "3:22" or "1:02:03", or returns 0.
func parseLength(s string) time.Duration {
	var total time.Duration
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + time.Duration(n)
	}
	return total * time.Second
}
