package sources

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

// Answers trimmed from YouTube's own, saved on 2026-10-22. AUDIO_URL stands
// for the video server: the real links carry the asker's IP address.
const (
	playerSnooze = `{
	"playabilityStatus": {"status": "OK", "playableInEmbed": true},
	"streamingData": {
		"expiresInSeconds": "21540",
		"adaptiveFormats": [
			{"itag": 137, "url": "AUDIO_URL/videoplayback?expire=1792710000&itag=137&source=youtube&c=IOS", "mimeType": "video/mp4; codecs=\"avc1.640028\"", "bitrate": 4903966, "contentLength": "80980337", "approxDurationMs": "280738"},
			{"itag": 139, "url": "AUDIO_URL/videoplayback?expire=1792710000&itag=139&source=youtube&c=IOS", "mimeType": "audio/mp4; codecs=\"mp4a.40.5\"", "bitrate": 50126, "contentLength": "1713531", "approxDurationMs": "280868", "audioQuality": "AUDIO_QUALITY_LOW", "audioSampleRate": "22050", "audioChannels": 2},
			{"itag": 140, "url": "AUDIO_URL/videoplayback?expire=1792710000&itag=140&source=youtube&c=IOS", "mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "bitrate": 130562, "contentLength": "4545273", "approxDurationMs": "280798", "audioQuality": "AUDIO_QUALITY_MEDIUM", "audioSampleRate": "44100", "audioChannels": 2}
		],
		"serverAbrStreamingUrl": "AUDIO_URL/videoplayback?sabr=1"
	},
	"videoDetails": {
		"videoId": "LDY_XyxBu8A", "title": "SZA - Snooze (Official Video)", "lengthSeconds": "281",
		"channelId": "UCKzhoblE_rPZbHtQKPVUp2A", "author": "SZAVEVO", "isLiveContent": false,
		"thumbnail": {"thumbnails": [
			{"url": "https://i.ytimg.com/vi/LDY_XyxBu8A/hqdefault.jpg", "width": 480, "height": 360},
			{"url": "https://i.ytimg.com/vi/LDY_XyxBu8A/sddefault.jpg", "width": 640, "height": 480}
		]}
	}
}`
	playerGone = `{"playabilityStatus": {"status": "ERROR", "reason": "This video is unavailable"}}`

	musicSearchSnooze = `{"contents": {"tabbedSearchResultsRenderer": {"tabs": [{"tabRenderer": {"title": "YT Music", "content": {"sectionListRenderer": {"contents": [{"musicShelfRenderer": {"contents": [
	{"musicResponsiveListItemRenderer": {
		"thumbnail": {"musicThumbnailRenderer": {"thumbnail": {"thumbnails": [
			{"url": "https://yt3.googleusercontent.com/o3i9-dA34xDjbw1hz4nhJO6LA6V4-ZYjs_kh7aUA0sq1pieJejt_Hnor-Rxl77GA1ugAFqxcmEUwy3c=w60-h60-l90-rj", "width": 60, "height": 60},
			{"url": "https://yt3.googleusercontent.com/o3i9-dA34xDjbw1hz4nhJO6LA6V4-ZYjs_kh7aUA0sq1pieJejt_Hnor-Rxl77GA1ugAFqxcmEUwy3c=w120-h120-l90-rj", "width": 120, "height": 120}
		]}}},
		"flexColumns": [
			{"musicResponsiveListItemFlexColumnRenderer": {"text": {"runs": [{"text": "Snooze", "navigationEndpoint": {"watchEndpoint": {"videoId": "ZqSlV5LmrTg"}}}]}}},
			{"musicResponsiveListItemFlexColumnRenderer": {"text": {"runs": [
				{"text": "SZA", "navigationEndpoint": {"browseEndpoint": {"browseId": "UCeKDV9JgivrXehVluw5bKFA"}}},
				{"text": " • "},
				{"text": "SOS", "navigationEndpoint": {"browseEndpoint": {"browseId": "MPREb_5ruOkUYLVZL"}}},
				{"text": " • "},
				{"text": "3:22"}
			]}}}
		],
		"playlistItemData": {"videoId": "ZqSlV5LmrTg"}
	}},
	{"musicResponsiveListItemRenderer": {
		"thumbnail": {"musicThumbnailRenderer": {"thumbnail": {"thumbnails": [
			{"url": "https://yt3.googleusercontent.com/tw5VGXEsehs9OpwnpbubqGp_3Pq9so7QShdyJSlCpXeI2mLRvqRqLNbA7EC4zcNWrFE0_lj9HxpZ23v6=w60-h60-l90-rj", "width": 60, "height": 60},
			{"url": "https://yt3.googleusercontent.com/tw5VGXEsehs9OpwnpbubqGp_3Pq9so7QShdyJSlCpXeI2mLRvqRqLNbA7EC4zcNWrFE0_lj9HxpZ23v6=w120-h120-l90-rj", "width": 120, "height": 120}
		]}}},
		"flexColumns": [
			{"musicResponsiveListItemFlexColumnRenderer": {"text": {"runs": [{"text": "Snooze", "navigationEndpoint": {"watchEndpoint": {"videoId": "MtUefXEQRAY"}}}]}}},
			{"musicResponsiveListItemFlexColumnRenderer": {"text": {"runs": [
				{"text": "SZA", "navigationEndpoint": {"browseEndpoint": {"browseId": "UCeKDV9JgivrXehVluw5bKFA"}}},
				{"text": " • "},
				{"text": "SOS", "navigationEndpoint": {"browseEndpoint": {"browseId": "MPREb_Fgij8PG2LWd"}}},
				{"text": " • "},
				{"text": "3:22"}
			]}}}
		],
		"playlistItemData": {"videoId": "MtUefXEQRAY"}
	}}
]}}]}}}}]}}}`
)

// fakeAPI answers like YouTube's API and video servers.
type fakeAPI struct {
	url     string
	player  string
	visitor int // status for /visitor_id, 0 for a normal answer

	mu          sync.Mutex
	visitorWait chan struct{} // if set, /visitor_id answers once it's closed
	count       map[string]int
	bodies      map[string]map[string]any
	headers     map[string]http.Header
}

func newFakeAPI(t *testing.T, player string) (*YouTube, *fakeAPI) {
	f := &fakeAPI{player: player, count: map[string]int{}, bodies: map[string]map[string]any{}, headers: map[string]http.Header{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return NewYouTube(srv.Client(), srv.URL, srv.URL+"/music"), f
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.count[r.URL.Path]++
	f.bodies[r.URL.Path] = body
	f.headers[r.URL.Path] = r.Header.Clone()
	f.mu.Unlock()

	switch r.URL.Path {
	case "/visitor_id":
		f.mu.Lock()
		wait := f.visitorWait
		f.mu.Unlock()
		if wait != nil {
			<-wait
		}
		if f.visitor != 0 {
			w.WriteHeader(f.visitor)
			return
		}
		io.WriteString(w, `{"responseContext": {"visitorData": "CgtWaXNpdG9yMTIz"}}`)
	case "/player":
		io.WriteString(w, strings.ReplaceAll(f.player, "AUDIO_URL", f.url))
	case "/music/search":
		io.WriteString(w, musicSearchSnooze)
	case "/videoplayback":
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0})
	default:
		http.NotFound(w, r)
	}
}

// client returns the clientName the last request to path said it came from.
func (f *fakeAPI) client(path string) any {
	c, _ := f.body(path)["context"].(map[string]any)["client"].(map[string]any)
	return c["clientName"]
}

// body is what the last request to path sent.
func (f *fakeAPI) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

// header is the headers of the last request to path.
func (f *fakeAPI) header(path string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[path]
}

// requests is how many requests went to path.
func (f *fakeAPI) requests(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count[path]
}

func TestLookup(t *testing.T) {
	yt, api := newFakeAPI(t, playerSnooze)
	song, err := yt.Lookup(context.Background(), "LDY_XyxBu8A")
	if err != nil {
		t.Fatal(err)
	}

	if song.ID != "LDY_XyxBu8A" || song.Link != "https://www.youtube.com/watch?v=LDY_XyxBu8A" {
		t.Errorf("ID, Link = %q, %q", song.ID, song.Link)
	}
	if song.Title != "SZA - Snooze (Official Video)" || song.Artist != "SZA" {
		t.Errorf("Title, Artist = %q, %q; want the title and SZA without VEVO", song.Title, song.Artist)
	}
	if song.Length != 281*time.Second {
		t.Errorf("Length = %v, want 4m41s", song.Length)
	}
	if song.Thumbnail != "https://i.ytimg.com/vi/LDY_XyxBu8A/sddefault.jpg" {
		t.Errorf("Thumbnail = %q, want the widest", song.Thumbnail)
	}
	a := song.Audio
	if !strings.Contains(a.URL, "itag=140") || a.Size != 4545273 {
		t.Errorf("Audio = %q, %d bytes; want itag 140, the best sound", a.URL, a.Size)
	}
	if a.Header.Get("User-Agent") != iosAgent {
		t.Errorf("audio User-Agent = %q, want the iPhone app's", a.Header.Get("User-Agent"))
	}
	if until := time.Until(a.Expires); until < 5*time.Hour || until > 6*time.Hour {
		t.Errorf("Expires in %v, want about 6 hours", until)
	}

	if c := api.client("/player"); c != "IOS" {
		t.Errorf("clientName = %v, want IOS", c)
	}
	if id := api.body("/player")["videoId"]; id != "LDY_XyxBu8A" {
		t.Errorf("videoId = %v", id)
	}
	h := api.header("/player")
	if h.Get("User-Agent") != iosAgent || h.Get("X-Goog-Visitor-Id") != "CgtWaXNpdG9yMTIz" {
		t.Errorf("headers = %v, want the iPhone app's user agent and the visitor ID", h)
	}
}

func TestLookupErrors(t *testing.T) {
	sabrOnly := strings.ReplaceAll(playerSnooze, `"url": `, `"signatureCipher": `)
	botCheck := `{"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm you’re not a bot"}}`
	ageCheck := `{"playabilityStatus": {"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm your age"}}`
	tests := []struct {
		name       string
		player     string
		want       error
		unplayable bool
	}{
		{"gone", playerGone, songs.ErrUnplayable, true},
		{"SABR only", sabrOnly, songs.ErrNoAudio, false},
		{"bot check", botCheck, songs.ErrBotCheck, false},
		{"age check", ageCheck, nil, false},
	}
	for _, tt := range tests {
		yt, _ := newFakeAPI(t, tt.player)
		_, err := yt.Lookup(context.Background(), "LDY_XyxBu8A")
		if err == nil {
			t.Errorf("%s: no error", tt.name)
			continue
		}
		if tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("%s: error = %v, want %v", tt.name, err, tt.want)
		}
		// Only a video that's gone stops Finder from trying yt-dlp.
		if errors.Is(err, songs.ErrUnplayable) != tt.unplayable {
			t.Errorf("%s: errors.Is(%v, ErrUnplayable) = %v", tt.name, err, !tt.unplayable)
		}
	}
}

func TestBestAudio(t *testing.T) {
	original := format{MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 130000, URL: "original"}
	formats := []format{
		{MimeType: `video/mp4; codecs="avc1"`, Bitrate: 5000000, URL: "video"},
		{MimeType: `audio/mp4; codecs="mp4a.40.2"`, Bitrate: 140000, URL: "even loudness", IsDrc: true},
		{MimeType: `audio/webm; codecs="opus"`, Bitrate: 150000, URL: "dub", AudioTrack: &struct {
			AudioIsDefault bool `json:"audioIsDefault"`
		}{false}},
		{MimeType: `audio/webm; codecs="opus"`, Bitrate: 160000},
		original,
	}
	if got, ok := bestAudio(formats); !ok || got.URL != "original" {
		t.Errorf("bestAudio = %q, %v; want the original language and loudness", got.URL, ok)
	}
}

func TestVisitorID(t *testing.T) {
	yt, api := newFakeAPI(t, playerSnooze)
	for range 3 {
		if _, err := yt.Lookup(context.Background(), "LDY_XyxBu8A"); err != nil {
			t.Fatal(err)
		}
	}
	if n := api.requests("/visitor_id"); n != 1 {
		t.Errorf("visitor ID fetched %d times for 3 songs, want once", n)
	}
	if c := api.client("/visitor_id"); c != "ANDROID" {
		t.Errorf("visitor ID clientName = %v, want ANDROID", c)
	}
}

func TestTouch(t *testing.T) {
	yt, api := newFakeAPI(t, playerSnooze)
	yt.Touch(context.Background())
	// It knocks on both APIs' doors, and asks for no song or visitor ID.
	if api.requests("/") != 1 || api.requests("/music/") != 1 || api.requests("/player") != 0 || api.requests("/visitor_id") != 0 {
		t.Errorf("asked %v, want only the API's door", api.count)
	}
}

func TestVisitorIDRenewsInBackground(t *testing.T) {
	yt, api := newFakeAPI(t, playerSnooze)
	if _, err := yt.Lookup(context.Background(), "LDY_XyxBu8A"); err != nil {
		t.Fatal(err)
	}
	// Ten minutes later, YouTube is slow to hand out a new one.
	release := make(chan struct{})
	api.mu.Lock()
	api.visitorWait = release
	api.mu.Unlock()
	yt.mu.Lock()
	yt.visitorAt = time.Now().Add(-visitorFor - time.Second)
	yt.mu.Unlock()

	looked := make(chan error, 1)
	go func() {
		_, err := yt.Lookup(context.Background(), "LDY_XyxBu8A")
		looked <- err
	}()
	select {
	case err := <-looked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the song waited for a new visitor ID")
	}
	if v := api.header("/player").Get("X-Goog-Visitor-Id"); v != "CgtWaXNpdG9yMTIz" {
		t.Errorf("visitor header = %q, want the one before while the new one comes", v)
	}

	close(release)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		yt.mu.Lock()
		renewed := !yt.renewing && time.Since(yt.visitorAt) < visitorFor
		yt.mu.Unlock()
		if renewed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the new visitor ID never came")
		}
	}
	if n := api.requests("/visitor_id"); n != 2 {
		t.Errorf("visitor ID fetched %d times, want twice", n)
	}
}

func TestVisitorIDFailing(t *testing.T) {
	yt, api := newFakeAPI(t, playerSnooze)
	api.visitor = http.StatusInternalServerError
	for range 2 {
		if _, err := yt.Lookup(context.Background(), "LDY_XyxBu8A"); err != nil {
			t.Fatalf("lookup without a visitor ID: %v", err)
		}
	}
	if v := api.header("/player").Get("X-Goog-Visitor-Id"); v != "" {
		t.Errorf("visitor header = %q, want none", v)
	}
	if n := api.requests("/visitor_id"); n != 1 {
		t.Errorf("visitor ID tried %d times, want once until 10 minutes pass", n)
	}
}

func TestSearch(t *testing.T) {
	yt, api := newFakeAPI(t, playerSnooze)
	found, err := yt.Search(context.Background(), "sza snooze")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("got %d songs, want 2", len(found))
	}
	s := found[0]
	if s.ID != "ZqSlV5LmrTg" || s.Title != "Snooze" || s.Artist != "SZA" || s.Length != 202*time.Second {
		t.Errorf("first result = %+v, want Snooze by SZA, 3:22", s)
	}
	if !strings.HasSuffix(s.Thumbnail, "=w120-h120-l90-rj") {
		t.Errorf("Thumbnail = %q, want the 120 px one", s.Thumbnail)
	}
	if s.Audio.URL != "" {
		t.Errorf("a search result has audio %q, want none until it's looked up", s.Audio.URL)
	}
	if found[1].ID != "MtUefXEQRAY" {
		t.Errorf("second result ID = %q", found[1].ID)
	}
	if c := api.client("/music/search"); c != "WEB_REMIX" {
		t.Errorf("clientName = %v, want WEB_REMIX", c)
	}
	if q := api.body("/music/search")["query"]; q != "sza snooze" {
		t.Errorf("query = %v", q)
	}
}

func TestWarm(t *testing.T) {
	yt, api := newFakeAPI(t, playerSnooze)
	if err := yt.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}
	if id := api.body("/player")["videoId"]; id != warmVideo {
		t.Errorf("warmed up with %v, want %s", id, warmVideo)
	}
	h := api.header("/videoplayback")
	if h.Get("Range") != "bytes=0-0" || h.Get("User-Agent") != iosAgent {
		t.Errorf("audio request headers = %v, want one byte as the iPhone app", h)
	}
}

func TestParseLength(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"3:22", 202 * time.Second},
		{"0:07", 7 * time.Second},
		{"1:02:03", time.Hour + 2*time.Minute + 3*time.Second},
		{"SOS", 0},
		{"3", 0},
		{"1:-2", 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got := parseLength(tt.in); got != tt.want {
			t.Errorf("parseLength(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestArtistName(t *testing.T) {
	tests := map[string]string{
		"SZAVEVO":     "SZA",
		"SZA - Topic": "SZA",
		"SZA":         "SZA",
		"VEVO":        "VEVO",
		"":            "",
	}
	for in, want := range tests {
		if got := artistName(in); got != want {
			t.Errorf("artistName(%q) = %q, want %q", in, got, want)
		}
	}
}
