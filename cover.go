package lullaby

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// coverSize matches the size YouTube Music puts in its cover links.
var coverSize = regexp.MustCompile(`=w\d+-h\d+`)

// cover asks for a bigger copy of a YouTube Music cover than the 120 px one
// search results link to.
func cover(link string) string {
	return coverSize.ReplaceAllString(link, "=w544-h544")
}

// thumbnails is where YouTube keeps its videos' thumbnails.
var thumbnails = "https://i.ytimg.com/vi/"

// coverAhead starts finding the cover for a YouTube video, whose own
// thumbnail is 4:3 with black bars, so it's ready by the time the song plays.
// It returns nil when the song's own cover will do.
func (e *Engine) coverAhead(song songs.Song) func() string {
	if song.ID == "" || !strings.HasPrefix(song.Thumbnail, thumbnails) {
		return nil
	}
	found := make(chan string, 1)
	go func() { found <- e.videoCover(e.life(), song.ID) }()
	return sync.OnceValue(func() string { return <-found })
}

// videoCover finds the best picture for a YouTube video that isn't on
// YouTube Music: its HD thumbnail, which most videos have, or else the small
// one that all do. Both are 16:9 without the black bars of the 4:3 ones.
func (e *Engine) videoCover(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	hd := thumbnails + id + "/maxresdefault.jpg"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, hd, nil)
	if err == nil {
		if resp, err := e.http.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return hd
			}
		}
	}
	return thumbnails + id + "/mqdefault.jpg"
}
