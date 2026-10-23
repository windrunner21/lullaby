package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// AppleAPI is Apple's iTunes Search API, which knows Apple Music's songs and
// needs no key.
const AppleAPI = "https://itunes.apple.com"

var (
	// appleCountry is a country code at the start of an Apple Music path.
	appleCountry = regexp.MustCompile(`^[a-z]{2}$`)
	// appleID is what an Apple Music ID looks like.
	appleID = regexp.MustCompile(`^[0-9]+$`)
)

// AppleMusic reads songs from Apple's iTunes Search API. Apple Music gives
// bots no audio, so Finder plays them from YouTube Music.
type AppleMusic struct {
	http *http.Client
	api  string
}

// NewAppleMusic returns an AppleMusic that asks the API at api, usually
// AppleAPI.
func NewAppleMusic(client *http.Client, api string) *AppleMusic {
	return &AppleMusic{http: client, api: api}
}

// Owns reports whether link is Apple Music's.
func (a *AppleMusic) Owns(link string) bool {
	switch host(link) {
	case "music.apple.com", "geo.music.apple.com", "itunes.apple.com":
		return true
	}
	return false
}

// Track finds the song at an Apple Music link: an album's link with ?i= for
// one of its songs, or a song's own link.
func (a *AppleMusic) Track(ctx context.Context, link string) (Track, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return Track{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	country := "us"
	if len(parts) > 0 && appleCountry.MatchString(parts[0]) {
		country, parts = parts[0], parts[1:]
	}
	id := u.Query().Get("i")
	switch {
	case len(parts) == 0:
	case id == "" && (parts[0] == "song" || parts[0] == "music-video"):
		id = parts[len(parts)-1]
	case id == "" && (parts[0] == "album" || parts[0] == "playlist"):
		return Track{}, fmt.Errorf("apple music %s: %w", link, songs.ErrPlaylist)
	}
	if !appleID.MatchString(id) {
		return Track{}, fmt.Errorf("apple music %s: %w: not a song", link, songs.ErrUnplayable)
	}

	q := url.Values{"id": {id}, "country": {country}, "entity": {"song"}}
	body, status, err := getPage(ctx, a.http, a.api+"/lookup?"+q.Encode())
	if err != nil {
		return Track{}, fmt.Errorf("apple music: %w", err)
	}
	if status != http.StatusOK {
		return Track{}, fmt.Errorf("apple music: %d", status)
	}
	var found struct {
		Results []struct {
			Wrapper string `json:"wrapperType"`
			Title   string `json:"trackName"`
			Artist  string `json:"artistName"`
			Length  int64  `json:"trackTimeMillis"`
			Artwork string `json:"artworkUrl100"`
			Link    string `json:"trackViewUrl"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &found); err != nil {
		return Track{}, fmt.Errorf("apple music: decode: %w", err)
	}
	for _, r := range found.Results {
		if r.Wrapper != "track" || r.Title == "" {
			continue
		}
		t := Track{
			Link:    link,
			Title:   r.Title,
			Artists: []string{r.Artist},
			Length:  time.Duration(r.Length) * time.Millisecond,
			// Artwork links end in 100x100bb.jpg; Apple makes any size asked for.
			Cover: strings.Replace(r.Artwork, "/100x100bb.", "/600x600bb.", 1),
		}
		if v, err := url.Parse(r.Link); err == nil && r.Link != "" {
			q := v.Query()
			q.Del("uo") // Apple's tracking
			v.RawQuery = q.Encode()
			t.Link = v.String()
		}
		return t, nil
	}
	return Track{}, fmt.Errorf("apple music %s: %w: not found", link, songs.ErrUnplayable)
}
