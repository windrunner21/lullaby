package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

const (
	// SoundCloudSite is SoundCloud's website, whose scripts hold the key its
	// API wants.
	SoundCloudSite = "https://soundcloud.com"
	// SoundCloudAPI is the API SoundCloud's own website uses.
	SoundCloudAPI = "https://api-v2.soundcloud.com"
	// maxScript is far more than the biggest of the website's scripts, about 3 MB.
	maxScript = 16 << 20
)

// SoundCloud asks SoundCloud's API for songs the way its website does: about
// 0.45 s on the Pi, where yt-dlp took 2.4 s. The key the API wants comes from
// the website's scripts, as lavaplayer's SoundCloud source gets it.
type SoundCloud struct {
	http *http.Client
	site string
	api  string

	mu  sync.Mutex
	key string
}

// NewSoundCloud returns a client for the website at site and the API at api,
// usually SoundCloudSite and SoundCloudAPI.
func NewSoundCloud(client *http.Client, site, api string) *SoundCloud {
	return &SoundCloud{http: client, site: site, api: api}
}

// Warm finds the API's key ahead of the first song. It takes about a second.
func (s *SoundCloud) Warm(ctx context.Context) error {
	_, err := s.clientID(ctx, false)
	return err
}

// Lookup finds the song at a SoundCloud link and its audio.
func (s *SoundCloud) Lookup(ctx context.Context, link string) (songs.Song, error) {
	link, err := s.unshorten(ctx, link)
	if err != nil {
		return songs.Song{}, err
	}
	var t track
	if err := s.get(ctx, s.api+"/resolve", url.Values{"url": {link}}, &t); err != nil {
		return songs.Song{}, err
	}
	if t.Kind != "track" {
		return songs.Song{}, fmt.Errorf("soundcloud %s: %w", link, songs.ErrPlaylist)
	}
	if t.Policy == "BLOCK" {
		return songs.Song{}, fmt.Errorf("soundcloud %s: %w: blocked here", link, songs.ErrUnplayable)
	}
	tc, ok := t.bestAudio()
	if !ok {
		return songs.Song{}, fmt.Errorf("soundcloud %s: %w: only a preview", link, songs.ErrUnplayable)
	}
	var stream struct {
		URL string `json:"url"`
	}
	if err := s.get(ctx, tc.URL, url.Values{"track_authorization": {t.Authorization}}, &stream); err != nil {
		return songs.Song{}, err
	}

	artist := t.User.Username
	if t.Publisher.Artist != "" {
		artist = t.Publisher.Artist
	}
	artwork := t.Artwork
	if artwork == "" {
		artwork = t.User.Avatar
	}
	var expires time.Time
	if u, err := url.Parse(stream.URL); err == nil {
		if s, err := strconv.ParseInt(u.Query().Get("expires"), 10, 64); err == nil {
			expires = time.Unix(s, 0)
		}
	}
	return songs.Song{
		Link:   t.Permalink,
		Title:  t.Title,
		Artist: artist,
		Length: time.Duration(t.Duration) * time.Millisecond,
		// Artwork links end in -large.jpg, which is 100 px.
		Thumbnail: strings.Replace(artwork, "-large.", "-t500x500.", 1),
		Audio: songs.Audio{
			URL:      stream.URL,
			Header:   http.Header{"User-Agent": {browserAgent}},
			Playlist: tc.Format.Protocol == "hls",
			Expires:  expires,
		},
	}, nil
}

// track is the part of SoundCloud's answer about a song that's used.
type track struct {
	Kind          string `json:"kind"`
	Title         string `json:"title"`
	Permalink     string `json:"permalink_url"`
	Duration      int64  `json:"duration"` // in milliseconds
	Artwork       string `json:"artwork_url"`
	Policy        string `json:"policy"`
	Authorization string `json:"track_authorization"`
	User          struct {
		Username string `json:"username"`
		Avatar   string `json:"avatar_url"`
	} `json:"user"`
	Publisher struct {
		Artist string `json:"artist"`
	} `json:"publisher_metadata"`
	Media struct {
		Transcodings []transcoding `json:"transcodings"`
	} `json:"media"`
}

// transcoding is one way to get a song's audio.
type transcoding struct {
	URL     string `json:"url"`
	Snipped bool   `json:"snipped"` // a 30-second preview
	Format  struct {
		Protocol string `json:"protocol"` // "progressive" or "hls"
		MimeType string `json:"mime_type"`
	} `json:"format"`
}

// bestAudio picks a whole file if there is one, since it downloads in
// pieces, then a playlist of AAC or Opus pieces, then any other. Previews
// don't count.
func (t track) bestAudio() (transcoding, bool) {
	rank := func(tc transcoding) int {
		switch {
		case tc.Format.Protocol == "progressive":
			return 3
		case strings.Contains(tc.Format.MimeType, "mp4a"), strings.Contains(tc.Format.MimeType, "opus"):
			return 2
		}
		return 1
	}
	var best transcoding
	found := false
	for _, tc := range t.Media.Transcodings {
		if tc.Snipped || tc.URL == "" {
			continue
		}
		if !found || rank(tc) > rank(best) {
			best, found = tc, true
		}
	}
	return best, found
}

// unshorten turns a short on.soundcloud.com link into the song's own, which
// the API knows.
func (s *SoundCloud) unshorten(ctx context.Context, link string) (string, error) {
	if host(link) != "on.soundcloud.com" {
		return link, nil
	}
	long, err := followShort(ctx, s.http, link)
	if err != nil {
		return "", fmt.Errorf("soundcloud %w", err)
	}
	return long, nil
}

// get asks the API for target with the key, and decodes the answer into out.
// A refusal gets a fresh key once, since SoundCloud changes it now and then.
func (s *SoundCloud) get(ctx context.Context, target string, query url.Values, out any) error {
	for fresh := false; ; fresh = true {
		key, err := s.clientID(ctx, fresh)
		if err != nil {
			return err
		}
		q := url.Values{"client_id": {key}}
		for k, v := range query {
			q[k] = v
		}
		status, err := s.getJSON(ctx, target+"?"+q.Encode(), out)
		if (status == http.StatusUnauthorized || status == http.StatusForbidden) && !fresh {
			continue
		}
		if status == http.StatusNotFound {
			return fmt.Errorf("soundcloud: %w: not found, or private", songs.ErrUnplayable)
		}
		return err
	}
}

func (s *SoundCloud) getJSON(ctx context.Context, target string, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", browserAgent)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("soundcloud: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("soundcloud: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("soundcloud: decode: %w", err)
	}
	return resp.StatusCode, nil
}

var (
	// scriptTag finds the website's scripts in its page.
	scriptTag = regexp.MustCompile(`<script crossorigin src="([^"]+\.js)"`)
	// clientIDIn finds the key in one of them.
	clientIDIn = regexp.MustCompile(`client_id\s*[:=]\s*"?([a-zA-Z0-9]{32})`)
)

// clientID returns the key SoundCloud's API wants, finding it in the
// website's scripts the first time, and again when fresh is set.
func (s *SoundCloud) clientID(ctx context.Context, fresh bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key != "" && !fresh {
		return s.key, nil
	}
	page, err := s.fetch(ctx, s.site+"/")
	if err != nil {
		return "", err
	}
	scripts := scriptTag.FindAllSubmatch(page, -1)
	// The key is in one of the last scripts, so they're searched from the end.
	for i := len(scripts) - 1; i >= 0; i-- {
		js, err := s.fetch(ctx, string(scripts[i][1]))
		if err != nil {
			return "", err
		}
		if m := clientIDIn.FindSubmatch(js); m != nil {
			s.key = string(m[1])
			return s.key, nil
		}
	}
	return "", errors.New("soundcloud: no API key in the website's scripts")
}

func (s *SoundCloud) fetch(ctx context.Context, target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserAgent)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("soundcloud: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("soundcloud %s: %s", target, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxScript))
}
