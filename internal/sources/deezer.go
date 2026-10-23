package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// DeezerAPI is Deezer's public API, which needs no key for songs.
const DeezerAPI = "https://api.deezer.com"

// deezerLang is a language code at the start of a Deezer path.
var deezerLang = regexp.MustCompile(`^[a-z]{2}$`)

// Deezer reads songs from Deezer's API. Deezer only gives bots 30-second
// previews, and its full songs are encrypted, so Finder plays them from
// YouTube Music.
type Deezer struct {
	http *http.Client
	api  string
}

// NewDeezer returns a Deezer that asks the API at api, usually DeezerAPI.
func NewDeezer(client *http.Client, api string) *Deezer {
	return &Deezer{http: client, api: api}
}

// Owns reports whether link is Deezer's, short share links included.
func (d *Deezer) Owns(link string) bool {
	switch host(link) {
	case "deezer.com", "link.deezer.com", "deezer.page.link":
		return true
	}
	return false
}

// Key names a song by its ID, from a link like deezer.com/en/track/ID.
func (d *Deezer) Key(link string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || host(link) != "deezer.com" {
		return "", false
	}
	if id, _ := deezerSong(u); id != "" {
		return "deezer:" + id, true
	}
	return "", false
}

// deezerSong reads a song's ID from a deezer.com link, with or without its
// language. list is set for an album or a playlist.
func deezerSong(u *url.URL) (id string, list bool) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && deezerLang.MatchString(parts[0]) {
		parts = parts[1:]
	}
	switch {
	case len(parts) == 2 && (parts[0] == "album" || parts[0] == "playlist"):
		return "", true
	case len(parts) != 2 || parts[0] != "track":
		return "", false
	}
	if _, err := strconv.ParseUint(parts[1], 10, 64); err != nil {
		return "", false
	}
	return parts[1], false
}

// Track finds the song at a Deezer link.
func (d *Deezer) Track(ctx context.Context, link string) (Track, error) {
	// A share link can lead to another before the song's own. Each is
	// followed only while it's still Deezer's.
	for hops := 0; host(link) != "deezer.com"; hops++ {
		if hops == 3 || !d.Owns(link) {
			return Track{}, fmt.Errorf("deezer: a short link led to %q", link)
		}
		long, err := followShort(ctx, d.http, link)
		if err != nil {
			return Track{}, fmt.Errorf("deezer: %w", err)
		}
		link = long
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return Track{}, fmt.Errorf("deezer: %w", err)
	}
	id, list := deezerSong(u)
	switch {
	case list:
		return Track{}, fmt.Errorf("deezer %s: %w", link, songs.ErrPlaylist)
	case id == "":
		return Track{}, fmt.Errorf("deezer %s: %w: not a song", link, songs.ErrUnplayable)
	}

	body, status, err := getPage(ctx, d.http, d.api+"/track/"+id)
	if err != nil {
		return Track{}, fmt.Errorf("deezer: %w", err)
	}
	if status != http.StatusOK {
		return Track{}, fmt.Errorf("deezer: %d", status)
	}
	var track struct {
		// Deezer answers a song it doesn't have with an error, and 200 OK.
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Title        string `json:"title"`
		Link         string `json:"link"`
		Duration     int64  `json:"duration"` // in seconds
		Contributors []struct {
			Name string `json:"name"`
		} `json:"contributors"`
		Artist struct {
			Name string `json:"name"`
		} `json:"artist"`
		Album struct {
			Cover string `json:"cover_xl"`
		} `json:"album"`
	}
	if err := json.Unmarshal(body, &track); err != nil {
		return Track{}, fmt.Errorf("deezer: decode: %w", err)
	}
	if track.Error != nil || track.Title == "" {
		return Track{}, fmt.Errorf("deezer %s: %w: not found", link, songs.ErrUnplayable)
	}
	t := Track{
		Link:   track.Link,
		Title:  track.Title,
		Length: time.Duration(track.Duration) * time.Second,
		Cover:  track.Album.Cover,
	}
	for _, c := range track.Contributors {
		t.Artists = append(t.Artists, c.Name)
	}
	if len(t.Artists) == 0 && track.Artist.Name != "" {
		t.Artists = []string{track.Artist.Name}
	}
	if t.Link == "" {
		t.Link = "https://www.deezer.com/track/" + id
	}
	return t, nil
}
