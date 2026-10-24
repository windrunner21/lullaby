package lullaby

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby/internal/audio"
	"github.com/windrunner21/lullaby/internal/netguard"
	"github.com/windrunner21/lullaby/internal/redact"
	"github.com/windrunner21/lullaby/songs"
)

// start plays query in a voice channel, or puts it in the queue when a song is
// playing already: at the front with next. It returns the song's place in the
// queue, or 0 when it plays now. It waits until ctx is done at most.
func (e *Engine) start(ctx context.Context, guildID, channelID snowflake.ID, query string, t *Track, next bool) (int, error) {
	if p := e.playingIn(guildID); p != nil {
		song, err := e.findFor(ctx, query)
		if err != nil {
			return 0, err
		}
		t.song = song
		if place, err := e.enqueue(guildID, p, t, next); place > 0 || err != nil {
			return place, err
		}
		// The music ran out while the song was being found, so it plays now.
	}
	return e.playNow(ctx, guildID, channelID, query, t, next)
}

// findFor finds a song for someone who waits until ctx is done at most. The
// lookup goes on with the engine's lifetime even if they stop waiting, since
// others asking for the same song share it.
func (e *Engine) findFor(ctx context.Context, query string) (songs.Song, error) {
	type found struct {
		song songs.Song
		err  error
	}
	done := make(chan found, 1)
	go func() {
		song, err := e.find(e.life(), query)
		done <- found{song, err}
	}()
	select {
	case f := <-done:
		return f.song, f.err
	case <-ctx.Done():
		return songs.Song{}, ctx.Err()
	}
}

// playingIn returns the player in a server if a song is playing there.
func (e *Engine) playingIn(guildID snowflake.ID) *player {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.players[guildID]; ok && p.playing != nil {
		return p
	}
	return nil
}

// playNow joins the voice channel, finds the song and starts it all at once:
// ffmpeg starts while the song is looked up, which took 0.25 s on the Pi. If
// another song beat it to it, it goes in the queue instead. It waits until
// ctx is done at most; the song's ffmpeg and download, and the join, live as
// long as the engine.
func (e *Engine) playNow(ctx context.Context, guildID, channelID snowflake.ID, query string, t *Track, next bool) (int, error) {
	asked := time.Now()
	life := e.life()
	stream, err := e.ffmpegFor(life)
	if err != nil {
		return 0, err
	}
	type joining struct {
		p     *player
		err   error
		after time.Duration
	}
	joined := make(chan joining, 1)
	go func() {
		p, err := e.join(life, guildID, channelID, idleFor)
		joined <- joining{p, err, time.Since(asked)}
	}()

	if t.song.Link == "" {
		if t.song, err = e.findFor(ctx, query); err != nil {
			stream.Close()
			return 0, err
		}
	}
	found := time.Since(asked)
	if stream, err = e.feedFor(ctx, life, stream, t.song); err != nil {
		return 0, err
	}
	t.cover = e.coverAhead(t.song)
	var j joining
	select {
	case j = <-joined:
	case <-ctx.Done():
		// Joining goes on, and the player leaves again if nothing plays.
		stream.Close()
		return 0, ctx.Err()
	}
	if j.err != nil {
		stream.Close()
		return 0, fmt.Errorf("%w: %w", ErrJoin, j.err)
	}

	t.stream = stream
	title, since := t.song.Title, t.asked
	return e.begin(guildID, j.p, t, next, func() {
		args := []any{slog.String("song", title), slog.Duration("found after", found),
			slog.Duration("joined after", j.after), slog.Duration("first frame after", time.Since(asked))}
		if !since.IsZero() {
			args = append(args, slog.Duration("since asked", time.Since(since).Round(time.Millisecond)))
		}
		e.logger().Info("song started", args...)
	})
}

// begin plays t in want now if nothing is playing, or puts it in the queue if
// another song got there first. It returns its place in the queue, or 0 when
// it plays. If the music was stopped meanwhile, so the server's player isn't
// want anymore, the song doesn't play. started is called once its first
// frame goes out.
func (e *Engine) begin(guildID snowflake.ID, want *player, t *Track, next bool, started func()) (int, error) {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok || p != want || p.conn == nil {
		e.mu.Unlock()
		t.stream.Close()
		return 0, ErrStopped
	}
	if p.playing == nil {
		p.playLocked(t, started)
		e.mu.Unlock()
		return 0, nil
	}
	place, err := e.insert(p, t, next)
	e.mu.Unlock()
	if err != nil {
		t.stream.Close()
		return 0, err
	}
	go e.prepare(guildID) // the song next in line, which can take a moment
	return place, nil
}

// enqueue puts t in want's queue, behind the song playing, at the front with
// next, and returns its place. It returns 0 when nothing is playing anymore,
// and ErrStopped when the music was stopped, so the server's player isn't
// want anymore.
func (e *Engine) enqueue(guildID snowflake.ID, want *player, t *Track, next bool) (int, error) {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok || p != want {
		e.mu.Unlock()
		return 0, ErrStopped
	}
	if p.playing == nil {
		e.mu.Unlock()
		return 0, nil
	}
	place, err := e.insert(p, t, next)
	e.mu.Unlock()
	if err == nil {
		go e.prepare(guildID) // the song next in line, which can take a moment
	}
	return place, err
}

// insert puts t in p's queue, at the front with next, and returns its place.
// A song asked for goes ahead of the ones the app chose by itself. Only the
// song next in line keeps its stream started. e.mu must be held.
func (e *Engine) insert(p *player, t *Track, next bool) (int, error) {
	if len(p.queue) >= MaxQueue {
		return 0, ErrFull
	}
	i := len(p.queue)
	switch {
	case next:
		i = 0
	case !t.autoplay:
		if first := slices.IndexFunc(p.queue, (*Track).Autoplay); first >= 0 {
			i = first
		}
	}
	p.queue = slices.Insert(p.queue, i, t)
	place := i + 1
	// frames may have taken over with a song a moment before moved takes it
	// out of the queue: that one plays on. Told apart in the same step that
	// gives frames the new song next in line, so the one before can't take
	// over while its stream is being closed.
	playing := p.frames.requeue(ready(p.queue))
	for _, q := range p.queue[1:] {
		if q.stream != nil && opus(q.stream) != playing {
			q.stream.Close()
			q.stream = nil
		}
	}
	p.queueChangedLocked()
	return place, nil
}

// ready is the stream of the song next in line, for frames to take over with,
// or nil when there's none yet.
func ready(queue []*Track) opus {
	if len(queue) == 0 || queue[0].stream == nil {
		return nil
	}
	return queue[0].stream
}

// prepare starts the song next in line, so it takes over the moment the one
// playing runs out.
func (e *Engine) prepare(guildID snowflake.ID) {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok || e.lifeLocked().Err() != nil || len(p.queue) == 0 || p.queue[0].stream != nil || p.queue[0].preparing != nil {
		e.mu.Unlock()
		return
	}
	t := p.queue[0]
	t.preparing = make(chan struct{})
	song := t.song
	e.mu.Unlock()
	e.getReady(guildID, p, t, song)
}

// getReady starts the stream of t, the song next in line in p, which its
// caller has marked as preparing, and has it take over once the song playing
// runs out, if it's still next in line by then.
func (e *Engine) getReady(guildID snowflake.ID, p *player, t *Track, song songs.Song) {
	ctx := e.life()
	song = e.fresh(ctx, song)
	stream, err := e.openSong(ctx, song)

	e.mu.Lock()
	defer e.mu.Unlock()
	close(t.preparing)
	t.preparing = nil
	current := e.players[guildID] == p && len(p.queue) > 0 && p.queue[0] == t && t.stream == nil
	switch {
	case err != nil:
		e.logger().Warn("next song won't start", slog.String("song", song.Title), redact.Attr(err))
		if current && e.lifeLocked().Err() == nil {
			// It would fail again when its turn came, so the one after it gets ready instead.
			p.queue = p.queue[1:]
			go e.prepare(guildID)
			go e.events.QueueChanged(guildID)
		}
	case current:
		t.song, t.stream, t.cover = song, stream, e.coverAhead(song)
		p.frames.queue(stream)
	default:
		stream.Close() // the queue changed meanwhile
	}
}

// fresh looks a song up again if its link stops working before long, and
// keeps its title, artist and cover.
func (e *Engine) fresh(ctx context.Context, song songs.Song) songs.Song {
	if song.Audio.Expires.IsZero() || time.Until(song.Audio.Expires) > freshFor {
		return song
	}
	again, err := e.finder.Find(ctx, song.Link)
	if err != nil {
		return song
	}
	song.Audio = again.Audio
	return song
}

// openSong starts turning a found song into frames.
func (e *Engine) openSong(ctx context.Context, song songs.Song) (*audio.Stream, error) {
	stream, err := e.ffmpegFor(ctx)
	if err != nil {
		return nil, err
	}
	return e.feed(ctx, stream, song)
}

// feed gives a started stream its song's audio: downloaded in pieces, or a
// playlist's pieces one after another. Playlists FetchHLS leaves alone, like
// live streams, get an ffmpeg that reads them itself instead.
func (e *Engine) feed(ctx context.Context, stream *audio.Stream, song songs.Song) (*audio.Stream, error) {
	if !song.Audio.Playlist {
		stream.Feed(audio.Fetch(ctx, e.http, source(song)))
		return stream, nil
	}
	pieces, err := audio.FetchHLS(ctx, e.http, source(song))
	if err == nil {
		stream.Feed(pieces)
		return stream, nil
	}
	if errors.Is(err, audio.ErrTooBig) || errors.Is(err, netguard.ErrPrivate) {
		// Not left to ffmpeg, which would read it all, or go where it was
		// refused.
		stream.Close()
		return nil, fmt.Errorf("%w: %w", songs.ErrUnplayable, err)
	}
	if !errors.Is(err, audio.ErrNotSimple) {
		e.logger().Debug("reading the playlist failed, ffmpeg will try", redact.Attr(err))
	}
	stream.Close()
	if e.guardLinks {
		if err := netguard.CheckLink(ctx, song.Audio.URL); err != nil {
			return nil, fmt.Errorf("%w: %w", songs.ErrUnplayable, err)
		}
	}
	return audio.Open(ctx, e.ffmpeg, source(song))
}

// feedFor feeds a stream for someone who waits until ctx is done at most:
// a playlist is read before it's fed, which can take a moment. The stream
// lives as long as life. Given up on, it's closed once it's fed.
func (e *Engine) feedFor(ctx, life context.Context, stream *audio.Stream, song songs.Song) (*audio.Stream, error) {
	type fed struct {
		stream *audio.Stream
		err    error
	}
	done := make(chan fed, 1)
	go func() {
		s, err := e.feed(life, stream, song)
		done <- fed{s, err}
	}()
	select {
	case f := <-done:
		return f.stream, f.err
	case <-ctx.Done():
		go func() {
			if f := <-done; f.err == nil {
				f.stream.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func source(song songs.Song) audio.Source {
	return audio.Source{URL: song.Audio.URL, Header: song.Audio.Header, Ready: song.Audio.Ready}
}

// moved follows a voice connection on to its next song, the moment frames
// switches to it, so the queue, pause and skips agree with what plays from
// then on. It runs in disgo's audio sender, so it only keeps the books; the
// rest waits for afterMove.
func (e *Engine) moved(guildID snowflake.ID, from, to opus) {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok || p.playing == nil || opus(p.playing.stream) != from {
		e.mu.Unlock()
		from.Close()
		return
	}
	old := p.playing
	p.playing, p.paused = nil, false
	p.frames.pause(false)
	if to != nil {
		// Usually first in line, unless a song was just asked for next.
		for i, q := range p.queue {
			if opus(q.stream) == to {
				p.playing, p.queue = q, slices.Delete(p.queue, i, i+1)
				break
			}
		}
	}
	late := p.playing == nil && len(p.queue) > 0 // the next song wasn't ready in time
	if p.playing == nil && !late {
		p.idle.Reset(idleFor)
	}
	now := p.playing
	e.mu.Unlock()
	go e.afterMove(guildID, old, now, from, late)
}

// afterMove sees to what waits once a song has run out: the music after it
// first, then the old song's ffmpeg, and the app, which can take seconds to
// show it.
func (e *Engine) afterMove(guildID snowflake.ID, old, now *Track, from opus, late bool) {
	if e.over() {
		// The song ran out because the engine is stopping, which isn't news.
		from.Close()
		return
	}
	if late {
		go e.advance(guildID)
	} else {
		go e.prepare(guildID)
	}
	<-from.Done()
	err := from.Err()
	from.Close()
	if e.over() {
		return
	}
	if err != nil {
		e.logger().Warn("song stopped early", redact.Attr(err))
	}
	e.mu.Lock()
	skipped := old.skipped
	e.mu.Unlock()
	e.events.Ended(guildID, old, End{SkippedBy: skipped, Err: public(err)})
	if now != nil {
		e.events.Started(guildID, now)
	}
}

// advance plays the next song in line when it wasn't ready in time, and
// passes over songs that won't start. One advance works for a player at a
// time, so two can't start songs out of order: another one asked for
// meanwhile leaves a note for the one at work, which goes round again before
// it's done. The app is told about the songs it started once it's done, so
// a slow app doesn't hold up the next song.
func (e *Engine) advance(guildID snowflake.ID) {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok {
		e.mu.Unlock()
		return
	}
	if p.advancing {
		p.again = true
		e.mu.Unlock()
		return
	}
	p.advancing = true
	var started []*Track
	for {
		if t := e.advanceLocked(guildID, p); t != nil {
			started = append(started, t)
		}
		if !p.again || e.players[guildID] != p {
			break
		}
		p.again = false
	}
	p.advancing = false
	e.mu.Unlock()
	for _, t := range started {
		e.events.Started(guildID, t)
	}
}

// advanceLocked is the advance at work's: it plays the next song in line,
// once it's ready, and returns it; or nil when another song plays or none is
// left. A song not ready yet gets ready while it stays first in line, so no
// song behind it can get ready and take over first, and one that won't
// start is passed over by getReady. e.mu is held throughout, but for the
// waits, and a ready song is taken from the queue and started in one go, so
// no other song can start in between.
func (e *Engine) advanceLocked(guildID snowflake.ID, p *player) *Track {
	for {
		if e.players[guildID] != p || p.playing != nil || p.conn == nil || e.lifeLocked().Err() != nil {
			return nil
		}
		if len(p.queue) == 0 {
			p.idle.Reset(idleFor)
			return nil
		}
		t := p.queue[0]
		if t.stream == nil {
			if t.preparing == nil {
				t.preparing = make(chan struct{})
				go e.getReady(guildID, p, t, t.song)
			}
			// Waited for, unless the queue changes meanwhile and another song
			// is next.
			if p.changed == nil {
				p.changed = make(chan struct{})
			}
			wait, changed := t.preparing, p.changed
			e.mu.Unlock()
			select {
			case <-wait:
			case <-changed:
			}
			e.mu.Lock()
			continue
		}
		p.queue = p.queue[1:]
		p.frames.queue(ready(p.queue)) // it plays now, rather than next
		p.playLocked(t, nil)
		go e.prepare(guildID) // the one after it, before the app is told, which can take seconds
		return t
	}
}
