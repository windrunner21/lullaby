package lullaby

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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

func TestAutoplayWaitsBehind(t *testing.T) {
	p := &player{frames: &frames{}}
	e := &Engine{players: map[snowflake.ID]*player{1: p}}
	song := func(title string, autoplay bool) *Track {
		return &Track{song: songs.Song{Title: title}, autoplay: autoplay}
	}
	for _, add := range []struct {
		t     *Track
		next  bool
		place int
	}{
		{song("Snooze", false), false, 1},
		{song("picked", true), false, 2},
		{song("Kill Bill", false), false, 2}, // ahead of the pick
		{song("Good Days", false), true, 1},
		{song("picked too", true), false, 5},
		{song("Saturn", false), false, 4},
	} {
		if place, err := e.insert(p, add.t, add.next); err != nil || place != add.place {
			t.Errorf("%s went in at %d (%v), want %d", add.t.song.Title, place, err, add.place)
		}
	}
	var order []string
	for _, q := range p.queue {
		order = append(order, q.song.Title)
	}
	if got := strings.Join(order, ", "); got != "Good Days, Snooze, Kill Bill, Saturn, picked, picked too" {
		t.Errorf("queue = %s, want the songs asked for ahead of the picks", got)
	}
	if next, more, ok := e.UpNextTrack(1); !ok || next != p.queue[0] || more != 5 || next.Autoplay() {
		t.Errorf("UpNextTrack = %v (+%d), %v; want Good Days's Track and 5 more", next, more, ok)
	}
}

func TestPicksDontJoinVoice(t *testing.T) {
	// A song the app chose by itself plays in the player in voice, at once
	// if nothing plays there, and never makes the bot join.
	client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/player") {
			return answer200(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"title":"Me & U","lengthSeconds":"193"},` +
				`"streamingData":{"expiresInSeconds":"21600","adaptiveFormats":[{"mimeType":"audio/mp4","bitrate":128000,"url":"https://audio.invalid/a"}]}}`), nil
		}
		return answer200("audio"), nil
	})}
	e := offlineEngine(t, client, silentEncoder(t), "")
	pick := Request{GuildID: 1, ChannelID: 100, Query: songs.WatchLink("UPeS_V1pVZk"), Autoplay: true}

	if _, _, err := e.Play(context.Background(), pick); !errors.Is(err, ErrStopped) {
		t.Errorf("a pick with nobody in voice: %v, want ErrStopped", err)
	}
	if _, ok := e.Channel(1); ok {
		t.Fatal("a pick made the bot join voice")
	}

	if _, err := e.join(context.Background(), 1, 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	tr, place, err := e.Play(context.Background(), pick)
	if err != nil || place != 0 {
		t.Fatalf("a pick in a quiet player went in at %d, %v; want it playing", place, err)
	}
	if now, _ := e.Playing(1); now != tr || !tr.Autoplay() {
		t.Error("the pick isn't playing, or doesn't say it's a pick")
	}
}

func TestPickWaitsBehindALateHandover(t *testing.T) {
	// Nothing plays, since the next song wasn't ready in time, and a pick
	// comes: it waits behind that song rather than starting ahead of it.
	e := &Engine{ctx: context.Background(), players: map[snowflake.ID]*player{}, events: app{}}
	encoder := silentEncoder(t)
	waiting := &Track{song: songs.Song{Title: "asked for"}, stream: bufferedStream(t, encoder)} // ready, so nothing is looked up
	p := &player{conn: &quietConn{}, frames: &frames{}, queue: []*Track{waiting}, idle: time.NewTimer(time.Hour)}
	defer p.idle.Stop()
	e.players[1] = p
	pick := &Track{song: songs.Song{Title: "picked"}, autoplay: true, stream: bufferedStream(t, encoder)}
	e.mu.Lock()
	place, err := e.pickLocked(context.Background(), 1, p, pick, false)
	e.mu.Unlock()
	if err != nil || place != 2 {
		t.Errorf("the pick went in at %d, %v; want it behind the song waiting", place, err)
	}
	if queue := e.Queue(1); len(queue) != 2 || queue[0] != waiting || queue[1] != pick {
		t.Errorf("Queue = %v, want the song asked for, then the pick", queue)
	}
	if e.Queue(2) != nil {
		t.Error("a server with no player has a queue")
	}
}

func TestPickCalledOff(t *testing.T) {
	// A pick's context is done by the time it would start: it doesn't
	// start, nor go in line.
	e := &Engine{ctx: context.Background(), players: map[snowflake.ID]*player{}, events: app{}}
	p := &player{conn: &quietConn{}, frames: &frames{}, idle: time.NewTimer(time.Hour)}
	defer p.idle.Stop()
	e.players[1] = p
	off, cancel := context.WithCancel(context.Background())
	cancel()
	pick := &Track{song: songs.Song{Title: "picked"}, autoplay: true, stream: bufferedStream(t, silentEncoder(t))}
	e.mu.Lock()
	_, err := e.pickLocked(off, 1, p, pick, false)
	playing, waiting := p.playing, len(p.queue)
	e.mu.Unlock()
	if !errors.Is(err, context.Canceled) || playing != nil || waiting != 0 {
		t.Errorf("a pick called off: %v, playing %v, %d waiting; want it nowhere", err, playing, waiting)
	}
}

func TestPickBehindWaitsWithoutAStream(t *testing.T) {
	// A pick that goes in behind a song waiting doesn't start its ffmpeg
	// and download yet, only to have them closed: it gets ready later. So
	// it goes in even with its ffmpeg gone.
	client := &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/player") {
			return answer200(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"title":"Me & U","lengthSeconds":"193"},` +
				`"streamingData":{"expiresInSeconds":"21600","adaptiveFormats":[{"mimeType":"audio/mp4","bitrate":128000,"url":"https://audio.invalid/pick"}]}}`), nil
		}
		return answer200("audio"), nil
	})}
	ffmpeg := silentEncoder(t)
	e := offlineEngine(t, client, ffmpeg, "")
	p, err := e.join(context.Background(), 1, 100, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	encoder := silentEncoder(t)
	e.mu.Lock()
	p.playing = &Track{stream: bufferedStream(t, encoder)}
	p.queue = []*Track{{song: songs.Song{Title: "asked for"}, stream: bufferedStream(t, encoder)}}
	e.mu.Unlock()
	if err := os.Remove(ffmpeg); err != nil {
		t.Fatal(err)
	}
	pick, place, err := e.Play(context.Background(), Request{GuildID: 1, ChannelID: 100, Query: songs.WatchLink("UPeS_V1pVZk"), Autoplay: true})
	if err != nil || place != 2 {
		t.Fatalf("the pick went in at %d, %v; want it behind the song waiting, without its ffmpeg", place, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if pick.stream != nil {
		t.Error("a pick behind a song waiting started its stream")
	}
}

func TestRemove(t *testing.T) {
	// A pick waits ready in line, behind the song playing, and is removed:
	// the song after it gets ready instead, and takes over.
	encoder := silentEncoder(t)
	playing, pickStream := endedStream(t), bufferedStream(t, encoder)
	picked := &Track{song: songs.Song{Title: "picked"}, autoplay: true, stream: pickStream}
	after := &Track{song: songs.Song{Title: "after", Audio: songs.Audio{URL: "https://audio.invalid/after"}}}
	p := &player{playing: &Track{stream: playing}, queue: []*Track{picked, after}, idle: time.NewTimer(time.Hour)}
	defer p.idle.Stop()
	e := &Engine{players: map[snowflake.ID]*player{1: p}, ffmpeg: encoder, events: app{},
		http: &http.Client{Transport: fakeTransport(func(*http.Request) (*http.Response, error) { return answer200("audio"), nil })}}
	p.frames = &frames{hold: func() bool { return false }, moved: func(from, to opus) { e.moved(1, from, to) }}
	p.frames.play(playing, nil)
	p.frames.queue(pickStream)

	if !e.Remove(1, picked) {
		t.Fatal("the pick waiting in line wasn't removed")
	}
	if _, err := pickStream.ProvideOpusFrame(); err != io.EOF {
		t.Errorf("the removed pick's ffmpeg is still going: %v", err)
	}
	var ready *audio.Stream
	for start := time.Now(); ready == nil; {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the song after the pick never got ready")
		}
		time.Sleep(time.Millisecond)
		e.mu.Lock()
		ready = after.stream
		e.mu.Unlock()
	}
	<-ready.Done()
	if frame, _ := p.frames.ProvideOpusFrame(); len(frame) == 0 {
		t.Error("the song after the pick didn't take over")
	}
	if now, ok := e.Playing(1); !ok || now != after {
		t.Errorf("playing %v, want the song after the pick", now)
	}
	if e.Remove(1, picked) || e.Remove(2, after) || e.Remove(1, &Track{}) {
		t.Error("removed a song that isn't waiting in that server's queue")
	}
}

func TestRemoveLeavesTheSongThatTookOver(t *testing.T) {
	// frames took over with the pick a moment before moved takes it out of
	// the queue: it's playing, so it stays.
	picked := &Track{song: songs.Song{Title: "picked"}, autoplay: true, stream: bufferedStream(t, silentEncoder(t))}
	fr := &frames{hold: func() bool { return false }}
	fr.play(picked.stream, nil)
	p := &player{playing: &Track{stream: endedStream(t)}, queue: []*Track{picked}, frames: fr}
	e := &Engine{players: map[snowflake.ID]*player{1: p}}
	if e.Remove(1, picked) {
		t.Error("removed the song that took over")
	}
	if _, err := picked.stream.ProvideOpusFrame(); err == io.EOF {
		t.Error("Remove closed the song that took over")
	}
}

// closingAudio is audio whose Close waits for release, after entered is
// closed: the moment a stream is being closed, but isn't gone yet.
type closingAudio struct {
	entered, release, end chan struct{}
}

func (a *closingAudio) Read([]byte) (int, error) {
	<-a.end
	return 0, io.EOF
}

func (a *closingAudio) Close() error {
	close(a.entered)
	<-a.release
	close(a.end)
	return nil
}

func TestAskingAheadOfAPickTakingOver(t *testing.T) {
	// A song is asked for while the pick waits ready in line, and the song
	// playing runs out while insert closes the pick's stream: the pick
	// can't take over with a stream that's being closed.
	encoder := silentEncoder(t)
	playing, pickStream := endedStream(t), bufferedStream(t, encoder)
	closing := &closingAudio{entered: make(chan struct{}), release: make(chan struct{}), end: make(chan struct{})}
	release := sync.OnceFunc(func() { close(closing.release) })
	defer release()
	pickStream.Feed(closing)
	picked := &Track{song: songs.Song{Title: "picked"}, autoplay: true, stream: pickStream}
	life, cancel := context.WithCancel(context.Background())
	cancel() // so nothing is prepared after, which isn't what's tested
	p := &player{playing: &Track{stream: playing}, queue: []*Track{picked}, idle: time.NewTimer(time.Hour)}
	defer p.idle.Stop()
	e := &Engine{ctx: life, players: map[snowflake.ID]*player{1: p}, events: app{}}
	ranOut := make(chan struct{})
	p.frames = &frames{hold: func() bool { return false }, moved: func(from, to opus) {
		close(ranOut) // before moved waits for insert to let go of the lock
		e.moved(1, from, to)
	}}
	p.frames.play(playing, nil)
	p.frames.queue(pickStream)

	inserted, sent := make(chan struct{}), make(chan struct{})
	go func() {
		e.mu.Lock()
		e.insert(p, &Track{song: songs.Song{Title: "asked for"}}, false)
		e.mu.Unlock()
		close(inserted)
	}()
	<-closing.entered
	go func() {
		p.frames.ProvideOpusFrame() // the song playing runs out now
		close(sent)
	}()
	<-ranOut
	release()
	<-inserted
	<-sent
	if p.frames.current() == opus(pickStream) {
		t.Error("the pick took over with its stream while insert closed it")
	}
}

// slowSong is a song whose playlist the sites answer once release is
// called, after slow is closed, and an HTTP client for it and for songs that
// come at once.
func slowSong(t *testing.T) (song songs.Song, client *http.Client, slow chan struct{}, release func()) {
	slow, released := make(chan struct{}), make(chan struct{})
	release = sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	client = &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/slow.m3u8" {
			close(slow)
			select {
			case <-released:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return answer200("#EXTM3U\n#EXTINF:1,\npart\n#EXT-X-ENDLIST\n"), nil
		}
		return answer200("audio"), nil
	})}
	return songs.Song{Title: "slow", Audio: songs.Audio{URL: "https://audio.invalid/slow.m3u8", Playlist: true}}, client, slow, release
}

// advanceWaiting waits until an advance waits for p's song next in line.
func advanceWaiting(t *testing.T, e *Engine, p *player) {
	t.Helper()
	for start := time.Now(); ; time.Sleep(time.Millisecond) {
		e.mu.Lock()
		waits := p.changed != nil
		e.mu.Unlock()
		if waits {
			return
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("advance never waited")
		}
	}
}

func TestRemoveStopsAdvanceWaiting(t *testing.T) {
	// The song playing ran out before the pick was ready, so advance waits
	// for the pick's slow preparation. The pick is removed: advance plays
	// the song after it once that's ready, without waiting for the pick.
	life, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	song, client, slow, _ := slowSong(t)
	picked := &Track{autoplay: true, song: song}
	after := &Track{song: songs.Song{Title: "after", Audio: songs.Audio{URL: "https://audio.invalid/after"}}}
	p := &player{conn: &quietConn{}, frames: &frames{hold: func() bool { return false }}, idle: time.NewTimer(time.Hour), queue: []*Track{picked, after}}
	defer p.idle.Stop()
	e := &Engine{ctx: life, players: map[snowflake.ID]*player{1: p}, ffmpeg: silentEncoder(t), http: client, events: app{}}
	go e.prepare(1)
	<-slow
	advanced := make(chan struct{})
	go func() {
		e.advance(1)
		close(advanced)
	}()
	advanceWaiting(t, e, p)
	if !e.Remove(1, picked) {
		t.Fatal("the pick wasn't removed")
	}
	select {
	case <-advanced:
	case <-time.After(5 * time.Second):
		t.Fatal("advance still waits for the pick's preparation")
	}
	e.mu.Lock()
	now := p.playing
	e.mu.Unlock()
	if now != after {
		t.Errorf("playing %v, want the song after the pick", now)
	}
}

func TestNextWhileAdvanceWaits(t *testing.T) {
	// advance waits for a slow song, and a song that's ready is asked for
	// next: it plays at once.
	life, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	song, client, slow, _ := slowSong(t)
	p := &player{conn: &quietConn{}, frames: &frames{hold: func() bool { return false }}, idle: time.NewTimer(time.Hour), queue: []*Track{{song: song}}}
	defer p.idle.Stop()
	e := &Engine{ctx: life, players: map[snowflake.ID]*player{1: p}, ffmpeg: silentEncoder(t), http: client, events: app{}}
	go e.prepare(1)
	<-slow
	advanced := make(chan struct{})
	go func() {
		e.advance(1)
		close(advanced)
	}()
	advanceWaiting(t, e, p)
	next := &Track{song: songs.Song{Title: "next"}, stream: bufferedStream(t, silentEncoder(t))}
	e.mu.Lock()
	e.insert(p, next, true)
	e.mu.Unlock()
	select {
	case <-advanced:
	case <-time.After(5 * time.Second):
		t.Fatal("advance still waits for the slow song")
	}
	if now, _ := e.Playing(1); now != next {
		t.Errorf("playing %v, want the song asked for next", now)
	}
}

func TestOneAdvanceAtATime(t *testing.T) {
	// One advance opens a slow song it took from the queue, and another one
	// is asked for meanwhile: the song after the slow one doesn't start
	// first, and once the slow one plays, nothing else is started.
	life, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	song, client, slow, release := slowSong(t)
	first := &Track{song: song}
	after := &Track{song: songs.Song{Title: "after", Audio: songs.Audio{URL: "https://audio.invalid/after"}}}
	p := &player{conn: &quietConn{}, frames: &frames{}, idle: time.NewTimer(time.Hour), queue: []*Track{first, after}}
	defer p.idle.Stop()
	e := &Engine{ctx: life, ffmpeg: silentEncoder(t), http: client, players: map[snowflake.ID]*player{1: p}, events: app{}}
	done := make(chan struct{})
	go func() {
		e.advance(1)
		close(done)
	}()
	<-slow
	e.advance(1) // returns at once, leaving it to the one at work
	if now, _ := e.Playing(1); now == after {
		t.Fatal("the song after the slow one started first")
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("advance never finished")
	}
	e.mu.Lock()
	now, waiting := p.playing, len(p.queue)
	for _, s := range []*audio.Stream{first.stream, after.stream} {
		if s != nil {
			s.Close()
		}
	}
	e.mu.Unlock()
	if now != first || waiting != 1 {
		t.Errorf("playing %v with %d in line, want the slow song, then the one after it waiting", now, waiting)
	}
}

// joinedPlayer is an engine whose sites answer through client, and its
// player in voice channel 100 of server 1.
func joinedPlayer(t *testing.T, client *http.Client) (*Engine, *player) {
	t.Helper()
	e := offlineEngine(t, client, silentEncoder(t), "")
	life, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	e.ctx = life
	context.AfterFunc(life, e.stopAll)
	p, err := e.join(life, 1, 100, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p.frames.hold = func() bool { return false }
	return e, p
}

// eventually waits until ok, for a few seconds at most.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for start := time.Now(); !ok(); time.Sleep(time.Millisecond) {
		if time.Since(start) > 5*time.Second {
			t.Fatal("never: " + what)
		}
	}
}

func TestNothingGetsAheadOfASongGettingReady(t *testing.T) {
	// advance gets a slow song ready, first in line. Meanwhile a song asked
	// for plays at once, another goes in the queue, and the first is
	// skipped: the song behind the slow one doesn't get ready and take over
	// ahead of it, and the slow one plays once it's ready.
	slow, client, entered, release := slowSong(t)
	dAsked := make(chan struct{})
	answer := client.Transport
	client.Transport = fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/D" {
			close(dAsked)
		}
		return answer.RoundTrip(r)
	})
	e, p := joinedPlayer(t, client)
	b := &Track{song: slow}
	d := &Track{song: songs.Song{Title: "D", Audio: songs.Audio{URL: "https://audio.invalid/D"}}}
	e.mu.Lock()
	p.queue = []*Track{b, d}
	e.mu.Unlock()
	go e.advance(1)
	<-entered

	c := &Track{song: songs.Song{Title: "C"}, stream: bufferedStream(t, silentEncoder(t))}
	if place, err := e.begin(1, p, c, false, nil); err != nil || place != 0 {
		t.Fatalf("the song asked for went in at %d, %v; want it playing", place, err)
	}
	if _, err := e.enqueue(1, p, &Track{song: songs.Song{Title: "E", Audio: songs.Audio{URL: "https://audio.invalid/E"}}}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dAsked:
		// D is getting ready ahead of B: let it be, as it would be by the
		// time C ends.
		eventually(t, "D is ready", func() bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			return d.stream != nil
		})
	case <-time.After(200 * time.Millisecond):
	}
	e.Skip(1, 7)
	p.frames.ProvideOpusFrame() // C runs out
	e.mu.Lock()
	gotReady := d.stream != nil
	e.mu.Unlock()
	if now, _ := e.Playing(1); gotReady || now == d {
		t.Fatal("the song behind the slow one got ready, or took over, ahead of it")
	}
	release()
	eventually(t, "the slow song plays", func() bool {
		now, _ := e.Playing(1)
		return now == b
	})
}

func TestSlowAppDoesntHoldUpTheNextSong(t *testing.T) {
	// advance starts a song that ends at once, while the app takes its time
	// to show it, and the song after it is getting ready: that one plays
	// once it's ready, without waiting for the app.
	slow, client, entered, releaseSong := slowSong(t)
	e, p := joinedPlayer(t, client)
	b := &Track{song: songs.Song{Title: "B"}, stream: endedStream(t)}
	c := &Track{song: slow}
	showing, shown := make(chan struct{}), make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(shown) }))
	e.events = app{started: func(t *Track) {
		if t == b {
			close(showing)
			<-shown
		}
	}}
	e.mu.Lock()
	p.queue = []*Track{b, c}
	e.mu.Unlock()
	go e.advance(1)
	<-showing
	<-entered
	p.frames.ProvideOpusFrame() // B ends at once, before C is ready
	releaseSong()
	eventually(t, "the song after it plays while the app is still showing the one before", func() bool {
		now, _ := e.Playing(1)
		return now == c
	})
}

// startedSignal is an app that says when track starts.
func startedSignal(track *Track) (app, chan struct{}) {
	started := make(chan struct{})
	once := sync.OnceFunc(func() { close(started) })
	return app{started: func(t *Track) {
		if t == track {
			once()
		}
	}}, started
}

func TestRemoveWhileAdvanceGetsItReady(t *testing.T) {
	// advance gets the pick ready itself, and the pick is removed meanwhile:
	// the song after it plays once it's ready, without waiting for the pick.
	slow, client, entered, _ := slowSong(t)
	e, p := joinedPlayer(t, client)
	picked := &Track{song: slow, autoplay: true}
	after := &Track{song: songs.Song{Title: "after", Audio: songs.Audio{URL: "https://audio.invalid/after"}}}
	var started chan struct{}
	e.events, started = startedSignal(after)
	e.mu.Lock()
	p.queue = []*Track{picked, after}
	e.mu.Unlock()
	go e.advance(1)
	<-entered
	if !e.Remove(1, picked) {
		t.Fatal("the pick getting ready wasn't removed")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the song after the pick never played")
	}
}

func TestAskedForWhileAdvanceGetsItReady(t *testing.T) {
	// advance gets the pick ready itself. Meanwhile a song asked for plays at
	// once, another goes ahead of the pick, next or simply asked for, and
	// the first ends before that one is ready: it plays once it's ready,
	// without waiting for the pick.
	for _, next := range []bool{false, true} {
		t.Run(fmt.Sprint("next ", next), func(t *testing.T) {
			slow, client, entered, _ := slowSong(t)
			// The song asked for gets ready only once the first has run out,
			// so it's advance that starts it, not the handover.
			askedFor, ranOut := make(chan struct{}), make(chan struct{})
			answer := client.Transport
			client.Transport = fakeTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/asked.m3u8" {
					close(askedFor)
					select {
					case <-ranOut:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return answer200("#EXTM3U\n#EXTINF:1,\npart\n#EXT-X-ENDLIST\n"), nil
				}
				return answer.RoundTrip(r)
			})
			e, p := joinedPlayer(t, client)
			picked := &Track{song: slow, autoplay: true}
			asked := &Track{song: songs.Song{Title: "asked for", Audio: songs.Audio{URL: "https://audio.invalid/asked.m3u8", Playlist: true}}}
			var started chan struct{}
			e.events, started = startedSignal(asked)
			e.mu.Lock()
			p.queue = []*Track{picked}
			e.mu.Unlock()
			go e.advance(1)
			<-entered

			first := &Track{song: songs.Song{Title: "first"}, stream: bufferedStream(t, silentEncoder(t))}
			if place, err := e.begin(1, p, first, false, nil); place != 0 || err != nil {
				t.Fatalf("the first song asked for went in at %d, %v; want it playing", place, err)
			}
			if place, err := e.enqueue(1, p, asked, next); place != 1 || err != nil {
				t.Fatalf("the song asked for went in at %d, %v; want it first in line", place, err)
			}
			select {
			case <-askedFor:
			case <-time.After(5 * time.Second):
				t.Fatal("the song asked for never started getting ready")
			}
			e.Skip(1, 7)
			p.frames.ProvideOpusFrame() // the first runs out before the other is ready
			close(ranOut)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the song asked for never played")
			}
		})
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
	b := &Track{song: songs.Song{Title: "B", Audio: songs.Audio{URL: "https://audio.invalid/song.m3u8", Playlist: true}}}
	p := &player{conn: &quietConn{}, frames: &frames{}, idle: time.NewTimer(time.Hour), queue: []*Track{b}}
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
	select {
	case <-advanced:
		t.Fatal("advance gave up on B before it was ready")
	default:
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
	playing := p.playing
	if playing != nil {
		playing.stream.Close()
	}
	e.mu.Unlock()
	if playing != b {
		t.Errorf("playing %v once B was ready, want B", playing)
	}
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
