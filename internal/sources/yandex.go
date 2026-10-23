package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// YandexAPI is Yandex Music's API, which names songs without signing in.
const YandexAPI = "https://api.music.yandex.net"

// YandexMusic reads songs from Yandex Music's API. Without signing in, it
// only gives 30-second previews, so Finder plays them from YouTube Music.
type YandexMusic struct {
	http *http.Client
	api  string
}

// NewYandexMusic returns a YandexMusic that asks the API at api, usually
// YandexAPI.
func NewYandexMusic(client *http.Client, api string) *YandexMusic {
	return &YandexMusic{http: client, api: api}
}

// Owns reports whether link is Yandex Music's, on any of its domains, like
// music.yandex.ru or music.yandex.com.
func (y *YandexMusic) Owns(link string) bool {
	h := host(link)
	return strings.HasPrefix(h, "music.yandex.") && !strings.Contains(strings.TrimPrefix(h, "music.yandex."), ".")
}

// Track finds the song at a Yandex Music link, like /album/1/track/2 or
// /track/2.
func (y *YandexMusic) Track(ctx context.Context, link string) (Track, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return Track{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	var id string
	switch {
	case len(parts) == 4 && parts[0] == "album" && parts[2] == "track":
		id = parts[3]
	case len(parts) == 2 && parts[0] == "track":
		id = parts[1]
	case len(parts) == 2 && parts[0] == "album", len(parts) >= 2 && parts[0] == "playlists",
		len(parts) == 4 && parts[0] == "users" && parts[2] == "playlists":
		return Track{}, fmt.Errorf("yandex music %s: %w", link, songs.ErrPlaylist)
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return Track{}, fmt.Errorf("yandex music %s: %w: not a song", link, songs.ErrUnplayable)
	}

	body, status, err := getPage(ctx, y.http, y.api+"/tracks/"+id)
	switch {
	case err != nil:
		return Track{}, fmt.Errorf("yandex music: %w", err)
	case status == http.StatusBadRequest || status == http.StatusNotFound:
		return Track{}, fmt.Errorf("yandex music %s: %w: not found", link, songs.ErrUnplayable)
	case status != http.StatusOK:
		return Track{}, fmt.Errorf("yandex music: %d", status)
	}
	var found struct {
		Result []struct {
			Title    string `json:"title"`
			Version  string `json:"version"` // like "Acoustic", if it isn't the original
			Duration int64  `json:"durationMs"`
			Cover    string `json:"coverUri"`
			Artists  []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &found); err != nil {
		return Track{}, fmt.Errorf("yandex music: decode: %w", err)
	}
	if len(found.Result) == 0 || found.Result[0].Title == "" {
		return Track{}, fmt.Errorf("yandex music %s: %w: not found", link, songs.ErrUnplayable)
	}
	r := found.Result[0]
	t := Track{
		Link:   "https://" + u.Host + u.Path,
		Title:  r.Title,
		Length: time.Duration(r.Duration) * time.Millisecond,
	}
	if r.Version != "" {
		t.Title += " (" + r.Version + ")"
	}
	for _, a := range r.Artists {
		t.Artists = append(t.Artists, a.Name)
	}
	// Cover links end in %%, for the size wanted.
	if r.Cover != "" {
		t.Cover = "https://" + strings.Replace(r.Cover, "%%", "600x600", 1)
	}
	return t, nil
}
