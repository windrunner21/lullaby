package lullaby

import (
	"io"
	"sync"
)

// opus is a song as Opus frames, like an *audio.Stream.
type opus interface {
	ProvideOpusFrame() ([]byte, error)
	Done() <-chan struct{}
	Err() error
	Close()
}

// frames is what disgo's audio sender plays in one voice connection. It's
// set once, when the player joins, and plays whichever song it's given:
// replacing the sender's provider for every song could catch disgo's sender
// half started.
// The song next in line waits ready and takes over in the same 20 ms as the
// last frame of the one before, so there's no gap between them. While the
// connection's encryption is being set up, it hands out nothing, which
// Discord hears as silence.
type frames struct {
	hold func() bool
	// moved is called the moment a song runs out, with the one that took
	// over or nil. It runs in disgo's audio sender, so it must be quick.
	moved func(from, to opus)

	mu      sync.Mutex
	song    opus   // nil while quiet
	next    opus   // takes over when song runs out
	started func() // called with song's first frame, then cleared
	paused  bool
}

// play switches to song now. started is called once its first frame goes out.
func (f *frames) play(song opus, started func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.song, f.started = song, started
}

// queue has next take over once the playing song runs out.
func (f *frames) queue(next opus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = next
}

// pause holds the song where it is, or lets it go on.
func (f *frames) pause(paused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = paused
}

func (f *frames) ProvideOpusFrame() ([]byte, error) {
	if f.hold() {
		return nil, nil
	}
	f.mu.Lock()
	song, paused := f.song, f.paused
	f.mu.Unlock()
	if song == nil || paused {
		return nil, nil
	}
	frame, err := song.ProvideOpusFrame()
	if err == io.EOF {
		return f.runOut(song)
	}
	if frame != nil {
		var started func()
		f.mu.Lock()
		if f.song == song {
			started, f.started = f.started, nil
		}
		f.mu.Unlock()
		if started != nil {
			go started()
		}
	}
	return frame, err
}

// runOut moves on from a song that has run out to the next one, if any, and
// hands out its first frame right away.
func (f *frames) runOut(song opus) ([]byte, error) {
	f.mu.Lock()
	if f.song != song {
		f.mu.Unlock()
		return nil, nil // play switched songs meanwhile
	}
	next := f.next
	f.song, f.next, f.started = next, nil, nil
	f.mu.Unlock()

	// Right away, in the same 20 ms, so the queue, pause and skips agree with
	// what plays, and a song that fails at once can't be told about before
	// the one before it.
	f.moved(song, next)
	if next == nil {
		return nil, nil
	}
	frame, err := next.ProvideOpusFrame()
	if err == io.EOF {
		return nil, nil // the next call finds it over, and moves on again
	}
	return frame, err
}

// current is the song playing, or nil.
func (f *frames) current() opus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.song
}

// Close stops the songs it holds.
func (f *frames) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range []opus{f.song, f.next} {
		if s != nil {
			s.Close()
		}
	}
}
