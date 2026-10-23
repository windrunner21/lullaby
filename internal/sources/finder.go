// Package sources finds songs and their audio for lullaby: YouTube asked as
// its iPhone app does, YouTube Music's search, SoundCloud, yt-dlp as the
// backup, and Spotify, Apple Music, Deezer and Yandex Music songs matched on
// YouTube Music by name.
package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/windrunner21/lullaby/internal/netguard"
	"github.com/windrunner21/lullaby/internal/redact"
	"github.com/windrunner21/lullaby/songs"
)

const (
	// keepFor is how much of a link's life is left when Finder stops reusing
	// it, so a song found from memory still plays to its end.
	keepFor = time.Hour
	// maxKept is how many found songs Finder remembers.
	maxKept = 500
	// botCheckFor is how long YouTube links go straight to the spare account
	// after a bot check, before the fast path is tried again.
	botCheckFor = 15 * time.Minute
)

// searcher searches YouTube Music.
type searcher interface {
	Search(ctx context.Context, query string) ([]songs.Song, error)
}

// lookup finds the song at a link, or with an ID.
type lookup interface {
	Lookup(ctx context.Context, link string) (songs.Song, error)
}

// Finder looks songs up the fast way first, on YouTube and SoundCloud, and
// falls back to yt-dlp, which also knows every other site.
type Finder struct {
	// Logger is where it logs. Nil is slog.Default().
	Logger *slog.Logger
	// GuardLinks refuses other sites' links on the bot host's own network
	// before yt-dlp fetches them.
	GuardLinks bool
	// Search, if set, is how catalogs' songs are searched for on YouTube
	// Music, like through the engine's searches, which are shared and
	// remembered. Nil asks YouTube Music each time.
	Search func(ctx context.Context, query string) ([]songs.Song, error)

	youtube    lookup // by video ID
	soundcloud lookup
	ytdlp      lookup
	signedIn   lookup // yt-dlp signed in to a spare YouTube account, or nil
	music      searcher
	catalogs   []Catalog

	mu sync.Mutex
	// kept remembers found songs by link while their links still work, so
	// a song played again doesn't wait for a lookup or ask the site again.
	kept map[string]songs.Song
	// finding are the lookups on their way, by link, so a song asked for
	// twice at once, like while it's typed and again on Enter, is looked up
	// once.
	finding map[string]*finding
	// checked is when YouTube last made a bot check.
	checked time.Time
}

// finding is a lookup on its way, which everyone asking for the same song
// waits for.
type finding struct {
	done  chan struct{}
	ahead bool // only the fast ways, for FindAhead
	song  songs.Song
	err   error
}

// errAhead is a lookup FindAhead leaves for when the song is asked for.
var errAhead = errors.New("left for when it's asked for")

// NewFinder returns a Finder that uses yt for YouTube links, sc for
// SoundCloud links and dl for the rest, and for either when the fast way
// fails. dl is nil when yt-dlp isn't installed. signedIn is yt-dlp signed in
// to a spare YouTube account, for bot checks, or nil without one. The
// catalogs' links play the same songs from YouTube Music.
func NewFinder(yt *YouTube, sc *SoundCloud, dl, signedIn *YTDLP, catalogs ...Catalog) *Finder {
	f := &Finder{youtube: yt, soundcloud: sc, music: yt, catalogs: catalogs}
	if dl != nil {
		f.ytdlp = dl
	}
	if signedIn != nil {
		f.signedIn = signedIn
	}
	return f
}

// Find looks up the song at link, or remembers it from before. If the same
// song is being looked up already, it waits for that lookup, which runs with
// the first asker's ctx.
func (f *Finder) Find(ctx context.Context, link string) (songs.Song, error) {
	return f.lookup(ctx, link, false)
}

// Knows reports whether the song at link is remembered, so finding it asks
// nobody.
func (f *Finder) Knows(link string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.recallLocked(f.key(link))
	return ok
}

// FindAhead looks up the song at link while someone is still choosing it, so
// Find finds it ready. It only takes the fast ways, never yt-dlp, and leaves
// YouTube alone during a bot check: a lookup nobody ends
// up asking for mustn't cost seconds, or tie up the spare account.
func (f *Finder) FindAhead(ctx context.Context, link string) {
	if f.Fast(link) {
		f.lookup(ctx, link, true)
	}
}

func (f *Finder) lookup(ctx context.Context, link string, ahead bool) (songs.Song, error) {
	key := f.key(link)
	f.mu.Lock()
	if song, ok := f.recallLocked(key); ok {
		f.mu.Unlock()
		return song, nil
	}
	w, ok := f.finding[key]
	if !ok {
		w = &finding{done: make(chan struct{}), ahead: ahead}
		if f.finding == nil {
			f.finding = make(map[string]*finding)
		}
		f.finding[key] = w
		go func() {
			w.song, w.err = f.find(ctx, link, ahead)
			if w.err == nil {
				f.keep(key, w.song)
			}
			f.mu.Lock()
			delete(f.finding, key)
			f.mu.Unlock()
			close(w.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-w.done:
		if w.err != nil && w.ahead && !ahead {
			return f.lookup(ctx, link, false) // ahead, only the fast ways were tried
		}
		return w.song, w.err
	case <-ctx.Done():
		return songs.Song{}, ctx.Err()
	}
}

// Fast reports whether link goes to one of the fast ways, not to yt-dlp: a
// YouTube video, SoundCloud, or a catalog like Spotify. Those are cheap to
// look up ahead of time, while someone is still typing.
func (f *Finder) Fast(link string) bool {
	if _, ok := songs.YouTubeID(link); ok || songs.IsSoundCloud(link) {
		return true
	}
	for _, c := range f.catalogs {
		if c.Owns(link) {
			return true
		}
	}
	return false
}

// find looks the song at link up. ahead, it only takes the fast ways.
func (f *Finder) find(ctx context.Context, link string, ahead bool) (songs.Song, error) {
	if id, ok := songs.YouTubeID(link); ok {
		checked := f.botChecked()
		switch {
		case checked && ahead:
			return songs.Song{}, errAhead
		case checked && f.signedIn != nil:
			return f.asSignedIn(ctx, songs.WatchLink(id))
		}
		song, err := f.youtube.Lookup(ctx, id)
		if errors.Is(err, songs.ErrBotCheck) {
			f.mu.Lock()
			f.checked = time.Now()
			f.mu.Unlock()
			if f.signedIn != nil && !ahead {
				logger(f.Logger).Info("youtube made a bot check, so the spare account signs in for a while", slog.String("link", songs.WatchLink(id)))
				return f.asSignedIn(ctx, songs.WatchLink(id))
			}
		}
		if ahead {
			return song, err
		}
		return f.orBackup(ctx, song, err, "youtube", songs.WatchLink(id))
	}
	if songs.IsYouTube(link) {
		// A playlist, a channel, or another page that isn't one video. yt-dlp
		// would look up every video on it, which is how the bot check starts.
		return songs.Song{}, fmt.Errorf("youtube %s: %w", link, songs.ErrPlaylist)
	}
	if songs.IsSoundCloud(link) {
		song, err := f.soundcloud.Lookup(ctx, link)
		if ahead {
			return song, err
		}
		return f.orBackup(ctx, song, err, "soundcloud", link)
	}
	for _, c := range f.catalogs {
		if c.Owns(link) {
			return f.byName(ctx, c, link, ahead)
		}
	}
	if ahead {
		return songs.Song{}, errAhead
	}
	if f.ytdlp == nil {
		return songs.Song{}, fmt.Errorf("%s: %w", link, songs.ErrNoYTDLP)
	}
	if f.GuardLinks {
		if err := netguard.CheckLink(ctx, link); err != nil {
			return songs.Song{}, fmt.Errorf("%w: %w", songs.ErrUnplayable, err)
		}
	}
	return f.ytdlp.Lookup(ctx, link)
}

// key is how the song at link is remembered: the same for every link to it,
// like youtu.be and youtube.com links, or two shares of a Spotify song.
func (f *Finder) key(link string) string {
	if id, ok := songs.YouTubeID(link); ok {
		return songs.WatchLink(id)
	}
	for _, c := range f.catalogs {
		if c.Owns(link) {
			if key, ok := c.Key(link); ok {
				return key
			}
			break
		}
	}
	return link
}

// botChecked reports whether YouTube made a bot check within botCheckFor.
func (f *Finder) botChecked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Since(f.checked) < botCheckFor
}

// asSignedIn asks yt-dlp for a YouTube song, signed in to the spare account.
func (f *Finder) asSignedIn(ctx context.Context, link string) (songs.Song, error) {
	song, err := f.signedIn.Lookup(ctx, link)
	if errors.Is(err, errOldCookies) {
		logger(f.Logger).Warn("the spare YouTube account's cookies stopped working: export them again, as lullaby's README says")
		return songs.Song{}, fmt.Errorf("%w: %w", songs.ErrBotCheck, err)
	}
	return song, err
}

// recallLocked returns a song found before whose link still has keepFor to
// go. f.mu must be held.
func (f *Finder) recallLocked(key string) (songs.Song, bool) {
	song, ok := f.kept[key]
	if !ok || time.Until(song.Audio.Expires) < keepFor {
		return songs.Song{}, false
	}
	return song, true
}

// keep remembers a found song, if its link says how long it works.
func (f *Finder) keep(key string, song songs.Song) {
	if time.Until(song.Audio.Expires) < keepFor {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kept == nil {
		f.kept = make(map[string]songs.Song)
	}
	if len(f.kept) >= maxKept {
		for k, s := range f.kept {
			if time.Until(s.Audio.Expires) < keepFor {
				delete(f.kept, k)
			}
		}
	}
	if len(f.kept) >= maxKept {
		clear(f.kept)
	}
	f.kept[key] = song
}

// orBackup returns what the fast way found, or asks yt-dlp when it failed.
// yt-dlp can't play a song the site says is gone either, and would only take
// a few more seconds to say so. Nor can it get past YouTube's bot check from
// the same address, and its requests would only keep the check going.
func (f *Finder) orBackup(ctx context.Context, song songs.Song, err error, site, link string) (songs.Song, error) {
	if err == nil || errors.Is(err, songs.ErrUnplayable) || errors.Is(err, songs.ErrPlaylist) || errors.Is(err, songs.ErrBotCheck) || ctx.Err() != nil || f.ytdlp == nil {
		return song, err
	}
	if errors.Is(err, netguard.ErrPrivate) {
		// yt-dlp would follow the same link where it was refused.
		return song, fmt.Errorf("%w: %w", songs.ErrUnplayable, err)
	}
	logger(f.Logger).Warn(site+" fast path failed, trying yt-dlp", slog.String("link", redact.Link(link)), redact.Attr(err))
	return f.ytdlp.Lookup(ctx, link)
}
