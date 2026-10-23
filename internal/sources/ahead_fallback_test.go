package sources

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

type failedAhead struct {
	entered chan struct{}
	release chan struct{}
	asked   atomic.Int32
}

func (l *failedAhead) Lookup(ctx context.Context, _ string) (songs.Song, error) {
	if l.asked.Add(1) == 1 {
		close(l.entered)
		select {
		case <-l.release:
		case <-ctx.Done():
			return songs.Song{}, ctx.Err()
		}
	}
	return songs.Song{}, songs.ErrNoAudio
}

// waiterContext tells the test that Find has reached its wait on the
// in-flight ahead lookup. It avoids using a sleep to guess when Find joined.
type waiterContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *waiterContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestFindAfterAFailedLookAhead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fast := &failedAhead{entered: make(chan struct{}), release: make(chan struct{})}
	backup := &fakeLookup{song: songs.Song{Title: "backup", Audio: songs.Audio{Expires: time.Now().Add(6 * time.Hour)}}}
	f := &Finder{youtube: fast, ytdlp: backup}
	link := songs.WatchLink("aaaaaaaaaaa")
	aheadDone := make(chan struct{})
	go func() { f.FindAhead(ctx, link); close(aheadDone) }()
	select {
	case <-fast.entered:
	case <-ctx.Done():
		t.Fatal("ahead lookup did not start")
	}
	waiter := &waiterContext{Context: ctx, entered: make(chan struct{})}
	type result struct {
		song songs.Song
		err  error
	}
	got := make(chan result, 1)
	go func() { song, err := f.Find(waiter, link); got <- result{song, err} }()
	select {
	case <-waiter.entered:
	case <-ctx.Done():
		t.Fatal("Find did not join the ahead lookup")
	}
	close(fast.release)
	select {
	case r := <-got:
		if r.err != nil || r.song.Title != "backup" {
			t.Fatalf("Find joining failed ahead = %q, %v; want backup", r.song.Title, r.err)
		}
	case <-ctx.Done():
		t.Fatal("Find stuck after ahead failed")
	}
	<-aheadDone
	if len(backup.asked) != 1 {
		t.Fatalf("backup called %d times, want once", len(backup.asked))
	}
	t.Logf("fast path requested %d times before one backup lookup", fast.asked.Load())
}

func TestFindAheadDuringABotCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	fast := &fakeLookup{err: songs.ErrBotCheck}
	f := &Finder{youtube: fast}
	if _, err := f.Find(ctx, songs.WatchLink("aaaaaaaaaaa")); !errors.Is(err, songs.ErrBotCheck) {
		t.Fatal(err)
	}
	f.FindAhead(ctx, songs.WatchLink("bbbbbbbbbbb"))
	if len(fast.asked) != 1 {
		t.Fatalf("ahead retried YouTube during a known bot check without cookies: %d requests", len(fast.asked))
	}
}
