package sources

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/windrunner21/lullaby/songs"
)

const (
	// closeLength is how far apart two copies of a song can be and still
	// count as the same length: album and video versions differ by a second
	// or two.
	closeLength = 3 * time.Second
	// maxDrift is the most a match can be longer or shorter than the song
	// asked for. An acoustic or radio version is usually further off.
	maxDrift = 10 * time.Second
)

// A Catalog is a site that names songs but gives bots no audio, like
// Spotify. Finder plays its songs by finding the same ones on YouTube Music.
type Catalog interface {
	// Owns reports whether link is one of the site's.
	Owns(link string) bool
	// Key names the song at one of the site's links without asking the
	// site, the same for every link to it: share links differ in tracking
	// codes like Spotify's si=. A short link can't be named until it's
	// followed.
	Key(link string) (string, bool)
	// Track finds the song at link.
	Track(ctx context.Context, link string) (Track, error)
}

// Track is a song as a catalog knows it.
type Track struct {
	Link    string // its page on the site
	Title   string
	Artists []string
	Length  time.Duration
	Cover   string
}

// byName plays a catalog's song: it finds the same song on YouTube Music,
// and gives it the catalog's link, title, artists and cover, since those are
// what the person asked for. ahead, it only takes the fast ways.
func (f *Finder) byName(ctx context.Context, c Catalog, link string, ahead bool) (songs.Song, error) {
	t, err := c.Track(ctx, link)
	if err != nil {
		return songs.Song{}, err
	}
	// A short link is only named once it's followed: it may be a song
	// already found through another link. A link with a name of its own
	// keeps it, since the catalog's own link for the song can name another
	// country's store.
	var key string
	var named bool
	if _, ok := c.Key(link); !ok {
		key, named = c.Key(t.Link)
	}
	if named {
		f.mu.Lock()
		song, ok := f.recallLocked(key)
		f.mu.Unlock()
		if ok {
			return song, nil
		}
	}
	if ahead && f.botChecked() {
		return songs.Song{}, errAhead // its search would ask YouTube Music too
	}
	query := t.Title
	if len(t.Artists) > 0 {
		query = t.Artists[0] + " " + t.Title
	}
	search := f.music.Search
	if f.Search != nil {
		search = f.Search
	}
	found, err := search(ctx, query)
	if err != nil {
		return songs.Song{}, err
	}
	same, ok := match(t, found)
	if !ok {
		return songs.Song{}, fmt.Errorf("%s: %w", t.Link, songs.ErrNoMatch)
	}
	song, err := f.lookup(ctx, same.Link, ahead)
	if err != nil {
		return songs.Song{}, err
	}
	song.Link, song.Title, song.Artist = t.Link, t.Title, strings.Join(t.Artists, ", ")
	if t.Cover != "" {
		song.Thumbnail = t.Cover
	}
	if named {
		f.keep(key, song)
	}
	return song, nil
}

// match picks the song in found that's the same as t: it needs two of t's
// title, one of its artists, and about its length. found is in the search's
// order, likeliest first, so ties go to the earlier song.
func match(t Track, found []songs.Song) (songs.Song, bool) {
	title := simple(mainTitle(t.Title))
	var best songs.Song
	bestScore := 0
	for _, s := range found {
		titled := hasWords(simple(s.Title), title) || hasWords(title, simple(mainTitle(s.Title)))
		byArtist := false
		for _, a := range t.Artists {
			a, b := simple(a), simple(s.Artist)
			byArtist = byArtist || hasWords(b, a) || hasWords(a, b)
		}
		close := false
		if t.Length > 0 && s.Length > 0 {
			d := (t.Length - s.Length).Abs()
			if d > maxDrift {
				continue
			}
			close = d <= closeLength
		}

		score, signs := 0, 0
		if titled {
			score, signs = score+2, signs+1
		}
		if byArtist {
			score, signs = score+2, signs+1
		}
		if close {
			score, signs = score+1, signs+1
		}
		if signs >= 2 && score > bestScore {
			best, bestScore = s, score
		}
	}
	return best, bestScore > 0
}

// mainTitle is a title without what's added after it, like "(feat. Drake)"
// or "- Remastered 2011".
func mainTitle(title string) string {
	if i := strings.IndexAny(title, "(["); i > 0 {
		title = title[:i]
	}
	for _, sep := range []string{" - ", " – ", " — "} {
		if before, _, ok := strings.Cut(title, sep); ok && strings.TrimSpace(before) != "" {
			title = before
		}
	}
	return title
}

// simple is s in lower case, with only its letters and numbers, a space
// between each word. Apostrophes go without a space, so "Don't" and "Dont"
// are the same.
func simple(s string) string {
	s = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(s))
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}), " ")
}

// hasWords reports whether the simple text s has the simple words in it,
// whole: "love" is in "all you need is love", but not in "lovely".
func hasWords(s, words string) bool {
	return words != "" && strings.Contains(" "+s+" ", " "+words+" ")
}
