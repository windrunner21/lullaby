package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// SpotifySite is Spotify's web player. Its embed pages, the player other
// websites show, name a song without the API key Spotify's API wants.
const SpotifySite = "https://open.spotify.com"

var (
	// nextData is the JSON an embed page draws itself from.
	nextData = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)
	// spotifyID is what a Spotify ID looks like.
	spotifyID = regexp.MustCompile(`^[0-9A-Za-z]{22}$`)
)

// Spotify reads songs from Spotify's embed pages. Spotify gives bots no
// audio, so Finder plays them from YouTube Music.
type Spotify struct {
	http *http.Client
	site string
}

// NewSpotify returns a Spotify that reads the embed pages at site, usually
// SpotifySite.
func NewSpotify(client *http.Client, site string) *Spotify {
	return &Spotify{http: client, site: site}
}

// Owns reports whether link is Spotify's, short share links included.
func (s *Spotify) Owns(link string) bool {
	switch host(link) {
	case "open.spotify.com", "play.spotify.com", "spotify.link", "spotify.app.link":
		return true
	}
	return false
}

// Key names a song by its ID, from a link like
// open.spotify.com/track/ID?si=CODE.
func (s *Spotify) Key(link string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(link))
	if h := host(link); err != nil || (h != "open.spotify.com" && h != "play.spotify.com") || strings.HasPrefix(u.Path, "/s/") {
		return "", false
	}
	kind, id := spotifyPath(u.Path)
	if kind != "track" || !spotifyID.MatchString(id) {
		return "", false
	}
	return "spotify:" + id, true
}

// Track finds the song at a Spotify link.
func (s *Spotify) Track(ctx context.Context, link string) (Track, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return Track{}, err
	}
	if h := host(link); (h != "open.spotify.com" && h != "play.spotify.com") || strings.HasPrefix(u.Path, "/s/") {
		long, err := followShort(ctx, s.http, link)
		if err != nil {
			return Track{}, fmt.Errorf("spotify: %w", err)
		}
		if u, err = url.Parse(long); err != nil || host(long) != "open.spotify.com" {
			return Track{}, fmt.Errorf("spotify: a short link led to %q", long)
		}
	}

	kind, id := spotifyPath(u.Path)
	switch {
	case kind == "album" || kind == "playlist":
		return Track{}, fmt.Errorf("spotify %s: %w", link, songs.ErrPlaylist)
	case kind != "track" || !spotifyID.MatchString(id):
		return Track{}, fmt.Errorf("spotify %s: %w: not a song", link, songs.ErrUnplayable)
	}

	page, status, err := getPage(ctx, s.http, s.site+"/embed/track/"+id)
	switch {
	case err != nil:
		return Track{}, fmt.Errorf("spotify: %w", err)
	case status == http.StatusNotFound:
		return Track{}, fmt.Errorf("spotify %s: %w: not found", link, songs.ErrUnplayable)
	case status != http.StatusOK:
		return Track{}, fmt.Errorf("spotify: %d", status)
	}
	m := nextData.FindSubmatch(page)
	if m == nil {
		return Track{}, errors.New("spotify: no song in the embed page")
	}
	var data struct {
		Props struct {
			PageProps struct {
				State struct {
					Data struct {
						Entity spotifyTrack `json:"entity"`
					} `json:"data"`
				} `json:"state"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(m[1], &data); err != nil {
		return Track{}, fmt.Errorf("spotify: decode: %w", err)
	}
	e := data.Props.PageProps.State.Data.Entity
	if e.Type != "track" || e.Name == "" {
		return Track{}, fmt.Errorf("spotify %s: %w: not found", link, songs.ErrUnplayable)
	}
	t := Track{
		Link:   SpotifySite + "/track/" + id,
		Title:  e.Name,
		Length: time.Duration(e.Duration) * time.Millisecond,
	}
	for _, a := range e.Artists {
		t.Artists = append(t.Artists, a.Name)
	}
	widest := 0
	for _, img := range e.Visual.Images {
		if img.Width > widest {
			t.Cover, widest = img.URL, img.Width
		}
	}
	return t, nil
}

// spotifyPath reads what a Spotify link's path points to, like "track" and
// its ID, from paths like /track/ID, /intl-de/track/ID and /embed/track/ID.
func spotifyPath(path string) (kind, id string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for len(parts) > 0 && (strings.HasPrefix(parts[0], "intl-") || parts[0] == "embed") {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// spotifyTrack is the part of an embed page's song that's used.
type spotifyTrack struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Duration int64  `json:"duration"` // in milliseconds
	Artists  []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Visual struct {
		Images []struct {
			URL   string `json:"url"`
			Width int    `json:"maxWidth"`
		} `json:"image"`
	} `json:"visualIdentity"`
}
