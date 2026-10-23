package lullaby

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/voice"
	"github.com/disgoorg/snowflake/v2"
)

func TestRejoinWaitsForTheOldConnection(t *testing.T) {
	manager := voice.NewManager(nil, 2, voice.WithConnCreateFunc(func(_, _ snowflake.ID, _ voice.StateUpdateFunc, remove func(), _ ...voice.ConnConfigOpt) voice.Conn {
		return &quietConn{remove: remove}
	}))
	e := &Engine{
		players: make(map[snowflake.ID]*player), turns: make(map[snowflake.ID]*sync.Mutex),
		leaving: make(map[snowflake.ID]chan struct{}), dave: NewDAVE(), conns: manager, events: app{},
	}
	if _, err := e.join(context.Background(), 1, 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	old := e.take(1, func(*player) bool { return true }) // a stop, which leaves in the background
	// A song asked for right after, which disgo would hand the old connection to.
	p, err := e.join(context.Background(), 1, 200, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer e.take(1, func(*player) bool { return true })
	if p.conn == old.conn || p.conn.(*quietConn).isClosed() || manager.GetConn(1) != p.conn {
		t.Errorf("the new player got the old connection, or lost its own: same %v, closed %v",
			p.conn == old.conn, p.conn.(*quietConn).isClosed())
	}
	if !old.conn.(*quietConn).isClosed() {
		t.Error("the old connection was never closed")
	}
}

type openingConn struct {
	*quietConn
	calls                                                    atomic.Int32
	enteredFirst, releaseFirst, enteredSecond, releaseSecond chan struct{}
}

func (c *openingConn) Open(ctx context.Context, _ snowflake.ID, _, _ bool) error {
	var release chan struct{}
	switch c.calls.Add(1) {
	case 1:
		close(c.enteredFirst)
		release = c.releaseFirst
	case 2:
		close(c.enteredSecond)
		release = c.releaseSecond
	default:
		return nil
	}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestStopWhileJoiningThenRejoin(t *testing.T) {
	c := &openingConn{quietConn: &quietConn{}, enteredFirst: make(chan struct{}), releaseFirst: make(chan struct{}), enteredSecond: make(chan struct{}), releaseSecond: make(chan struct{})}
	manager := voice.NewManager(nil, 2, voice.WithConnCreateFunc(func(_, _ snowflake.ID, _ voice.StateUpdateFunc, remove func(), _ ...voice.ConnConfigOpt) voice.Conn {
		c.remove = remove
		return c
	}))
	e := &Engine{players: make(map[snowflake.ID]*player), turns: make(map[snowflake.ID]*sync.Mutex), leaving: make(map[snowflake.ID]chan struct{}), dave: NewDAVE(), conns: manager, events: app{}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	firstDone := make(chan struct{})
	go func() { e.join(ctx, 1, 100, time.Hour); close(firstDone) }()
	select {
	case <-c.enteredFirst:
	case <-ctx.Done():
		t.Fatal("first Open missing")
	}
	old := e.take(1, func(*player) bool { return true })
	type result struct {
		p   *player
		err error
	}
	secondDone := make(chan result, 1)
	go func() { p, err := e.join(ctx, 1, 200, time.Hour); secondDone <- result{p, err} }()
	// Ensure the replacement reserved its player and captured the leaving
	// barrier before the first Open returns. No sockets or scheduling luck.
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		e.mu.Lock()
		reserved := e.players[1] != nil && e.players[1] != old
		e.mu.Unlock()
		if reserved {
			break
		}
		if time.Now().After(deadline) {
			close(c.releaseFirst)
			close(c.releaseSecond)
			t.Fatal("replacement not reserved")
		}
	}
	// Hold bookkeeping at e.mu after the old join releases turn. The new
	// join can now acquire turn and reuse disgo's still-registered old conn.
	e.mu.Lock()
	close(c.releaseFirst)
	reused := false
	select {
	case <-c.enteredSecond:
		reused = true
	case <-time.After(200 * time.Millisecond):
	}
	close(c.releaseSecond)
	e.mu.Unlock()
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("first join stuck")
	}
	var second result
	select {
	case second = <-secondDone:
	case <-ctx.Done():
		t.Fatal("replacement stuck")
	}
	if second.p != nil {
		e.take(1, func(*player) bool { return true })
	}
	if reused {
		t.Fatalf("replacement Open reused a connection whose original player was stopped while joining; old cleanup closes it (second error=%v)", second.err)
	}
}

func TestStoppedJoinLeavesTheNewOneAlone(t *testing.T) {
	manager := voice.NewManager(nil, 2, voice.WithConnCreateFunc(func(_, _ snowflake.ID, _ voice.StateUpdateFunc, remove func(), _ ...voice.ConnConfigOpt) voice.Conn {
		return &quietConn{remove: remove}
	}))
	e := &Engine{players: make(map[snowflake.ID]*player), turns: make(map[snowflake.ID]*sync.Mutex), leaving: make(map[snowflake.ID]chan struct{}), dave: NewDAVE(), conns: manager, events: app{}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	oldGone := make(chan struct{})
	e.leaving[1] = oldGone
	firstDone := make(chan struct{})
	go func() { e.join(ctx, 1, 100, time.Hour); close(firstDone) }()
	for deadline := time.Now().Add(time.Second); ; time.Sleep(time.Millisecond) {
		e.mu.Lock()
		reserved := e.players[1] != nil
		e.mu.Unlock()
		if reserved {
			break
		}
		if time.Now().After(deadline) {
			close(oldGone)
			t.Fatal("first join not reserved")
		}
	}
	e.take(1, func(*player) bool { return true }) // Stop the join waiting for old cleanup.
	// Pause old cleanup between deleting its barrier and closing it
	// (leavingLocked's final two operations). This also models the stopped
	// waiter simply not being scheduled until after the replacement joins.
	e.mu.Lock()
	delete(e.leaving, 1)
	e.mu.Unlock()
	replacement, err := e.join(ctx, 1, 200, time.Hour)
	close(oldGone)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("stopped join stuck")
	}
	defer e.take(1, func(*player) bool { return true })
	if replacement.conn.(*quietConn).isClosed() || manager.GetConn(1) != replacement.conn {
		t.Fatal("a join stopped while waiting closed the newer join's connection")
	}
}
