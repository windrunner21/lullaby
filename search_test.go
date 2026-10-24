package lullaby

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

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

func TestRememberMakesRoom(t *testing.T) {
	e := &Engine{suggested: make(map[string]suggestion)}
	now := time.Now()
	link := func(i int) string { return fmt.Sprint("https://www.youtube.com/watch?v=", i) }
	// 452 suggestions from the last few minutes, the newest last.
	for i := range 452 {
		e.suggested[link(i)] = suggestion{song: songs.Song{Link: link(i)}, at: now.Add(-time.Duration(452-i) * time.Second)}
	}
	var radio []songs.Song
	for i := range 49 {
		radio = append(radio, songs.Song{Link: link(1000 + i)})
	}
	e.remember(radio)
	if len(e.suggested) > maxSuggested {
		t.Errorf("remembering %d, want at most %d", len(e.suggested), maxSuggested)
	}
	for _, i := range []int{1000, 1048, 451, 400} {
		if _, ok := e.recall(link(i)); !ok {
			t.Errorf("forgot %s, one of the radio's or a recent suggestion", link(i))
		}
	}
	if _, ok := e.recall(link(0)); ok {
		t.Error("kept the suggestion remembered longest ago")
	}

	// The same radio again adds nothing, so nothing is forgotten for it.
	before := len(e.suggested)
	e.remember(radio)
	if len(e.suggested) != before {
		t.Errorf("remembering songs it had already: %d remembered, want %d", len(e.suggested), before)
	}

	// Full, with old ones among the songs coming again, and the same song
	// twice: what's left fits, and nothing new is forgotten.
	clear(e.suggested)
	for i := range maxSuggested {
		at := now.Add(-time.Duration(maxSuggested-i) * time.Second)
		if i < 48 {
			at = now.Add(-suggestedFor - time.Minute)
		}
		e.suggested[link(i)] = suggestion{song: songs.Song{Link: link(i)}, at: at}
	}
	again := []songs.Song{{Link: link(2000)}, {Link: link(2000)}}
	for i := range 48 {
		again = append(again, songs.Song{Link: link(i)})
	}
	e.remember(again)
	if len(e.suggested) > maxSuggested {
		t.Errorf("remembering %d, want at most %d", len(e.suggested), maxSuggested)
	}
	for _, i := range []int{0, 47, 2000} {
		if _, ok := e.recall(link(i)); !ok {
			t.Errorf("forgot %s, which was just remembered", link(i))
		}
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
	// Remembered longer, the same words are answered from memory, but
	// narrowing stays with recent searches.
	e.searchFor = 24 * time.Hour
	if _, ok := e.recallSearch("sz"); !ok {
		t.Error("didn't recall a search within Config.SearchMemory")
	}
	if _, ok := e.narrowed("sza"); ok {
		t.Error("narrowed from a search over 10 minutes old")
	}
	// A search that found nothing is believed for a minute only.
	e.keepSearchLocked("zzzz", nil)
	e.searched["zzzz"] = search{at: time.Now().Add(-emptyFor - time.Second)}
	if _, ok := e.recallSearch("zzzz"); ok {
		t.Error("recalled a search that found nothing a while ago")
	}
}

func TestSearchMemoryForgetsTheLeastUsed(t *testing.T) {
	e := &Engine{searched: make(map[string]search)}
	for i := range maxSearched {
		e.keepSearchLocked(fmt.Sprint("words ", i), []songs.Song{{Title: "Snooze"}})
		e.searched[fmt.Sprint("words ", i)] = search{found: []songs.Song{{Title: "Snooze"}}, at: time.Now(), used: time.Now().Add(-time.Duration(maxSearched-i) * time.Second)}
	}
	// The oldest is reused, so the one after it is now used longest ago.
	e.recallSearch("words 0")
	e.keepSearchLocked("one more", []songs.Song{{Title: "Saturn"}})
	if len(e.searched) != maxSearched {
		t.Errorf("remembering %d searches, want %d: one forgotten for the new one", len(e.searched), maxSearched)
	}
	for key, want := range map[string]bool{"words 0": true, "words 1": false, "words 2": true, "one more": true} {
		if _, ok := e.searched[key]; ok != want {
			t.Errorf("%q remembered: %v, want %v", key, ok, want)
		}
	}
}

func TestSuggest(t *testing.T) {
	e := offlineEngine(t, &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		return answer200(`{}`), nil
	})}, silentEncoder(t), "")
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

// musicAnswers answers like YouTube Music: a search for each words in found
// finds one song, with that video ID. It sends the video ID of each lookup
// on lookedUp.
func musicAnswers(found map[string]string, lookedUp chan<- string) *http.Client {
	return &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			return answer200(fmt.Sprintf(`{"contents": {"tabbedSearchResultsRenderer": {"tabs": [{"tabRenderer": {"content": {"sectionListRenderer": {"contents": [{"musicShelfRenderer": {"contents": [
				{"musicResponsiveListItemRenderer": {"playlistItemData": {"videoId": %q}, "flexColumns": [
					{"musicResponsiveListItemFlexColumnRenderer": {"text": {"runs": [{"text": "Kosandra"}]}}},
					{"musicResponsiveListItemFlexColumnRenderer": {"text": {"runs": [{"text": "Miyagi & Andy Panda • Kosandra • 3:42"}]}}}
				]}}
			]}}]}}}}]}}}`, found[body["query"].(string)])), nil
		case strings.HasSuffix(r.URL.Path, "/player"):
			lookedUp <- body["videoId"].(string)
		}
		return answer200(`{}`), nil
	})}
}

func TestTypingOnLooksUpOnce(t *testing.T) {
	lookedUp := make(chan string, 10)
	e := offlineEngine(t, musicAnswers(map[string]string{
		"kos": "aaaaaaaaaaa", "kosa": "bbbbbbbbbbb", "kosan": "ccccccccccc", "snooze": "ddddddddddd",
	}, lookedUp), silentEncoder(t), "")

	// One person typing on, and someone else typing something else.
	for _, typed := range []SuggestRequest{
		{GuildID: 1, UserID: 3, Query: "kos"}, {GuildID: 1, UserID: 3, Query: "kosa"},
		{GuildID: 1, UserID: 3, Query: "kosan"}, {GuildID: 1, UserID: 4, Query: "snooze"},
	} {
		e.SuggestFor(context.Background(), typed)
	}
	got := map[string]bool{}
	for range 2 {
		select {
		case id := <-lookedUp:
			got[id] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("looked up %v, want two songs", got)
		}
	}
	if !got["ccccccccccc"] || !got["ddddddddddd"] {
		t.Errorf("looked up %v, want the first songs for kosan and snooze", got)
	}
	select {
	case id := <-lookedUp:
		t.Errorf("also looked up %s: each keystroke shouldn't cost a lookup", id)
	case <-time.After(2 * settleAfter):
	}
}

func TestEarlyLookupFollowsTheLastKeystroke(t *testing.T) {
	lookedUp := make(chan string, 10)
	e := offlineEngine(t, musicAnswers(nil, lookedUp), silentEncoder(t), "")
	someone := typist{1, 3}
	quiet := func(why string) {
		t.Helper()
		select {
		case id := <-lookedUp:
			t.Errorf("%s, yet looked up %s", why, id)
		case <-time.After(settleAfter + 200*time.Millisecond):
		}
	}

	// A slow search for an older keystroke comes back last: it can't win.
	old := e.typed(someone)
	newer := e.typed(someone)
	e.settle(someone, old, songs.WatchLink("aaaaaaaaaaa"), settleAfter)
	quiet("an older keystroke's search came back after a newer keystroke")
	e.settle(someone, newer, songs.WatchLink("bbbbbbbbbbb"), settleAfter)
	select {
	case id := <-lookedUp:
		if id != "bbbbbbbbbbb" {
			t.Errorf("looked up %s, want the last keystroke's song", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the last keystroke's song wasn't looked up")
	}

	// The pause counts from the keystroke: a search slower than it is
	// followed by the lookup at once.
	slow := e.typed(someone)
	time.Sleep(settleAfter + 50*time.Millisecond)
	start := time.Now()
	e.settle(someone, slow, songs.WatchLink("ccccccccccc"), settleAfter)
	select {
	case <-lookedUp:
		if took := time.Since(start); took >= settleAfter {
			t.Errorf("looked up %v after a slow search, want at once, not after another pause", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no lookup after a slow search")
	}

	// Typing on to one letter, or pasting a link, replaces the words.
	waiting := e.typed(someone)
	e.settle(someone, waiting, songs.WatchLink("ddddddddddd"), settleAfter)
	e.SuggestFor(context.Background(), SuggestRequest{GuildID: 1, UserID: 3, Query: "k"})
	quiet("the words were typed over")
}

func TestEarlyLookupsHaveABudget(t *testing.T) {
	lookedUp := make(chan string, 10)
	e := offlineEngine(t, musicAnswers(nil, lookedUp), silentEncoder(t), "")
	e.mu.Lock()
	for range maxLookAhead {
		e.lookedAhead = append(e.lookedAhead, time.Now())
	}
	e.mu.Unlock()
	someone := typist{1, 3}
	e.settle(someone, e.typed(someone), songs.WatchLink("aaaaaaaaaaa"), settleAfter)
	select {
	case id := <-lookedUp:
		t.Errorf("looked %s up past the budget", id)
	case <-time.After(settleAfter + 200*time.Millisecond):
	}
	// An hour on, there's room again.
	e.mu.Lock()
	for i := range e.lookedAhead {
		e.lookedAhead[i] = time.Now().Add(-time.Hour)
	}
	e.mu.Unlock()
	e.settle(someone, e.typed(someone), songs.WatchLink("aaaaaaaaaaa"), settleAfter)
	select {
	case <-lookedUp:
	case <-time.After(2 * time.Second):
		t.Fatal("no lookup once the hour had passed")
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

func TestPastedLinksCountTowardTheBudget(t *testing.T) {
	lookedUp := make(chan string, maxLookAhead+5)
	e := offlineEngine(t, musicAnswers(nil, lookedUp), silentEncoder(t), "")
	e.mu.Lock()
	for range maxLookAhead {
		e.lookedAhead = append(e.lookedAhead, time.Now())
	}
	e.mu.Unlock()
	for i := range 3 {
		e.SuggestFor(context.Background(), SuggestRequest{GuildID: 1, UserID: snowflake.ID(100 + i), Query: songs.WatchLink(fmt.Sprintf("%011d", i))})
	}
	select {
	case id := <-lookedUp:
		t.Errorf("looked pasted link %s up past the budget", id)
	case <-time.After(settleAfter + 200*time.Millisecond):
	}

	// With room, a pasted link is looked up without waiting for a pause.
	e.mu.Lock()
	e.lookedAhead = nil
	e.mu.Unlock()
	start := time.Now()
	e.SuggestFor(context.Background(), SuggestRequest{GuildID: 1, UserID: 9, Query: songs.WatchLink("aaaaaaaaaaa")})
	select {
	case <-lookedUp:
		if took := time.Since(start); took >= settleAfter {
			t.Errorf("a pasted link waited %v, want it looked up at once", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a pasted link wasn't looked up")
	}
}

func TestEarlyLookupLimits(t *testing.T) {
	e := &Engine{}
	if !e.lookAheadLocked() || !e.lookAheadLocked() {
		t.Fatal("the first two early lookups weren't allowed")
	}
	if e.lookAheadLocked() {
		t.Errorf("allowed a third early lookup with %d already asking", maxLookingAhead)
	}
	e.lookingAhead = 0
	// A day's worth, spread over the hours before this one.
	e.lookedAhead = nil
	for i := range maxLookAheadDay {
		e.lookedAhead = append(e.lookedAhead, time.Now().Add(-time.Hour-time.Duration(i)*time.Minute))
	}
	if e.lookAheadLocked() {
		t.Errorf("allowed an early lookup past %d in a day", maxLookAheadDay)
	}
}

func TestTypistsAreBounded(t *testing.T) {
	e := &Engine{}
	for i := range maxTypists + 1 {
		e.typed(typist{guild: 1, user: snowflake.ID(i + 1)})
	}
	if len(e.typing) > maxTypists {
		t.Errorf("remembering %d typists, want at most %d", len(e.typing), maxTypists)
	}
	// One more still gets suggestions, but nothing looked up early.
	if keystroke := e.typed(typist{guild: 1, user: 1000}); keystroke != 0 {
		t.Errorf("a typist past the limit got keystroke %d, want 0, untracked", keystroke)
	}
}

func TestNarrowingIsFromRecentSearches(t *testing.T) {
	// Searches remembered for a day still answer the same words, but an
	// old search for shorter ones would show fewer choices than a search.
	e := &Engine{searched: make(map[string]search), searchFor: 24 * time.Hour}
	sza := []songs.Song{{Title: "Snooze", Artist: "SZA"}, {Title: "Saturn", Artist: "SZA"}, {Title: "Shirt", Artist: "SZA"}}
	e.searched["sz"] = search{found: sza, at: time.Now().Add(-12 * time.Hour)}
	if _, ok := e.narrowed("sza"); ok {
		t.Error("narrowed from a search 12 hours old")
	}
	if found, ok := e.recallSearch("sz"); !ok || len(found) != 3 {
		t.Errorf("the same words, 12 hours on: %d songs, %v; want all 3 from memory", len(found), ok)
	}
}

func TestNarrowingCountsAsUse(t *testing.T) {
	e := &Engine{searched: make(map[string]search)}
	now := time.Now()
	for i := range maxSearched - 1 {
		e.searched[fmt.Sprintf("other %d", i)] = search{found: []songs.Song{{Title: "other"}}, at: now, used: now}
	}
	sza := []songs.Song{{Title: "Snooze", Artist: "SZA"}, {Title: "Saturn", Artist: "SZA"}, {Title: "Shirt", Artist: "SZA"}}
	e.searched["sz"] = search{found: sza, at: now, used: now.Add(-time.Minute)}
	if _, ok := e.narrowed("sza s"); !ok {
		t.Fatal("didn't narrow")
	}
	e.keepSearchLocked("new words", []songs.Song{{Title: "new"}})
	if _, ok := e.searched["sz"]; !ok {
		t.Error("forgot the search just narrowed down, as if it were the least used")
	}
}

func TestEarlyLookupSlotsAreFreed(t *testing.T) {
	// Two lookups take both slots; once they're done, or the engine stops,
	// the slots are free again.
	for _, stops := range []bool{false, true} {
		t.Run(fmt.Sprint("engine stops: ", stops), func(t *testing.T) {
			entered, release := make(chan struct{}, maxLookingAhead+1), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			e := offlineEngine(t, &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/player") {
					entered <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return answer200(`{}`), nil
			})}, silentEncoder(t), "")
			life, stop := context.WithCancel(context.Background())
			defer stop()
			e.ctx = life
			for i := range maxLookingAhead {
				who := typist{1, snowflake.ID(i + 1)}
				e.settle(who, e.typed(who), songs.WatchLink(fmt.Sprintf("%011d", i)), 0)
			}
			for range maxLookingAhead {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("an early lookup never started")
				}
			}
			if stops {
				stop()
			} else {
				unblock()
			}
			for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
				e.mu.Lock()
				busy := e.lookingAhead
				e.mu.Unlock()
				if busy == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%d early lookup slots still taken", busy)
				}
			}
		})
	}
}

func TestRefusedEarlyLookupsArentCounted(t *testing.T) {
	// A day-old one is forgotten, so there's room for one more today.
	e := &Engine{lookedAhead: []time.Time{time.Now().Add(-25 * time.Hour)}}
	for range maxLookAheadDay - 1 {
		e.lookedAhead = append(e.lookedAhead, time.Now().Add(-2*time.Hour))
	}
	if !e.lookAheadLocked() || len(e.lookedAhead) != maxLookAheadDay {
		t.Fatalf("a day-old early lookup still counted: %d remembered", len(e.lookedAhead))
	}
	e.lookingAhead--
	if e.lookAheadLocked() || e.lookingAhead != 0 || len(e.lookedAhead) != maxLookAheadDay {
		t.Errorf("an early lookup refused for the day was counted: %d asking, %d remembered", e.lookingAhead, len(e.lookedAhead))
	}
}

func TestUntrackedTypistStillGetsChoices(t *testing.T) {
	e := offlineEngine(t, &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		t.Error("asked a site for remembered suggestions")
		return answer200(`{}`), nil
	})}, silentEncoder(t), "")
	for i := range maxTypists {
		e.typed(typist{1, snowflake.ID(i + 1)})
	}
	e.mu.Lock()
	e.keepSearchLocked("sza snooze", []songs.Song{{Title: "Snooze", Artist: "SZA", Link: songs.WatchLink("aaaaaaaaaaa")}})
	e.mu.Unlock()
	got := e.SuggestFor(context.Background(), SuggestRequest{GuildID: 1, UserID: 1000, Query: "sza snooze"})
	if len(got) != 1 || got[0].Title != "Snooze" {
		t.Fatalf("a typist past the limit got %+v, want the remembered choice", got)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.typing) != maxTypists || e.lookingAhead != 0 || len(e.lookedAhead) != 0 {
		t.Errorf("a typist past the limit was tracked or looked up early: %d typists, %d asking, %d counted", len(e.typing), e.lookingAhead, len(e.lookedAhead))
	}
}
