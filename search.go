package lullaby

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/windrunner21/lullaby/internal/redact"
	"github.com/windrunner21/lullaby/songs"
)

// search is what YouTube Music found for some words.
type search struct {
	found []songs.Song
	at    time.Time
}

// searching is a search on its way, which everyone asking for the same words
// waits for.
type searching struct {
	done  chan struct{}
	found []songs.Song
	err   error
}

// suggestion is a song Suggest suggested. Picking one only sends its link,
// so it's remembered for YouTube Music's title, artist and cover.
type suggestion struct {
	song songs.Song
	at   time.Time
}

// find looks a song up from a link, or searches YouTube Music for the words
// and takes the first song.
func (e *Engine) find(ctx context.Context, query string) (songs.Song, error) {
	if isLink(query) {
		song, err := e.finder.Find(ctx, query)
		if s, ok := e.recall(query); ok && err == nil {
			song = withMusicInfo(song, s)
		}
		return song, err
	}
	// Usually the same words Suggest searched a moment before.
	found, err := e.search(ctx, query)
	if err != nil {
		return songs.Song{}, err
	}
	if len(found) == 0 {
		return songs.Song{}, ErrNotFound
	}
	song, err := e.finder.Find(ctx, found[0].Link)
	if err != nil {
		return song, err
	}
	return withMusicInfo(song, found[0]), nil
}

// withMusicInfo gives a song the title, artist and cover YouTube Music has
// for it, which suit it better than its video's: "Snooze" by SZA with the
// album cover, rather than "SZA - Snooze (Official Video)".
func withMusicInfo(song, music songs.Song) songs.Song {
	song.Title, song.Artist = music.Title, music.Artist
	if music.Thumbnail != "" {
		song.Thumbnail = cover(music.Thumbnail)
	}
	return song
}

// Suggest finds songs for someone typing a song's name, as fast as it can:
// from memory, from an earlier search narrowed down to the words typed since,
// or from YouTube Music. A link gets no suggestions; it's looked up while
// they're still choosing instead, so Play finds it ready. What's suggested
// is remembered, since picking a suggestion only sends its link.
func (e *Engine) Suggest(ctx context.Context, query string) []songs.Song {
	query = strings.TrimSpace(query)
	if isLink(query) {
		go e.finder.FindAhead(e.life(), query)
		return nil
	}
	if utf8.RuneCountInString(query) < 2 {
		return nil
	}

	asked, from := time.Now(), "memory"
	key := searchKey(query)
	found, ok := e.recallSearch(key)
	if !ok {
		if found, ok = e.narrowed(key); ok {
			from = "an earlier search"
			go e.search(e.life(), query) // for the next keystroke, or Enter
		}
	}
	if !ok {
		from = "youtube music"
		var err error
		if found, err = e.search(ctx, query); err != nil {
			e.logger().Debug("song suggestions failed", redact.Attr(err))
		}
	}
	e.logger().Debug("song suggestions", slog.String("words", query), slog.String("from", from), slog.Duration("took", time.Since(asked)))

	found = found[:min(len(found), MaxSuggestions)]
	e.remember(found)
	// A copy, since the found songs stay in memory for others.
	out := make([]songs.Song, len(found))
	for i, s := range found {
		out[i] = owned(s)
	}
	return out
}

// remember keeps suggestions for a while, and forgets old ones.
func (e *Engine) remember(found []songs.Song) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if len(e.suggested)+len(found) > maxSuggested {
		for link, s := range e.suggested {
			if now.Sub(s.at) > suggestedFor {
				delete(e.suggested, link)
			}
		}
	}
	if len(e.suggested)+len(found) > maxSuggested {
		clear(e.suggested)
	}
	for _, s := range found {
		e.suggested[s.Link] = suggestion{song: s, at: now}
	}
}

// recall returns the suggestion a picked link came from, if it's recent.
func (e *Engine) recall(link string) (songs.Song, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.suggested[link]
	if !ok || time.Since(s.at) > suggestedFor {
		return songs.Song{}, false
	}
	return s.song, true
}

// searchKey is how searched words are remembered: in lower case, with single
// spaces.
func searchKey(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}

// search finds songs for some words on YouTube Music, or reuses what a search
// for the same words found within searchedFor. A search for the same words
// already on its way, like Suggest's when the song is asked for, is waited
// for rather than asked again. It goes on if ctx ends, so its answer is
// there for the next keystroke.
func (e *Engine) search(ctx context.Context, query string) ([]songs.Song, error) {
	key := searchKey(query)
	e.mu.Lock()
	if s, ok := e.searched[key]; ok && time.Since(s.at) <= searchedFor {
		e.mu.Unlock()
		return s.found, nil
	}
	w, ok := e.searching[key]
	if !ok {
		w = &searching{done: make(chan struct{})}
		e.searching[key] = w
		go func() {
			ctx, cancel := context.WithTimeout(e.life(), searchTimeout)
			defer cancel()
			w.found, w.err = e.yt.Search(ctx, query)
			if w.err != nil {
				e.logger().Debug("search failed", slog.String("words", query), redact.Attr(w.err))
			}
			e.mu.Lock()
			delete(e.searching, key)
			if w.err == nil {
				e.keepSearchLocked(key, w.found)
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

// narrowed suggests songs at once from an earlier search for the start of
// the same words: what "sza s" found, kept to the songs with every word of
// "sza sno" in their title or artist. It needs minNarrowed songs to count.
func (e *Engine) narrowed(key string) ([]songs.Song, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var from string
	for k, s := range e.searched {
		if len(k) > len(from) && strings.HasPrefix(key, k) && time.Since(s.at) < searchedFor {
			from = k
		}
	}
	if from == "" {
		return nil, false
	}
	words := strings.Fields(key)
	var out []songs.Song
	for _, s := range e.searched[from].found {
		about := strings.ToLower(s.Title + " " + s.Artist)
		matches := true
		for _, w := range words {
			if !strings.Contains(about, w) {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, s)
		}
	}
	return out, len(out) >= minNarrowed
}

// recallSearch returns what a search for key found, if it's recent.
func (e *Engine) recallSearch(key string) ([]songs.Song, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.searched[key]
	if !ok || time.Since(s.at) > searchedFor {
		return nil, false
	}
	return s.found, true
}

// keepSearchLocked remembers what a search found, and forgets old searches.
// e.mu must be held.
func (e *Engine) keepSearchLocked(key string, found []songs.Song) {
	if len(e.searched) >= maxSearched {
		for k, s := range e.searched {
			if time.Since(s.at) > searchedFor {
				delete(e.searched, k)
			}
		}
	}
	if len(e.searched) >= maxSearched {
		clear(e.searched)
	}
	e.searched[key] = search{found: found, at: time.Now()}
}

// isLink reports whether a song was asked for by link rather than by name.
func isLink(s string) bool {
	if _, ok := songs.YouTubeID(s); ok {
		return true
	}
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}
