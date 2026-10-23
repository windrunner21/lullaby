package lullaby

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

// Stop stops the music in a server and leaves voice, in the background. It
// returns the song that was playing, if any, and whether the player was in
// voice there, or on its way.
func (e *Engine) Stop(guildID snowflake.ID) (*Track, bool) {
	p := e.take(guildID, func(*player) bool { return true })
	if p == nil {
		return nil, false
	}
	return p.playing, true
}

// StopSong is Stop, but only while t is playing, so an old song's buttons
// can't stop a newer one. It reports whether it stopped.
func (e *Engine) StopSong(guildID snowflake.ID, t *Track) bool {
	return e.take(guildID, func(p *player) bool { return p.playing != nil && p.playing == t }) != nil
}

// Disconnected stops the music in a server after someone took the player out
// of voice. It returns the song that was playing, if any, and whether it
// stopped: a player still joining is left to finish.
func (e *Engine) Disconnected(guildID snowflake.ID) (*Track, bool) {
	p := e.take(guildID, func(p *player) bool { return p.conn != nil })
	if p == nil {
		return nil, false
	}
	return p.playing, true
}

// Moved follows the player to the voice channel someone moved it to.
func (e *Engine) Moved(guildID, channelID snowflake.ID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.players[guildID]; ok {
		p.channelID = channelID
	}
}

// stopAll stops the music in every server and leaves voice, once the
// engine's lifetime is over.
func (e *Engine) stopAll() {
	e.mu.Lock()
	guilds := slices.Collect(maps.Keys(e.players))
	spare := e.spare
	e.spare = nil
	e.mu.Unlock()
	if spare != nil {
		spare.Close()
	}
	for _, guildID := range guilds {
		e.take(guildID, func(*player) bool { return true })
	}
}

// leaveIfIdle leaves when nothing has played, or the song has been paused,
// for a while.
func (e *Engine) leaveIfIdle(guildID snowflake.ID, p *player) {
	if e.over() {
		return // stopAll sees to it, without telling anyone
	}
	if left := e.take(guildID, func(q *player) bool { return q == p && (q.playing == nil || q.paused) }); left != nil {
		e.events.Left(guildID, left.playing)
	}
}

// take forgets a server's player if want says so, stops its music, leaves
// voice in the background and returns it, or returns nil.
func (e *Engine) take(guildID snowflake.ID, want func(*player) bool) *player {
	e.mu.Lock()
	p, ok := e.players[guildID]
	if !ok || !want(p) {
		e.mu.Unlock()
		return nil
	}
	delete(e.players, guildID)
	if p.idle != nil {
		p.idle.Stop()
	}
	for _, t := range append([]*Track{p.playing}, p.queue...) {
		if t != nil && t.stream != nil {
			t.stream.Close()
		}
	}
	var gone func()
	if p.conn != nil { // still joining otherwise: join hangs up itself once it sees the music was stopped
		gone = e.leavingLocked(guildID)
	}
	e.mu.Unlock()
	if gone != nil {
		go func() {
			e.hangUp(guildID, p.conn)
			gone()
		}()
	}
	return p
}

// leavingLocked marks a server's voice connection as on its way out, and
// returns what to call once it's gone. Joining the server waits until then:
// disgo's voice manager knows connections by server, so joining sooner would
// get the old connection back, just before it closes. e.mu must be held.
func (e *Engine) leavingLocked(guildID snowflake.ID) func() {
	gone := make(chan struct{})
	e.leaving[guildID] = gone
	return func() {
		e.mu.Lock()
		if e.leaving[guildID] == gone {
			delete(e.leaving, guildID)
		}
		e.mu.Unlock()
		close(gone)
	}
}

// join connects to a voice channel, unless the player is there already or on
// its way, and returns the server's player. If nothing plays, it leaves again
// after idle.
func (e *Engine) join(ctx context.Context, guildID, channelID snowflake.ID, idle time.Duration) (*player, error) {
	e.mu.Lock()
	if err := e.lifeLocked().Err(); err != nil {
		// The engine is stopping, and stopAll may have looked already.
		e.mu.Unlock()
		return nil, fmt.Errorf("%w: the engine's lifetime is over", ErrStopped)
	}
	p, ok := e.players[guildID]
	if !ok {
		p = &player{channelID: channelID, joined: make(chan struct{})}
		e.players[guildID] = p
	}
	turn := e.turn(guildID)
	old := e.leaving[guildID]
	e.mu.Unlock()
	if ok {
		<-p.joined
		return p, p.joinErr
	}

	if old != nil {
		<-old // at most leaveTimeout
	}
	turn.Lock()
	defer turn.Unlock()
	e.mu.Lock()
	if e.players[guildID] != p {
		// Stopped while the old connection went: there's nothing to join for,
		// and disgo would hand this join a newer player's connection.
		p.joinErr = ErrStopped
		close(p.joined)
		e.mu.Unlock()
		return p, ErrStopped
	}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	conn := e.conns.CreateConn(guildID)
	// Deafened: the player only plays, and never listens to anyone.
	err := conn.Open(ctx, channelID, false, true)
	cancel()

	e.mu.Lock()
	current := e.players[guildID] == p
	switch {
	case err != nil && current:
		delete(e.players, guildID)
	case err == nil && current:
		p.conn = conn
		p.frames = &frames{
			hold:  func() bool { return e.dave.hold(guildID) },
			moved: func(from, to opus) { e.moved(guildID, from, to) },
		}
		conn.SetOpusFrameProvider(p.frames)
		p.idle = time.AfterFunc(idle, func() { e.leaveIfIdle(guildID, p) })
	}
	p.joinErr = err
	close(p.joined)
	e.mu.Unlock()

	// Leave again if it didn't work, or the music was stopped while joining,
	// before letting go of the turn: a join after this one would get this
	// connection back from disgo.
	if err != nil || !current {
		e.closeConn(guildID, conn)
	}
	return p, err
}

// hangUp leaves voice and closes the connection's encryption session.
func (e *Engine) hangUp(guildID snowflake.ID, conn voice.Conn) {
	e.mu.Lock()
	turn := e.turn(guildID)
	e.mu.Unlock()
	turn.Lock()
	defer turn.Unlock()
	e.closeConn(guildID, conn)
}

// closeConn is hangUp for whoever holds the server's turn lock already.
func (e *Engine) closeConn(guildID snowflake.ID, conn voice.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), leaveTimeout)
	defer cancel()
	conn.Close(ctx)
	e.dave.forget(guildID, conn)
}

// turn returns a server's turn lock. disgo's voice manager knows connections
// by server, so closing an old one while a new one opens would lose the new
// one. e.mu must be held.
func (e *Engine) turn(guildID snowflake.ID) *sync.Mutex {
	t, ok := e.turns[guildID]
	if !ok {
		t = &sync.Mutex{}
		e.turns[guildID] = t
	}
	return t
}
