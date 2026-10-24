package lullaby

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby/internal/redact"
	"github.com/windrunner21/lullaby/songs"
)

// search is what YouTube Music found for some words.
type search struct {
	found []songs.Song
	at    time.Time // when it was found, which reusing it doesn't change
	used  time.Time // when it was last reused, so the least used goes first
}

// searching is a search on its way, which everyone asking for the same words
// waits for.
type searching struct {
	done  chan struct{}
	found []songs.Song
	err   error
}

// SuggestRequest is someone typing a song's name, for SuggestFor.
type SuggestRequest struct {
	// GuildID and UserID are who's typing, so their keystrokes replace each
	// other's, and nobody else's.
	GuildID, UserID snowflake.ID
	// Query is what they've typed so far.
	Query string
}

// typist is who's typing, from a SuggestRequest.
type typist struct {
	guild, user snowflake.ID
}

// typing is a typist's last keystroke, and the early lookup waiting on it.
type typing struct {
	keystroke uint64      // counts their keystrokes, so an old one's search can't win over a newer one
	at        time.Time   // when it came
	timer     *time.Timer // the early lookup, waiting for them to stop
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
// is remembered, since picking a suggestion only sends its link, and once
// they stop typing, the first song is looked up, so it plays at once when
// it's picked or Enter is pressed.
//
// Suggest is SuggestFor without saying who's typing, so everyone's
// keystrokes replace each other's early lookups.
func (e *Engine) Suggest(ctx context.Context, query string) []songs.Song {
	return e.SuggestFor(ctx, SuggestRequest{Query: query})
}

// SuggestFor is Suggest for someone in particular, so two people typing at
// once each get the first song they're shown looked up early, as the
// early lookups' limits allow.
func (e *Engine) SuggestFor(ctx context.Context, r SuggestRequest) []songs.Song {
	who := typist{r.GuildID, r.UserID}
	keystroke := e.typed(who)
	query := r.Query
	query = strings.TrimSpace(query)
	if isLink(query) {
		e.settle(who, keystroke, query, 0) // they've pasted it: no pause to wait for
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
	if len(found) > 0 {
		e.settle(who, keystroke, found[0].Link, settleAfter)
	}
	// A copy, since the found songs stay in memory for others.
	out := make([]songs.Song, len(found))
	for i, s := range found {
		out[i] = owned(s)
	}
	return out
}

// remember keeps suggestions, and a radio's songs, for a while. When there
// are too many, old ones are forgotten first, then the ones remembered
// longest ago, a tenth more than needed so this doesn't happen every time:
// a radio's 49 songs push out the oldest choices, not all of them.
func (e *Engine) remember(found []songs.Song) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for _, s := range found {
		e.suggested[s.Link] = suggestion{song: s, at: now}
	}
	if len(e.suggested) <= maxSuggested {
		return
	}
	for link, s := range e.suggested {
		if now.Sub(s.at) > suggestedFor {
			delete(e.suggested, link)
		}
	}
	if over := len(e.suggested) - maxSuggested; over > 0 {
		links := slices.SortedFunc(maps.Keys(e.suggested), func(a, b string) int {
			return e.suggested[a].at.Compare(e.suggested[b].at)
		})
		for _, link := range links[:min(over+maxSuggested/10, len(links))] {
			delete(e.suggested, link)
		}
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
// for the same words found within Config.SearchMemory. A search for the
// same words already on its way, like Suggest's when the song is asked for,
// is waited for rather than asked again. It goes on if ctx ends, so its
// answer is there for the next keystroke.
func (e *Engine) search(ctx context.Context, query string) ([]songs.Song, error) {
	key := searchKey(query)
	e.mu.Lock()
	if found, ok := e.recallSearchLocked(key); ok {
		e.mu.Unlock()
		return found, nil
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

// typed notes a keystroke by who, and stops the early lookup waiting on
// their last one: whatever they typed since, words, a link or nothing,
// replaces it. It returns the keystroke's number for settle.
func (e *Engine) typed(who typist) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.typing == nil {
		e.typing = make(map[typist]*typing)
	}
	t := e.typing[who]
	if t == nil {
		if len(e.typing) >= maxTypists {
			for k, old := range e.typing {
				if old.timer == nil && time.Since(old.at) > typistIdle {
					delete(e.typing, k)
				}
			}
		}
		if len(e.typing) >= maxTypists {
			return 0 // too many typing at once: no early lookup for this one
		}
		t = &typing{}
		e.typing[who] = t
	}
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.keystroke++
	t.at = time.Now()
	return t.keystroke
}

// settle looks up link, the first song suggested for who's keystroke, or a
// link they pasted, once pause has passed since the keystroke came: at once
// if its search took that long. Their next keystroke replaces it, so each
// pause costs one lookup at most, and a slow search for an old keystroke
// can't replace a newer one's. A song already remembered asks nobody, and
// the others are bounded by lookAheadLocked.
func (e *Engine) settle(who typist, keystroke uint64, link string, pause time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.typing[who]
	if t == nil || t.keystroke != keystroke {
		return // they've typed on, or aren't tracked
	}
	t.timer = time.AfterFunc(pause-time.Since(t.at), func() {
		e.mu.Lock()
		current := e.typing[who] == t && t.keystroke == keystroke
		if current {
			t.timer = nil
		}
		e.mu.Unlock()
		if !current || e.over() || !e.finder.Fast(link) || e.finder.Knows(link) {
			return
		}
		e.mu.Lock()
		ahead := e.lookAheadLocked()
		e.mu.Unlock()
		if !ahead {
			return
		}
		e.finder.FindAhead(e.life(), link)
		e.mu.Lock()
		e.lookingAhead--
		e.mu.Unlock()
	})
}

// lookAheadLocked reports whether an early lookup may ask YouTube, and
// counts it if so: at most maxLookingAhead at once, maxLookAhead in an hour
// and maxLookAheadDay in a day. Whoever it says yes to decrements
// lookingAhead when done. e.mu must be held.
func (e *Engine) lookAheadLocked() bool {
	recent, lastHour := e.lookedAhead[:0], 0
	for _, at := range e.lookedAhead {
		if time.Since(at) < 24*time.Hour {
			recent = append(recent, at)
		}
		if time.Since(at) < time.Hour {
			lastHour++
		}
	}
	e.lookedAhead = recent
	if e.lookingAhead >= maxLookingAhead || lastHour >= maxLookAhead || len(recent) >= maxLookAheadDay {
		return false
	}
	e.lookingAhead++
	e.lookedAhead = append(e.lookedAhead, time.Now())
	return true
}

// narrowed suggests songs at once from an earlier search for the start of
// the same words: what "sza s" found, kept to the songs with every word of
// "sza sno" in their title or artist. It needs minNarrowed songs to count.
func (e *Engine) narrowed(key string) ([]songs.Song, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var from string
	for k, s := range e.searched {
		// Narrowed choices stand in for a search of the words typed, so
		// they come from recent searches only, whatever SearchMemory says.
		if len(k) > len(from) && strings.HasPrefix(key, k) && e.freshLocked(s) && time.Since(s.at) <= searchedFor {
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
	if len(out) < minNarrowed {
		return out, false
	}
	s := e.searched[from]
	s.used = time.Now() // it's of use, so it's kept
	e.searched[from] = s
	return out, true
}

// recallSearch returns what a search for key found, if it's recent.
func (e *Engine) recallSearch(key string) ([]songs.Song, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.recallSearchLocked(key)
}

// recallSearchLocked is recallSearch for whoever holds e.mu.
func (e *Engine) recallSearchLocked(key string) ([]songs.Song, bool) {
	s, ok := e.searched[key]
	if !ok || !e.freshLocked(s) {
		return nil, false
	}
	s.used = time.Now()
	e.searched[key] = s
	return s.found, true
}

// freshLocked reports whether a search is recent enough to reuse: within
// Config.SearchMemory, or a minute if it found nothing. e.mu must be held.
func (e *Engine) freshLocked(s search) bool {
	keep := cmp.Or(e.searchFor, searchedFor)
	if len(s.found) == 0 {
		keep = min(keep, emptyFor)
	}
	return time.Since(s.at) <= keep
}

// keepSearchLocked remembers what a search found. When there are too many,
// old searches are forgotten first, then the one reused longest ago. e.mu
// must be held.
func (e *Engine) keepSearchLocked(key string, found []songs.Song) {
	if _, ok := e.searched[key]; !ok && len(e.searched) >= maxSearched {
		var least string
		for k, s := range e.searched {
			switch {
			case !e.freshLocked(s):
				delete(e.searched, k)
			case least == "" || s.used.Before(e.searched[least].used):
				least = k
			}
		}
		if len(e.searched) >= maxSearched {
			delete(e.searched, least)
		}
	}
	now := time.Now()
	e.searched[key] = search{found: found, at: now, used: now}
}

// isLink reports whether a song was asked for by link rather than by name.
func isLink(s string) bool {
	if _, ok := songs.YouTubeID(s); ok {
		return true
	}
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}
