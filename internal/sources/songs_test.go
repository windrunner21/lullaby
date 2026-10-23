package sources

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// fakeLookup answers every lookup with the same song or error.
type fakeLookup struct {
	song  songs.Song
	err   error
	asked []string
}

func (f *fakeLookup) Lookup(_ context.Context, s string) (songs.Song, error) {
	f.asked = append(f.asked, s)
	return f.song, f.err
}

func TestFind(t *testing.T) {
	fast := songs.Song{Title: "from the fast path"}
	backup := songs.Song{Title: "from yt-dlp"}
	broken := fmt.Errorf("x: %w", songs.ErrNoAudio)
	tests := []struct {
		name    string
		link    string
		fastErr error
		want    string
		yt, sc  []string // what the fast paths were asked
		dl      []string // what yt-dlp was asked
	}{
		{"YouTube", "https://youtu.be/LDY_XyxBu8A", nil,
			"from the fast path", []string{"LDY_XyxBu8A"}, nil, nil},
		{"YouTube breaks", "https://youtu.be/LDY_XyxBu8A", broken,
			"from yt-dlp", []string{"LDY_XyxBu8A"}, nil, []string{"https://www.youtube.com/watch?v=LDY_XyxBu8A"}},
		{"YouTube video gone", "https://youtu.be/LDY_XyxBu8A", fmt.Errorf("x: %w", songs.ErrUnplayable),
			"", []string{"LDY_XyxBu8A"}, nil, nil},
		{"YouTube's bot check", "https://youtu.be/LDY_XyxBu8A", fmt.Errorf("x: %w", songs.ErrBotCheck),
			"", []string{"LDY_XyxBu8A"}, nil, nil},
		{"SoundCloud", "https://soundcloud.com/someone/a-song", nil,
			"from the fast path", nil, []string{"https://soundcloud.com/someone/a-song"}, nil},
		{"SoundCloud breaks", "https://soundcloud.com/someone/a-song", broken,
			"from yt-dlp", nil, []string{"https://soundcloud.com/someone/a-song"}, []string{"https://soundcloud.com/someone/a-song"}},
		{"SoundCloud playlist", "https://soundcloud.com/someone/sets/an-album", fmt.Errorf("x: %w", songs.ErrPlaylist),
			"", nil, []string{"https://soundcloud.com/someone/sets/an-album"}, nil},
		{"another site", "https://bandcamp.com/a-song", nil,
			"from yt-dlp", nil, nil, []string{"https://bandcamp.com/a-song"}},
	}
	for _, tt := range tests {
		song := fast
		if tt.fastErr != nil {
			song = songs.Song{}
		}
		yt := &fakeLookup{song: song, err: tt.fastErr}
		sc := &fakeLookup{song: song, err: tt.fastErr}
		dl := &fakeLookup{song: backup}
		f := &Finder{youtube: yt, soundcloud: sc, ytdlp: dl}

		got, err := f.Find(context.Background(), tt.link)
		if got.Title != tt.want {
			t.Errorf("%s: got %q (err %v), want %q", tt.name, got.Title, err, tt.want)
		}
		if !slices.Equal(yt.asked, tt.yt) || !slices.Equal(sc.asked, tt.sc) || !slices.Equal(dl.asked, tt.dl) {
			t.Errorf("%s: asked YouTube %q, SoundCloud %q and yt-dlp %q; want %q, %q and %q", tt.name, yt.asked, sc.asked, dl.asked, tt.yt, tt.sc, tt.dl)
		}
	}
}

func TestFindWithoutYTDLP(t *testing.T) {
	broken := fmt.Errorf("youtube: %w", songs.ErrNoAudio)
	f := &Finder{youtube: &fakeLookup{err: broken}}
	if _, err := f.Find(context.Background(), "https://bandcamp.com/a-song"); !errors.Is(err, songs.ErrNoYTDLP) {
		t.Errorf("another site without yt-dlp: %v, want ErrNoYTDLP", err)
	}
	if _, err := f.Find(context.Background(), "https://youtu.be/LDY_XyxBu8A"); !errors.Is(err, songs.ErrNoAudio) {
		t.Errorf("YouTube failing without yt-dlp: %v, want the fast path's own error", err)
	}
	if f := NewFinder(nil, nil, nil, nil); f.ytdlp != nil || f.signedIn != nil {
		t.Error("NewFinder without yt-dlp kept a nil *YTDLP, which would crash when used")
	}
}

func TestFindSignsIn(t *testing.T) {
	works := songs.Song{Title: "Snooze", Audio: songs.Audio{URL: "a"}}
	yt := &fakeLookup{err: fmt.Errorf("youtube: %w", songs.ErrBotCheck)}
	dl := &fakeLookup{song: works}
	signedIn := &fakeLookup{song: works}
	f := &Finder{youtube: yt, ytdlp: dl, signedIn: signedIn}

	song, err := f.Find(context.Background(), "https://youtu.be/LDY_XyxBu8A")
	if err != nil || song.Title != "Snooze" {
		t.Fatalf("Find = %q, %v; want it from the spare account", song.Title, err)
	}
	if !slices.Equal(signedIn.asked, []string{"https://www.youtube.com/watch?v=LDY_XyxBu8A"}) || len(dl.asked) != 0 {
		t.Errorf("signed in for %q, signed out for %q; want only signed in", signedIn.asked, dl.asked)
	}

	// For a while, songs go straight to the account, without asking the
	// fast path to be turned down again.
	f.Find(context.Background(), "https://youtu.be/MX7FSAnRPug")
	if len(yt.asked) != 1 || len(signedIn.asked) != 2 {
		t.Errorf("asked the fast path %d times and the account %d, want 1 and 2", len(yt.asked), len(signedIn.asked))
	}
	// Then the fast path gets another try.
	f.checked = time.Now().Add(-botCheckFor - time.Minute)
	yt.err = nil
	yt.song = songs.Song{Title: "from the fast path"}
	if song, _ := f.Find(context.Background(), "https://youtu.be/9bZkp7q19f0"); song.Title != "from the fast path" {
		t.Errorf("after %v, got %q, want the fast path again", botCheckFor, song.Title)
	}

	// Cookies opened in a browser again stop working: that's the bot check
	// for the person who asked, and a warning in the log.
	f.checked = time.Now()
	signedIn.err = fmt.Errorf("yt-dlp: %w", errOldCookies)
	if _, err := f.Find(context.Background(), "https://youtu.be/kJQP7kiw5Fk"); !errors.Is(err, songs.ErrBotCheck) {
		t.Errorf("with old cookies: %v, want ErrBotCheck", err)
	}

	// Without an account, the bot check is the answer, and nothing is asked.
	f = &Finder{youtube: &fakeLookup{err: fmt.Errorf("youtube: %w", songs.ErrBotCheck)}, ytdlp: dl}
	if _, err := f.Find(context.Background(), "https://youtu.be/LDY_XyxBu8A"); !errors.Is(err, songs.ErrBotCheck) || len(dl.asked) != 0 {
		t.Errorf("without an account: %v, and yt-dlp asked %q; want ErrBotCheck and nothing", err, dl.asked)
	}
}

func TestFindRemembers(t *testing.T) {
	works := songs.Song{Title: "Snooze", Audio: songs.Audio{URL: "a", Expires: time.Now().Add(6 * time.Hour)}}
	yt := &fakeLookup{song: works}
	f := &Finder{youtube: yt}

	for _, link := range []string{"https://youtu.be/LDY_XyxBu8A", "https://www.youtube.com/watch?v=LDY_XyxBu8A&t=42s"} {
		if song, err := f.Find(context.Background(), link); err != nil || song.Title != "Snooze" {
			t.Fatalf("Find(%s) = %v, %v", link, song.Title, err)
		}
	}
	if len(yt.asked) != 1 {
		t.Errorf("asked YouTube %d times for one song by two links, want once", len(yt.asked))
	}

	// A link close to stopping isn't reused, so the song can play to its end.
	f.kept["https://www.youtube.com/watch?v=LDY_XyxBu8A"] = songs.Song{Audio: songs.Audio{Expires: time.Now().Add(keepFor / 2)}}
	f.Find(context.Background(), "https://youtu.be/LDY_XyxBu8A")
	if len(yt.asked) != 2 {
		t.Errorf("reused a link with %v left", keepFor/2)
	}

	// A song whose link doesn't say when it stops isn't remembered.
	sc := &fakeLookup{song: songs.Song{Title: "A Song", Audio: songs.Audio{URL: "b"}}}
	f.soundcloud = sc
	for range 2 {
		f.Find(context.Background(), "https://soundcloud.com/someone/a-song")
	}
	if len(sc.asked) != 2 {
		t.Errorf("asked SoundCloud %d times, want each time", len(sc.asked))
	}
}

// slowLookup answers once release is closed, and counts what it's asked.
type slowLookup struct {
	release chan struct{}
	mu      sync.Mutex
	asked   int
}

func (l *slowLookup) Lookup(context.Context, string) (songs.Song, error) {
	l.mu.Lock()
	l.asked++
	l.mu.Unlock()
	<-l.release
	return songs.Song{Title: "Snooze", Audio: songs.Audio{URL: "a", Expires: time.Now().Add(6 * time.Hour)}}, nil
}

func TestFindSharesLookups(t *testing.T) {
	yt := &slowLookup{release: make(chan struct{})}
	f := &Finder{youtube: yt}
	// Someone pastes a link, the Finder starts looking it up while they type, and
	// they press Enter before it's back. Another asker gives up meanwhile.
	quitter, quit := context.WithCancel(context.Background())
	results := make(chan error, 3)
	for _, ctx := range []context.Context{context.Background(), quitter, context.Background()} {
		go func() {
			_, err := f.Find(ctx, "https://youtu.be/LDY_XyxBu8A")
			results <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	quit()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Errorf("the one who gave up got %v, want context.Canceled", err)
	}
	close(yt.release)
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if yt.asked != 1 {
		t.Errorf("looked one song up %d times at once, want once", yt.asked)
	}
}

func TestFindYouTubePages(t *testing.T) {
	dl := &fakeLookup{}
	f := &Finder{ytdlp: dl}
	for _, link := range []string{
		"https://www.youtube.com/playlist?list=PLx0sYbCqOb8TBPRdmBHs5Iftvv9TPboYG",
		"https://music.youtube.com/playlist?list=OLAK5uy_example",
		"https://www.youtube.com/@SZA",
		"https://m.youtube.com/channel/UCxyz",
	} {
		if _, err := f.Find(context.Background(), link); !errors.Is(err, songs.ErrPlaylist) {
			t.Errorf("%s: %v, want ErrPlaylist", link, err)
		}
	}
	// yt-dlp would look up every video on them, one after another.
	if len(dl.asked) != 0 {
		t.Errorf("sent %q to yt-dlp", dl.asked)
	}
}

func TestFast(t *testing.T) {
	f := &Finder{catalogs: []Catalog{NewSpotify(nil, SpotifySite)}}
	for link, want := range map[string]bool{
		"https://youtu.be/LDY_XyxBu8A":                          true,
		"https://soundcloud.com/someone/a-song":                 true,
		"https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2": true,
		"https://youtu.be/LDY_Xy":                               false, // still being typed
		"https://vimeo.com/76979871":                            false, // yt-dlp's
	} {
		if got := f.Fast(link); got != want {
			t.Errorf("Fast(%s) = %v, want %v", link, got, want)
		}
	}
}

func TestFindGivesUpWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	yt := &fakeLookup{err: context.Canceled}
	dl := &fakeLookup{}
	f := &Finder{youtube: yt, ytdlp: dl}
	if _, err := f.Find(ctx, "https://youtu.be/LDY_XyxBu8A"); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if len(dl.asked) != 0 {
		t.Errorf("tried yt-dlp after being canceled")
	}
}
