package lullaby

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/internal/sources"
	"github.com/windrunner21/lullaby/songs"
)

func TestRemembersSuggestions(t *testing.T) {
	e := &Engine{suggested: make(map[string]suggestion)}
	picked := songs.Song{Title: "Snooze", Artist: "SZA", Link: "https://www.youtube.com/watch?v=ZqSlV5LmrTg",
		Thumbnail: "https://yt3.googleusercontent.com/o3i9=w120-h120-l90-rj"}
	e.remember([]songs.Song{picked})

	s, ok := e.recall(picked.Link)
	if !ok || s.Title != "Snooze" {
		t.Fatalf("recall = %+v, %v; want the suggestion", s, ok)
	}
	video := songs.Song{ID: "ZqSlV5LmrTg", Title: "SZA - Snooze (Audio)", Artist: "SZAVEVO", Length: 202 * time.Second,
		Thumbnail: "https://i.ytimg.com/vi/ZqSlV5LmrTg/sddefault.jpg"}
	got := withMusicInfo(video, s)
	if got.Title != "Snooze" || got.Artist != "SZA" || got.Thumbnail != "https://yt3.googleusercontent.com/o3i9=w544-h544-l90-rj" || got.Length != video.Length {
		t.Errorf("withMusicInfo = %+v, want YouTube Music's title, artist and big cover, and the video's length", got)
	}

	// Old suggestions are forgotten.
	e.suggested[picked.Link] = suggestion{song: picked, at: time.Now().Add(-suggestedFor - time.Minute)}
	if _, ok := e.recall(picked.Link); ok {
		t.Error("recalled a suggestion older than suggestedFor")
	}
	for i := range maxSuggested + 50 {
		e.remember([]songs.Song{{Link: fmt.Sprint("https://example.com/", i)}})
	}
	if len(e.suggested) > maxSuggested {
		t.Errorf("remembering %d suggestions, want at most %d", len(e.suggested), maxSuggested)
	}
}

func TestSearchMemory(t *testing.T) {
	e := &Engine{searched: make(map[string]search), searching: make(map[string]*searching)}
	sza := func(title string) songs.Song { return songs.Song{Title: title, Artist: "SZA"} }
	e.keepSearchLocked(searchKey("  SZA   s "), []songs.Song{
		sza("Snooze"), sza("Saturn"), sza("Kill Bill"), sza("Good Days"), sza("Shirt"),
		{Title: "Snooze (acoustic)", Artist: "Someone Else"},
	})

	if found, ok := e.recallSearch("sza s"); !ok || len(found) != 6 {
		t.Errorf("the same words: %d songs, %v; want all 6 from memory", len(found), ok)
	}
	// "sza sh" keeps only Shirt: too few to show without asking YouTube Music.
	if found, ok := e.narrowed("sza sh"); ok {
		t.Errorf("narrowed to %d songs and used them, want at least %d", len(found), minNarrowed)
	}
	// "sza " plus more words still matching many songs is answered at once.
	e.keepSearchLocked("sz", []songs.Song{sza("Snooze"), sza("Saturn"), sza("Kill Bill"), {Title: "Size", Artist: "Other"}})
	found, ok := e.narrowed("sza")
	if !ok || len(found) != 3 {
		t.Errorf("narrowing \"sz\" to \"sza\": %d songs, %v; want SZA's 3", len(found), ok)
	}
	// Going back a letter can't use a search for longer words.
	if _, ok := e.narrowed("s"); ok {
		t.Error("narrowed from a search for longer words")
	}
	// Old searches aren't used.
	e.searched["sz"] = search{found: e.searched["sz"].found, at: time.Now().Add(-searchedFor - time.Minute)}
	if _, ok := e.narrowed("sza"); ok {
		t.Error("narrowed from an old search")
	}
}

func TestSuggest(t *testing.T) {
	e := &Engine{ctx: context.Background(), suggested: make(map[string]suggestion), searched: make(map[string]search), searching: make(map[string]*searching)}
	var found []songs.Song
	for i := range MaxSuggestions + 5 {
		found = append(found, songs.Song{Title: fmt.Sprint("Snooze ", i), Artist: "SZA", Link: fmt.Sprint("https://example.com/", i)})
	}
	e.keepSearchLocked("sza snooze", found)

	got := e.Suggest(context.Background(), "  SZA snooze ")
	if len(got) != MaxSuggestions {
		t.Fatalf("suggested %d songs, want %d", len(got), MaxSuggestions)
	}
	if _, ok := e.recall(got[0].Link); !ok {
		t.Error("a suggestion wasn't remembered for when it's picked")
	}
	// What's returned is the caller's: changing it changes nothing for others.
	got[0].Title = "changed"
	if again := e.Suggest(context.Background(), "sza snooze"); again[0].Title != "Snooze 0" {
		t.Errorf("a caller's change reached the engine's memory: %q", again[0].Title)
	}
	if got := e.Suggest(context.Background(), "s"); got != nil {
		t.Errorf("one letter got %d suggestions, want none", len(got))
	}
}

func TestIsLink(t *testing.T) {
	tests := map[string]bool{
		"https://www.youtube.com/watch?v=LDY_XyxBu8A": true,
		"youtu.be/LDY_XyxBu8A":                        true,
		"https://soundcloud.com/sza/snooze":           true,
		"HTTP://example.com/song.mp3":                 true,
		"sza snooze":                                  false,
		"snooze by sza https":                         false,
	}
	for in, want := range tests {
		if got := isLink(in); got != want {
			t.Errorf("isLink(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSearchShared(t *testing.T) {
	var asked atomic.Int32
	release := make(chan struct{})
	client := &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		asked.Add(1)
		<-release
		return answer200(`{}`), nil
	})}
	e := &Engine{ctx: context.Background(), yt: sources.NewYouTube(client, "https://api.invalid", "https://music.invalid"),
		searched: make(map[string]search), searching: make(map[string]*searching)}
	// Suggest's search is on its way when the song is asked for with the
	// same words, and a keystroke's gives up on it.
	impatient, cancel := context.WithCancel(context.Background())
	results := make(chan error, 3)
	for _, ctx := range []context.Context{context.Background(), impatient, context.Background()} {
		go func() {
			_, err := e.search(ctx, "SZA  snooze")
			results <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-results; err != context.Canceled {
		t.Errorf("the one who gave up got %v", err)
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	// One search, plus the visitor ID it asked for first.
	if n := asked.Load(); n != 2 {
		t.Errorf("asked YouTube %d times, want one search and one visitor ID", n)
	}
	if _, ok := e.recallSearch("sza snooze"); !ok {
		t.Error("the search wasn't remembered for the next keystroke")
	}
}

func TestVideoCover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/hd/maxresdefault.jpg" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := thumbnails
	thumbnails = srv.URL + "/"
	defer func() { thumbnails = old }()

	e := &Engine{http: srv.Client()}
	if got := e.videoCover(context.Background(), "hd"); got != srv.URL+"/hd/maxresdefault.jpg" {
		t.Errorf("a video with an HD thumbnail got %q", got)
	}
	if got := e.videoCover(context.Background(), "old"); got != srv.URL+"/old/mqdefault.jpg" {
		t.Errorf("a video without one got %q, want the small 16:9 one", got)
	}
}

func TestCover(t *testing.T) {
	in := "https://yt3.googleusercontent.com/o3i9-dA34xDjbw1hz4nhJO6LA6V4=w120-h120-l90-rj"
	if got, want := cover(in), "https://yt3.googleusercontent.com/o3i9-dA34xDjbw1hz4nhJO6LA6V4=w544-h544-l90-rj"; got != want {
		t.Errorf("cover = %q, want %q", got, want)
	}
	if got := cover("https://i.ytimg.com/vi/LDY_XyxBu8A/sddefault.jpg"); got != "https://i.ytimg.com/vi/LDY_XyxBu8A/sddefault.jpg" {
		t.Errorf("cover changed a link without a size: %q", got)
	}
}
