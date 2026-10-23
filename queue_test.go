package lullaby

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby/internal/audio"
	"github.com/windrunner21/lullaby/internal/sources"
	"github.com/windrunner21/lullaby/songs"
)

func TestQueueOrder(t *testing.T) {
	p := &player{frames: &frames{}}
	e := &Engine{players: map[snowflake.ID]*player{1: p}}
	song := func(title string) *Track { return &Track{song: songs.Song{Title: title, Artist: "SZA"}} }

	for i, title := range []string{"Snooze", "Kill Bill", "Saturn"} {
		if place, err := e.insert(p, song(title), false); err != nil || place != i+1 {
			t.Fatalf("%s went in at %d (%v), want %d", title, place, err, i+1)
		}
	}
	if place, _ := e.insert(p, song("Good Days"), true); place != 1 {
		t.Errorf("a song asked for next went in at %d, want 1", place)
	}
	var order []string
	for _, q := range p.queue {
		order = append(order, q.song.Title)
	}
	if got := strings.Join(order, ", "); got != "Good Days, Snooze, Kill Bill, Saturn" {
		t.Errorf("queue = %s", got)
	}
	if next, more, ok := e.UpNext(1); !ok || next.Title != "Good Days" || more != 3 {
		t.Errorf("up next: %q (+%d), want Good Days and 3 more", next.Title, more)
	}

	for len(p.queue) < MaxQueue {
		p.queue = append(p.queue, song("filler"))
	}
	if _, err := e.insert(p, song("one too many"), false); !errors.Is(err, ErrFull) {
		t.Errorf("a song past %d: %v, want ErrFull", MaxQueue, err)
	}
}

func TestSongsRunOutInOrder(t *testing.T) {
	// The song after the one that ran out fails at once, 20 ms later. Its
	// bookkeeping mustn't come first, or the player would think it's still
	// playing, with nothing left to move it on.
	a, b := endedStream(t), endedStream(t)
	p := &player{
		playing: &Track{song: songs.Song{Title: "A"}, stream: a},
		queue:   []*Track{{song: songs.Song{Title: "B"}, stream: b}},
		idle:    time.NewTimer(time.Hour),
	}
	defer p.idle.Stop()
	e := &Engine{ctx: context.Background(), players: map[snowflake.ID]*player{1: p}, events: app{}}
	var order []string
	var mu sync.Mutex
	told := make(chan struct{}, 2)
	p.frames = &frames{hold: func() bool { return false }, moved: func(from, to opus) {
		if from == opus(a) {
			time.Sleep(50 * time.Millisecond) // a slow app, say
		}
		mu.Lock()
		order = append(order, map[opus]string{a: "A", b: "B"}[from])
		mu.Unlock()
		e.moved(1, from, to)
		told <- struct{}{}
	}}
	p.frames.play(a, nil)
	p.frames.queue(b)
	p.frames.ProvideOpusFrame() // A runs out, B takes over
	p.frames.ProvideOpusFrame() // B runs out
	for range 2 {
		select {
		case <-told:
		case <-time.After(5 * time.Second):
			t.Fatal("moved was never called")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, " ") != "A B" {
		t.Errorf("moved for %q, want A then B", order)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if p.playing != nil {
		t.Errorf("the player thinks %q is playing, with nothing playing", p.playing.song.Title)
	}
}

func TestStoppedSongStaysOut(t *testing.T) {
	// A song is being looked up while one plays. The music is stopped, then a
	// song is asked for in another channel: the one asked for before stays out.
	looking, found := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/player") {
			return answer200(`{}`), nil
		}
		close(looking)
		<-found
		return answer200(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"title":"Old","lengthSeconds":"60"},` +
			`"streamingData":{"expiresInSeconds":"21600","adaptiveFormats":[{"mimeType":"audio/mp4","bitrate":128000,"url":"https://audio.invalid/a"}]}}`), nil
	})}
	yt := sources.NewYouTube(client, "https://player.invalid", "https://music.invalid")
	e := &Engine{ctx: context.Background(), players: map[snowflake.ID]*player{1: {playing: &Track{}}},
		leaving: make(map[snowflake.ID]chan struct{}), finder: sources.NewFinder(yt, nil, nil, nil), events: app{}}
	result := make(chan error, 1)
	go func() {
		_, err := e.start(context.Background(), 1, 100, songs.WatchLink("aaaaaaaaaaa"), &Track{}, false)
		result <- err
	}()
	<-looking
	e.take(1, func(*player) bool { return true })
	replacement := &player{channelID: 200, playing: &Track{}, frames: &frames{}}
	e.mu.Lock()
	e.players[1] = replacement
	e.mu.Unlock()
	close(found)
	select {
	case err := <-result:
		if err != ErrStopped {
			t.Errorf("start = %v, want ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start never finished")
	}
	if len(replacement.queue) != 0 {
		t.Error("the song from before the stop went in the new queue")
	}
}

func TestNextKeepsTheSongThatTookOver(t *testing.T) {
	// frames took over with B a moment before moved takes B out of the
	// queue, and a song asked for next comes in between.
	a, b := endedStream(t), bufferedStream(t, silentEncoder(t))
	fr := &frames{hold: func() bool { return false }}
	fr.play(b, nil)
	p := &player{playing: &Track{stream: a}, queue: []*Track{{song: songs.Song{Title: "B"}, stream: b}}, frames: fr}
	e := &Engine{}
	e.mu.Lock()
	_, err := e.insert(p, &Track{song: songs.Song{Title: "C"}}, true)
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ProvideOpusFrame(); err == io.EOF {
		t.Fatal("asking for C next closed B, which was playing already")
	}
}

func TestHandoffUnpauses(t *testing.T) {
	// Paused a moment after B took over: B plays, and the app and the
	// player agree.
	a, b := endedStream(t), bufferedStream(t, silentEncoder(t))
	p := &player{playing: &Track{song: songs.Song{Title: "A"}, stream: a}, queue: []*Track{{song: songs.Song{Title: "B"}, stream: b}},
		frames: &frames{hold: func() bool { return false }}, paused: true, idle: time.NewTimer(time.Hour)}
	defer p.idle.Stop()
	p.frames.pause(true)
	e := &Engine{ctx: context.Background(), players: map[snowflake.ID]*player{1: p}, events: app{}}
	e.moved(1, a, b)
	e.mu.Lock()
	playing, paused := p.playing, p.paused
	e.mu.Unlock()
	p.frames.mu.Lock()
	framesPaused := p.frames.paused
	p.frames.mu.Unlock()
	if playing == nil || playing.song.Title != "B" || paused || framesPaused {
		t.Errorf("playing %v, paused %v, frames paused %v; want B playing, not paused", playing, paused, framesPaused)
	}
}

func TestSkipWhileTheAppIsSlow(t *testing.T) {
	encoder := silentEncoder(t)
	a, b := endedStream(t), bufferedStream(t, encoder)
	// The app takes its time with A being over, like a card edit waiting in
	// Discord's rate limit.
	ending, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := app{ended: func(*Track, End) {
		once.Do(func() { close(ending) })
		<-release
	}}
	p := &player{playing: &Track{song: songs.Song{Title: "A"}, stream: a}, queue: []*Track{
		{song: songs.Song{Title: "B"}, stream: b},
		{song: songs.Song{Title: "C", Audio: songs.Audio{URL: "https://audio.invalid/C"}}},
	}, idle: time.NewTimer(time.Hour)}
	defer p.idle.Stop()
	e := &Engine{ctx: context.Background(), ffmpeg: encoder, events: slow, players: map[snowflake.ID]*player{1: p},
		http: &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) { return answer200("input"), nil })}}
	p.frames = &frames{hold: func() bool { return false }, moved: func(from, to opus) { e.moved(1, from, to) }}
	p.frames.play(a, nil)
	p.frames.queue(b)
	p.frames.ProvideOpusFrame()
	select {
	case <-ending:
	case <-time.After(time.Second):
		t.Fatal("the app wasn't told A is over")
	}
	// A's move has prepared C, but the app is still busy with A.
	var c *audio.Stream
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		e.mu.Lock()
		c = p.queue[0].stream
		e.mu.Unlock()
		if c != nil {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("C not prepared")
		}
	}
	t.Cleanup(c.Close)
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		close(release)
		t.Fatal("C encoder stuck")
	}
	e.Skip(1, 3)
	if packet, _ := p.frames.ProvideOpusFrame(); len(packet) == 0 {
		close(release)
		t.Fatal("C did not take over")
	}
	e.Skip(1, 3) // C is skipped at once too, while the app is still busy with A
	packet, _ := p.frames.ProvideOpusFrame()
	close(release)
	if len(packet) != 0 {
		t.Fatal("the second skip closed B again, so C kept playing: the app held up the handover")
	}
}

// slowPlaylist is a client whose playlist at /song.m3u8 waits for release,
// and says when it's asked for in entered. playlists counts them.
func slowPlaylist(entered, release chan struct{}, playlists *atomic.Int32) *http.Client {
	return &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/song.m3u8" {
			playlists.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return answer200("#EXTM3U\n#EXTINF:1,\npart\n#EXT-X-ENDLIST\n"), nil
		}
		return answer200("input"), nil
	})}
}

func TestPrepareOnce(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var playlists atomic.Int32
	p := &player{frames: &frames{}, queue: []*Track{{song: songs.Song{Title: "next", Audio: songs.Audio{URL: "https://audio.invalid/song.m3u8", Playlist: true}}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e := &Engine{ctx: ctx, ffmpeg: silentEncoder(t), http: slowPlaylist(entered, release, &playlists), events: app{}, players: map[snowflake.ID]*player{1: p}}
	done := make(chan struct{}, 2)
	go func() { e.prepare(1); done <- struct{}{} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first preparation did not start")
	}
	go func() { e.prepare(1); done <- struct{}{} }()
	completed := 0
	select {
	case <-entered:
	case <-done:
		completed++
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for ; completed < 2; completed++ {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("preparation stuck")
		}
	}
	e.mu.Lock()
	stream := p.queue[0].stream
	e.mu.Unlock()
	if stream != nil {
		stream.Close()
	}
	if n := playlists.Load(); n != 1 {
		t.Fatalf("two songs in the queue started %d playlist fetches and ffmpeg processes for the same next song", n)
	}
}

func TestAdvanceWaitsForThePreparation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var playlists atomic.Int32
	p := &player{conn: &quietConn{}, frames: &frames{}, idle: time.NewTimer(time.Hour), queue: []*Track{{song: songs.Song{Title: "B", Audio: songs.Audio{URL: "https://audio.invalid/song.m3u8", Playlist: true}}}}}
	defer p.idle.Stop()
	e := &Engine{ctx: ctx, ffmpeg: silentEncoder(t), http: slowPlaylist(entered, release, &playlists), events: app{}, players: map[snowflake.ID]*player{1: p}}
	prepared, advanced := make(chan struct{}), make(chan struct{})
	go func() { e.prepare(1); close(prepared) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("preparation did not start")
	}
	// A has ended while B's playlist is still arriving. afterMove starts advance.
	go func() { e.advance(1); close(advanced) }()
	select {
	case <-entered:
	case <-time.After(150 * time.Millisecond):
	}
	unblock()
	for _, done := range []chan struct{}{prepared, advanced} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("preparation or advance stuck")
		}
	}
	e.mu.Lock()
	if p.playing != nil {
		p.playing.stream.Close()
	}
	e.mu.Unlock()
	if n := playlists.Load(); n != 1 {
		t.Fatalf("started the same song %d times", n)
	}
}

func TestAdvancePreparesBeforeTheApp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The app takes its time to show a song that started, like a card
	// waiting in Discord's rate limit.
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	slow := app{started: func(*Track) {
		close(entered)
		<-release
	}}
	fetched := make(chan struct{}, 1)
	client := &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) {
		select {
		case fetched <- struct{}{}:
		default:
		}
		return answer200("input"), nil
	})}
	b := bufferedStream(t, silentEncoder(t))
	p := &player{conn: &quietConn{}, frames: &frames{}, idle: time.NewTimer(time.Hour), queue: []*Track{
		{song: songs.Song{Title: "B"}, stream: b},
		{song: songs.Song{Title: "C", Audio: songs.Audio{URL: "https://audio.invalid/C"}}},
	}}
	defer p.idle.Stop()
	e := &Engine{ctx: ctx, ffmpeg: silentEncoder(t), http: client, events: slow, players: map[snowflake.ID]*player{1: p}}
	done := make(chan struct{})
	go func() { e.advance(1); close(done) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the app wasn't told B started")
	}
	ready := false
	select {
	case <-fetched:
		ready = true
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("advance stuck")
	}
	e.mu.Lock()
	if len(p.queue) != 0 && p.queue[0].stream != nil {
		p.queue[0].stream.Close()
	}
	e.mu.Unlock()
	if !ready {
		t.Fatal("the song after waited for the app")
	}
}
