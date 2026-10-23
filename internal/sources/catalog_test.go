package sources

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

func TestMatch(t *testing.T) {
	snooze := Track{Title: "Snooze", Artists: []string{"SZA"}, Length: 201800 * time.Millisecond}
	tests := []struct {
		name  string
		track Track
		found []songs.Song
		want  string // the matched song's title and artist, or empty for none
	}{
		{"skips a version too short to be it", snooze, []songs.Song{
			{Title: "Snooze (Acoustic) [feat. Justin Bieber]", Artist: "SZA & Justin Bieber", Length: 190 * time.Second},
			{Title: "Snooze", Artist: "SZA", Length: 202 * time.Second},
		}, "Snooze by SZA"},
		{"takes the earlier of two as good", snooze, []songs.Song{
			{Title: "Snooze", Artist: "SZA", Length: 202 * time.Second, Link: "first"},
			{Title: "Snooze", Artist: "SZA", Length: 201 * time.Second, Link: "second"},
		}, "Snooze by SZA"},
		{"a remaster has the same main title",
			Track{Title: "Bohemian Rhapsody - Remastered 2011", Artists: []string{"Queen"}, Length: 354 * time.Second},
			[]songs.Song{{Title: "Bohemian Rhapsody (Remastered 2011)", Artist: "Queen", Length: 355 * time.Second}},
			"Bohemian Rhapsody (Remastered 2011) by Queen"},
		{"one of two artists is enough",
			Track{Title: "SICKO MODE", Artists: []string{"Travis Scott & Drake"}, Length: 312 * time.Second},
			[]songs.Song{{Title: "SICKO MODE", Artist: "Travis Scott", Length: 313 * time.Second}},
			"SICKO MODE by Travis Scott"},
		{"apostrophes don't matter",
			Track{Title: "Don't Stop Me Now", Artists: []string{"Queen"}, Length: 209 * time.Second},
			[]songs.Song{{Title: "Dont Stop Me Now", Artist: "Queen", Length: 210 * time.Second}},
			"Dont Stop Me Now by Queen"},
		{"another artist's song with the same name is too long",
			Track{Title: "Hello", Artists: []string{"Adele"}, Length: 295 * time.Second},
			[]songs.Song{{Title: "Hello", Artist: "Lionel Richie", Length: 252 * time.Second}}, ""},
		{"a title alone isn't enough",
			Track{Title: "Hello", Artists: []string{"Adele"}},
			[]songs.Song{{Title: "Hello", Artist: "Lionel Richie", Length: 252 * time.Second}}, ""},
		{"a word inside another word isn't the title",
			Track{Title: "Love", Artists: []string{"Someone"}},
			[]songs.Song{{Title: "Lovely", Artist: "Someone", Length: 200 * time.Second}}, ""},
		{"nothing found", snooze, nil, ""},
	}
	for _, tt := range tests {
		got, ok := match(tt.track, tt.found)
		if want := tt.want; ok != (want != "") || ok && got.Title+" by "+got.Artist != want {
			t.Errorf("%s: matched %q by %q (%v), want %q", tt.name, got.Title, got.Artist, ok, want)
		}
		if tt.name == "takes the earlier of two as good" && got.Link != "first" {
			t.Errorf("%s: took %q", tt.name, got.Link)
		}
	}
}

// fakeCatalog owns links starting with its prefix, and answers each with the
// same track or error.
type fakeCatalog struct {
	prefix string
	track  Track
	err    error
	asked  []string
}

func (c *fakeCatalog) Owns(link string) bool     { return strings.HasPrefix(link, c.prefix) }
func (c *fakeCatalog) Key(string) (string, bool) { return "", false }

func (c *fakeCatalog) Track(_ context.Context, link string) (Track, error) {
	c.asked = append(c.asked, link)
	return c.track, c.err
}

// fakeSearch answers every search with the same songs.
type fakeSearch struct {
	found []songs.Song
	asked []string
}

func (s *fakeSearch) Search(_ context.Context, query string) ([]songs.Song, error) {
	s.asked = append(s.asked, query)
	return s.found, nil
}

func TestFindByName(t *testing.T) {
	cat := &fakeCatalog{prefix: "https://open.spotify.com/", track: Track{
		Link: "https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2", Title: "Snooze", Artists: []string{"SZA"},
		Length: 201800 * time.Millisecond, Cover: "https://image-cdn-ak.spotifycdn.com/image/cover",
	}}
	search := &fakeSearch{found: []songs.Song{
		{ID: "acoustic000", Link: songs.WatchLink("acoustic000"), Title: "Snooze (Acoustic)", Artist: "SZA", Length: 190 * time.Second},
		{ID: "LDY_XyxBu8A", Link: songs.WatchLink("LDY_XyxBu8A"), Title: "Snooze", Artist: "SZA", Length: 202 * time.Second},
	}}
	yt := &fakeLookup{song: songs.Song{ID: "LDY_XyxBu8A", Title: "SZA - Snooze (Official Video)",
		Audio: songs.Audio{URL: "audio", Expires: time.Now().Add(6 * time.Hour)}}}
	f := &Finder{youtube: yt, music: search, catalogs: []Catalog{cat}}

	link := "https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2?si=abc"
	song, err := f.Find(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if song.Link != cat.track.Link || song.Title != "Snooze" || song.Artist != "SZA" || song.Thumbnail != cat.track.Cover {
		t.Errorf("song = %+v, want Spotify's link, title, artist and cover", song)
	}
	if song.Audio.URL != "audio" || !slices.Equal(yt.asked, []string{"LDY_XyxBu8A"}) {
		t.Errorf("audio %q from %q, want YouTube's for the matching song", song.Audio.URL, yt.asked)
	}
	if !slices.Equal(search.asked, []string{"SZA Snooze"}) {
		t.Errorf("searched %q", search.asked)
	}
	// Played again, it's remembered.
	f.Find(context.Background(), link)
	if len(cat.asked) != 1 || len(search.asked) != 1 {
		t.Errorf("asked Spotify %d times and searched %d times for one song, want once", len(cat.asked), len(search.asked))
	}

	// Nothing like it on YouTube Music.
	cat.track.Artists, cat.track.Link = []string{"Someone Else"}, "https://open.spotify.com/track/other"
	search.found = search.found[1:]
	search.found[0].Length = 3 * time.Minute
	if _, err := f.Find(context.Background(), "https://open.spotify.com/track/other"); !errors.Is(err, songs.ErrNoMatch) {
		t.Errorf("no match: %v, want ErrNoMatch", err)
	}
	// What the catalog says goes to the person who asked.
	cat.err = songs.ErrPlaylist
	if _, err := f.Find(context.Background(), "https://open.spotify.com/album/x"); !errors.Is(err, songs.ErrPlaylist) {
		t.Errorf("an album: %v, want ErrPlaylist", err)
	}
}

// catalogServer answers each path with its body, any other with 404. Links
// to the hosts in redirects, asked for over plain HTTP, are redirected
// where the map says. Its client takes every host to the server, so short
// links work without leaving this machine.
func catalogServer(t *testing.T, pages map[string]string, redirects map[string]string) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to, ok := redirects[r.Host+r.URL.Path]; ok {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		body, ok := pages[r.URL.RequestURI()]
		if !ok {
			body, ok = pages[r.URL.Path]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
	return srv, client
}

// spotifyEmbed is Spotify's embed page for Snooze, saved on 2026-10-23 and
// trimmed to the song's data and a few fields that aren't read.
const spotifyEmbed = `<!DOCTYPE html><html><head><title>Spotify Embed</title></head><body>
<script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"state":{"data":{"entity":{
"type":"track","name":"Snooze","uri":"spotify:track:4iZ4pt7kvcaH6Yo8UoZ4s2","id":"4iZ4pt7kvcaH6Yo8UoZ4s2","title":"Snooze",
"artists":[{"name":"SZA","uri":"spotify:artist:7tYKF4w9nC0nq9CsPZTHyP"}],"releaseDate":{"isoString":"2022-12-09T00:00:00Z"},
"duration":201800,"isPlayable":true,"isExplicit":true,
"visualIdentity":{"image":[
{"url":"https://image-cdn-ak.spotifycdn.com/image/ab67616d00001e02bc18bdade69ec5ef0bb25b17","maxHeight":300,"maxWidth":300},
{"url":"https://image-cdn-ak.spotifycdn.com/image/ab67616d00004851bc18bdade69ec5ef0bb25b17","maxHeight":64,"maxWidth":64},
{"url":"https://image-cdn-ak.spotifycdn.com/image/ab67616d0000b273bc18bdade69ec5ef0bb25b17","maxHeight":640,"maxWidth":640}]}
}}}}},"page":"/track/[id]"}</script></body></html>`

func TestSpotify(t *testing.T) {
	srv, client := catalogServer(t, map[string]string{"/embed/track/4iZ4pt7kvcaH6Yo8UoZ4s2": spotifyEmbed},
		map[string]string{"spotify.link/AbCdEf": "https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2?si=x"})
	s := NewSpotify(client, srv.URL)

	for _, link := range []string{
		"https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2?si=8c1b2",
		"https://open.spotify.com/intl-de/track/4iZ4pt7kvcaH6Yo8UoZ4s2",
		"http://spotify.link/AbCdEf",
	} {
		if !s.Owns(link) {
			t.Errorf("doesn't own %s", link)
		}
		got, err := s.Track(context.Background(), link)
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		if got.Title != "Snooze" || !slices.Equal(got.Artists, []string{"SZA"}) || got.Length != 201800*time.Millisecond {
			t.Errorf("%s: %+v", link, got)
		}
		if got.Link != "https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2" || !strings.HasSuffix(got.Cover, "b273bc18bdade69ec5ef0bb25b17") {
			t.Errorf("%s: link %q, cover %q; want the song's link and the 640 px cover", link, got.Link, got.Cover)
		}
	}

	for link, want := range map[string]error{
		"https://open.spotify.com/album/1pzvBxYgT6OVwJLtHkrdQK":    songs.ErrPlaylist,
		"https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M": songs.ErrPlaylist,
		"https://open.spotify.com/artist/7tYKF4w9nC0nq9CsPZTHyP":   songs.ErrUnplayable,
		"https://open.spotify.com/track/0000000000000000000000":    songs.ErrUnplayable, // not found
		"https://open.spotify.com/track/short":                     songs.ErrUnplayable,
	} {
		if _, err := s.Track(context.Background(), link); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", link, err, want)
		}
	}
	if s.Owns("https://www.youtube.com/watch?v=LDY_XyxBu8A") {
		t.Error("owns a YouTube link")
	}
}

// appleSnooze is Apple's lookup answer for Snooze, saved on 2026-10-23 and
// trimmed.
const appleSnooze = `{"resultCount":1,"results":[{"wrapperType":"track","kind":"song","artistId":1368156612,
"collectionId":1658650093,"trackId":1658650499,"artistName":"SZA","collectionName":"SOS","trackName":"Snooze",
"trackCensoredName":"Snooze","trackViewUrl":"https://music.apple.com/us/album/snooze/1658650093?i=1658650499&uo=4",
"artworkUrl100":"https://is1-ssl.mzstatic.com/image/thumb/Music122/v4/62/93/13/6293132e-20ff-67ab-3d1f-96bb6797a6ba/196589564955.jpg/100x100bb.jpg",
"trackTimeMillis":201800,"country":"USA","primaryGenreName":"R&B/Soul"}]}`

func TestAppleMusic(t *testing.T) {
	srv, client := catalogServer(t, map[string]string{
		"/lookup?country=us&entity=song&id=1658650499": appleSnooze,
		"/lookup?country=az&entity=song&id=1658650499": appleSnooze,
		"/lookup?country=us&entity=song&id=1":          `{"resultCount":0,"results":[]}`,
	}, nil)
	a := NewAppleMusic(client, srv.URL)

	for _, link := range []string{
		"https://music.apple.com/us/album/snooze/1658650093?i=1658650499",
		"https://music.apple.com/az/song/snooze/1658650499",
		"https://music.apple.com/song/1658650499",
	} {
		if !a.Owns(link) {
			t.Errorf("doesn't own %s", link)
		}
		got, err := a.Track(context.Background(), link)
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		if got.Title != "Snooze" || !slices.Equal(got.Artists, []string{"SZA"}) || got.Length != 201800*time.Millisecond {
			t.Errorf("%s: %+v", link, got)
		}
		if got.Link != "https://music.apple.com/us/album/snooze/1658650093?i=1658650499" || !strings.HasSuffix(got.Cover, "/600x600bb.jpg") {
			t.Errorf("%s: link %q, cover %q; want Apple's link without tracking, and a 600 px cover", link, got.Link, got.Cover)
		}
	}
	for link, want := range map[string]error{
		"https://music.apple.com/us/album/sos/1658650093":             songs.ErrPlaylist,
		"https://music.apple.com/us/playlist/todays-hits/pl.f4d106fe": songs.ErrPlaylist,
		"https://music.apple.com/us/artist/sza/1368156612":            songs.ErrUnplayable,
		"https://music.apple.com/us/song/gone/1":                      songs.ErrUnplayable,
	} {
		if _, err := a.Track(context.Background(), link); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", link, err, want)
		}
	}
}

// deezerSnooze is Deezer's answer for Snooze, saved on 2026-10-23 and trimmed.
const deezerSnooze = `{"id":2055292087,"readable":true,"title":"Snooze","title_short":"Snooze","title_version":"",
"isrc":"USRC12204591","link":"https://www.deezer.com/track/2055292087","duration":201,"rank":845535,
"preview":"https://cdnt-preview.dzcdn.net/api/1/1/e/2/7/0/e2740662feeec.mp3",
"contributors":[{"id":7869640,"name":"SZA","role":"Main"}],"artist":{"id":7869640,"name":"SZA"},
"album":{"id":389815327,"title":"SOS","cover_xl":"https://cdn-images.dzcdn.net/images/cover/328d68300e654b21831b261e413780e0/1000x1000-000000-80-0-0.jpg"},
"type":"track"}`

func TestDeezer(t *testing.T) {
	srv, client := catalogServer(t, map[string]string{
		"/track/2055292087": deezerSnooze,
		"/track/1":          `{"error":{"type":"DataException","message":"no data","code":800}}`,
	}, map[string]string{"link.deezer.com/s/31abc": "https://www.deezer.com/en/track/2055292087?host=0&utm_campaign=share"})
	d := NewDeezer(client, srv.URL)

	for _, link := range []string{
		"https://www.deezer.com/track/2055292087",
		"https://www.deezer.com/en/track/2055292087",
		"http://link.deezer.com/s/31abc",
	} {
		if !d.Owns(link) {
			t.Errorf("doesn't own %s", link)
		}
		got, err := d.Track(context.Background(), link)
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		if got.Title != "Snooze" || !slices.Equal(got.Artists, []string{"SZA"}) || got.Length != 201*time.Second ||
			got.Link != "https://www.deezer.com/track/2055292087" || !strings.Contains(got.Cover, "1000x1000") {
			t.Errorf("%s: %+v", link, got)
		}
	}
	for link, want := range map[string]error{
		"https://www.deezer.com/en/album/389815327":  songs.ErrPlaylist,
		"https://www.deezer.com/playlist/3155776842": songs.ErrPlaylist,
		"https://www.deezer.com/en/artist/7869640":   songs.ErrUnplayable,
		"https://www.deezer.com/track/1":             songs.ErrUnplayable,
	} {
		if _, err := d.Track(context.Background(), link); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", link, err, want)
		}
	}
}

// yandexSnooze is Yandex Music's answer for Snooze, saved on 2026-10-23 and
// trimmed.
const yandexSnooze = `{"invocationInfo":{"hostname":"hostname","exec-duration-millis":3},"result":[{"id":"148182281",
"realId":"148182281","title":"Snooze","available":true,"availableFullWithoutPermission":false,"durationMs":201850,
"previewDurationMs":30000,"coverUri":"avatars.yandex.net/get-music-content/17730916/d8c77204.a.40645743-1/%%",
"artists":[{"id":7121390,"name":"SZA","various":false}],"albums":[{"id":40645743,"title":"SOS","year":2022}],
"lyricsAvailable":false,"type":"music"}]}`

func TestYandexMusic(t *testing.T) {
	srv, client := catalogServer(t, map[string]string{"/tracks/148182281": yandexSnooze}, nil)
	y := NewYandexMusic(client, srv.URL)

	for _, link := range []string{
		"https://music.yandex.ru/album/40645743/track/148182281?utm_source=share",
		"https://music.yandex.com/track/148182281",
	} {
		if !y.Owns(link) {
			t.Errorf("doesn't own %s", link)
		}
		got, err := y.Track(context.Background(), link)
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		if got.Title != "Snooze" || !slices.Equal(got.Artists, []string{"SZA"}) || got.Length != 201850*time.Millisecond ||
			got.Cover != "https://avatars.yandex.net/get-music-content/17730916/d8c77204.a.40645743-1/600x600" ||
			strings.Contains(got.Link, "utm_source") {
			t.Errorf("%s: %+v", link, got)
		}
	}
	for link, want := range map[string]error{
		"https://music.yandex.ru/album/40645743":             songs.ErrPlaylist,
		"https://music.yandex.ru/users/someone/playlists/3":  songs.ErrPlaylist,
		"https://music.yandex.ru/artist/7121390":             songs.ErrUnplayable,
		"https://music.yandex.ru/album/1/track/999999999999": songs.ErrUnplayable, // Yandex answers 400 or 404
	} {
		if _, err := y.Track(context.Background(), link); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", link, err, want)
		}
	}
	for link, want := range map[string]bool{
		"https://music.yandex.kz/track/1":              true,
		"https://yandex.ru/search?text=x":              false,
		"https://music.yandex.ru.evil.example/track/1": false,
	} {
		if y.Owns(link) != want {
			t.Errorf("Owns(%s) = %v", link, !want)
		}
	}
}

func TestCatalogKeys(t *testing.T) {
	spotify, apple := NewSpotify(nil, SpotifySite), NewAppleMusic(nil, AppleAPI)
	deezer, yandex := NewDeezer(nil, DeezerAPI), NewYandexMusic(nil, YandexAPI)
	tests := []struct {
		c    Catalog
		link string
		want string // empty for no key
	}{
		{spotify, "https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2?si=abc", "spotify:4iZ4pt7kvcaH6Yo8UoZ4s2"},
		{spotify, "https://open.spotify.com/intl-de/track/4iZ4pt7kvcaH6Yo8UoZ4s2?si=xyz", "spotify:4iZ4pt7kvcaH6Yo8UoZ4s2"},
		{spotify, "https://spotify.link/aBcDeFgHiJ", ""},
		{spotify, "https://open.spotify.com/album/4iZ4pt7kvcaH6Yo8UoZ4s2", ""},
		{apple, "https://music.apple.com/us/album/snooze/1655268023?i=1655268385&uo=4", "apple:us:1655268385"},
		{apple, "https://music.apple.com/us/song/snooze/1655268385", "apple:us:1655268385"},
		{apple, "https://music.apple.com/gb/song/snooze/1655268385", "apple:gb:1655268385"},
		{apple, "https://music.apple.com/us/album/sos/1655268023", ""},
		{deezer, "https://www.deezer.com/en/track/2047662537?utm_source=share", "deezer:2047662537"},
		{deezer, "https://deezer.com/track/2047662537", "deezer:2047662537"},
		{deezer, "https://deezer.page.link/aBcD", ""},
		{yandex, "https://music.yandex.ru/album/22962015/track/105922651", "yandex:105922651"},
		{yandex, "https://music.yandex.com/track/105922651?utm_source=share", "yandex:105922651"},
		{yandex, "https://music.yandex.ru/album/22962015", ""},
	}
	for _, tt := range tests {
		got, ok := tt.c.Key(tt.link)
		if ok != (tt.want != "") || got != tt.want {
			t.Errorf("Key(%s) = %q, %v; want %q", tt.link, got, ok, tt.want)
		}
	}
}

// spotifyLike is a fakeCatalog that names songs as Spotify does.
type spotifyLike struct{ *fakeCatalog }

func (spotifyLike) Key(link string) (string, bool) { return (&Spotify{}).Key(link) }

func TestOneSongAcrossItsLinks(t *testing.T) {
	cat := spotifyLike{&fakeCatalog{prefix: "https://", track: Track{
		Link: "https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2", Title: "Snooze", Artists: []string{"SZA"}, Length: 202 * time.Second,
	}}}
	music := &fakeSearch{found: []songs.Song{{ID: "LDY_XyxBu8A", Link: songs.WatchLink("LDY_XyxBu8A"), Title: "Snooze", Artist: "SZA", Length: 202 * time.Second}}}
	yt := &fakeLookup{song: songs.Song{ID: "LDY_XyxBu8A", Audio: songs.Audio{URL: "audio", Expires: time.Now().Add(6 * time.Hour)}}}
	var shared []string
	f := &Finder{youtube: yt, music: music, catalogs: []Catalog{cat}}
	f.Search = func(ctx context.Context, query string) ([]songs.Song, error) {
		shared = append(shared, query)
		return music.found, nil
	}

	// Two people share the same song: their links differ in Spotify's si=.
	for _, link := range []string{
		"https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2?si=abc",
		"https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2?si=xyz",
	} {
		if _, err := f.Find(context.Background(), link); err != nil {
			t.Fatal(err)
		}
	}
	if len(cat.asked) != 1 || len(shared) != 1 {
		t.Errorf("asked Spotify %d times and searched %d times for two links to one song, want once", len(cat.asked), len(shared))
	}
	if len(music.asked) != 0 {
		t.Errorf("searched YouTube Music itself %d times, want the shared search", len(music.asked))
	}

	// A short link has to be followed, but then it's the same song.
	if _, err := f.Find(context.Background(), "https://spotify.link/aBcDeFgHiJ"); err != nil {
		t.Fatal(err)
	}
	if len(cat.asked) != 2 || len(shared) != 1 || len(yt.asked) != 1 {
		t.Errorf("a short link: asked Spotify %d times, searched %d and looked up %d; want 2, 1 and 1", len(cat.asked), len(shared), len(yt.asked))
	}
}

func TestAppleStoresStayApart(t *testing.T) {
	// The store's answer links the song in the US store even when it was
	// asked about Azerbaijan's: that mustn't answer for the US store.
	var countries []string
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		country := r.URL.Query().Get("country")
		countries = append(countries, country)
		body := `{"resultCount": 0, "results": []}`
		if country == "az" {
			body = appleSnooze
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	music := &fakeSearch{found: []songs.Song{{ID: "LDY_XyxBu8A", Link: songs.WatchLink("LDY_XyxBu8A"), Title: "Snooze", Artist: "SZA", Length: 202 * time.Second}}}
	yt := &fakeLookup{song: songs.Song{ID: "LDY_XyxBu8A", Audio: songs.Audio{URL: "audio", Expires: time.Now().Add(6 * time.Hour)}}}
	f := &Finder{youtube: yt, music: music, catalogs: []Catalog{NewAppleMusic(client, "https://apple.invalid")}}
	if _, err := f.Find(context.Background(), "https://music.apple.com/az/song/snooze/1658650499"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Find(context.Background(), "https://music.apple.com/us/song/snooze/1658650499"); !errors.Is(err, songs.ErrUnplayable) {
		t.Errorf("the US store's link = %v, want it asked, and not there; asked %v", err, countries)
	}
}

func TestCatalogsWaitOutABotCheck(t *testing.T) {
	cat := spotifyLike{&fakeCatalog{prefix: "https://", track: Track{
		Link: "https://open.spotify.com/track/4iZ4pt7kvcaH6Yo8UoZ4s2", Title: "Snooze", Artists: []string{"SZA"}, Length: 202 * time.Second,
	}}}
	music := &fakeSearch{found: []songs.Song{{ID: "LDY_XyxBu8A", Link: songs.WatchLink("LDY_XyxBu8A"), Title: "Snooze", Artist: "SZA", Length: 202 * time.Second}}}
	f := &Finder{music: music, catalogs: []Catalog{cat}, checked: time.Now()}
	f.FindAhead(context.Background(), cat.track.Link)
	if len(music.asked) != 0 {
		t.Errorf("searched YouTube Music %d times ahead, during a bot check", len(music.asked))
	}
}
