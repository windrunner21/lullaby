package lullaby

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/windrunner21/lullaby/internal/redact"
	"github.com/windrunner21/lullaby/songs"
)

// Radio returns YouTube Music's radio for a song: the songs its app plays
// after it, about 50, for an app that picks what plays next by itself, like
// when the queue runs out. Queue a pick with Request.Autoplay, so songs asked
// for go ahead of it.
//
// It needs the song's YouTube video, which songs from YouTube and YouTube
// Music have, and so do Spotify's and the other catalogs', since they play
// from one. For SoundCloud's and other sites' songs it returns ErrNoRadio. A
// song's radio is remembered for as long as searches are (Config.SearchMemory),
// and one already on its way is waited for rather than asked again. ctx only
// bounds the wait. Its songs are remembered like suggestions, so one played
// soon after plays with YouTube Music's title, artist and cover.
func (e *Engine) Radio(ctx context.Context, song songs.Song) ([]songs.Song, error) {
	id := song.ID
	if id == "" {
		id, _ = songs.YouTubeID(song.Link)
	}
	if id == "" {
		return nil, ErrNoRadio
	}
	found, err := e.radio(ctx, id)
	if err != nil {
		return nil, err
	}
	e.remember(found)
	// A copy, since the found songs stay in memory for others.
	out := make([]songs.Song, len(found))
	for i, s := range found {
		out[i] = owned(s)
	}
	return out, nil
}

// radio finds the radio for a YouTube video, or reuses one found within
// Config.SearchMemory, without the video itself. A radio for the same video
// already on its way is waited for. It goes on if ctx ends, for whoever asks
// next.
func (e *Engine) radio(ctx context.Context, id string) ([]songs.Song, error) {
	e.mu.Lock()
	if e.radios == nil {
		e.radios, e.radioing = make(map[string]search), make(map[string]*searching)
	}
	if r, ok := e.radios[id]; ok && time.Since(r.at) <= cmp.Or(e.searchFor, searchedFor) {
		e.mu.Unlock()
		return r.found, nil
	}
	w, ok := e.radioing[id]
	if !ok {
		w = &searching{done: make(chan struct{})}
		e.radioing[id] = w
		go func() {
			ctx, cancel := context.WithTimeout(e.life(), searchTimeout)
			defer cancel()
			found, err := e.yt.Radio(ctx, id)
			if err != nil {
				e.logger().Debug("radio failed", slog.String("video", id), redact.Attr(err))
			}
			// The song itself comes first.
			w.found, w.err = slices.DeleteFunc(found, func(s songs.Song) bool { return s.ID == id }), err
			e.mu.Lock()
			delete(e.radioing, id)
			if err == nil && len(w.found) > 0 {
				e.keepRadioLocked(id, w.found)
			}
			e.mu.Unlock()
			close(w.done)
		}()
	}
	e.mu.Unlock()
	select {
	case <-w.done:
		return w.found, w.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// keepRadioLocked remembers a video's radio. When there are too many, the
// oldest is forgotten. e.mu must be held.
func (e *Engine) keepRadioLocked(id string, found []songs.Song) {
	if _, ok := e.radios[id]; !ok && len(e.radios) >= maxRadios {
		var oldest string
		for k, r := range e.radios {
			if oldest == "" || r.at.Before(e.radios[oldest].at) {
				oldest = k
			}
		}
		delete(e.radios, oldest)
	}
	e.radios[id] = search{found: found, at: time.Now()}
}
