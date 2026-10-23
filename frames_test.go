package lullaby

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSong is a song of n frames.
type fakeSong struct {
	mu     sync.Mutex
	left   int
	done   chan struct{}
	closed bool
}

func newFakeSong(n int) *fakeSong {
	done := make(chan struct{})
	close(done)
	return &fakeSong{left: n, done: done}
}

func (s *fakeSong) ProvideOpusFrame() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.left == 0 {
		return nil, io.EOF
	}
	s.left--
	return []byte{0xfc}, nil
}

func (s *fakeSong) Done() <-chan struct{} { return s.done }
func (s *fakeSong) Err() error            { return nil }
func (s *fakeSong) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// moves records what frames told moved.
type moves chan [2]opus

func (m moves) moved(from, to opus) { m <- [2]opus{from, to} }

// next waits for the next move.
func (m moves) next(t *testing.T) [2]opus {
	t.Helper()
	select {
	case got := <-m:
		return got
	case <-time.After(time.Second):
		t.Fatal("moved wasn't called")
		return [2]opus{}
	}
}

func TestFrames(t *testing.T) {
	var holding atomic.Bool
	m := make(moves, 4)
	f := &frames{hold: holding.Load, moved: m.moved}

	if frame, err := f.ProvideOpusFrame(); frame != nil || err != nil {
		t.Errorf("with nothing playing: %v, %v; want silence", frame, err)
	}

	song := newFakeSong(2)
	var starts atomic.Int32
	f.play(song, func() { starts.Add(1) })
	holding.Store(true)
	for range 3 {
		if frame, _ := f.ProvideOpusFrame(); frame != nil {
			t.Fatal("handed out a frame while encryption was being set up")
		}
	}
	holding.Store(false)
	for i := range 2 {
		if frame, err := f.ProvideOpusFrame(); frame == nil || err != nil {
			t.Fatalf("frame %d: %v, %v", i, frame, err)
		}
	}
	// The song is over, with nothing next: silence, and one move to nothing.
	for range 3 {
		if frame, err := f.ProvideOpusFrame(); frame != nil || err != nil {
			t.Errorf("after the song: %v, %v; want silence", frame, err)
		}
	}
	if got := m.next(t); got[0] != opus(song) || got[1] != nil {
		t.Errorf("moved(%v, %v), want from the song to nothing", got[0], got[1])
	}
	select {
	case <-m:
		t.Error("moved was called twice for one song")
	case <-time.After(50 * time.Millisecond):
	}
	if starts.Load() != 1 {
		t.Errorf("started was called %d times for one song, want once", starts.Load())
	}

	// Paused, the song waits where it is.
	paused := newFakeSong(1)
	f.play(paused, nil)
	f.pause(true)
	if frame, _ := f.ProvideOpusFrame(); frame != nil {
		t.Error("handed out a frame while paused")
	}
	f.pause(false)
	if frame, _ := f.ProvideOpusFrame(); frame == nil {
		t.Error("the song didn't go on after the pause")
	}
}

func TestFramesWithoutAGap(t *testing.T) {
	m := make(moves, 4)
	f := &frames{hold: func() bool { return false }, moved: m.moved}
	first, second := newFakeSong(1), newFakeSong(3)
	f.play(first, nil)
	f.queue(second)

	if frame, _ := f.ProvideOpusFrame(); frame == nil {
		t.Fatal("the first song's frame didn't come")
	}
	// The call that finds the first song over hands out the second's first
	// frame: not one 20 ms beat of silence between them.
	if frame, _ := f.ProvideOpusFrame(); frame == nil {
		t.Error("a gap between the songs")
	}
	if got := m.next(t); got[0] != opus(first) || got[1] != opus(second) {
		t.Errorf("moved(%v, %v), want from the first song to the second", got[0], got[1])
	}
	second.mu.Lock()
	left := second.left
	second.mu.Unlock()
	if left != 2 {
		t.Errorf("the second song has %d frames left, want 2", left)
	}
	f.Close()
	if !second.closed {
		t.Error("Close didn't stop the song")
	}
}
