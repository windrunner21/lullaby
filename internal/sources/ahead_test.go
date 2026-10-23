package sources

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/songs"
)

func TestFindAheadLeavesYTDLPAlone(t *testing.T) {
	var extracted atomic.Int32
	dl := NewYTDLP("offline-extractor", "", "")
	dl.run = func(context.Context, string, ...string) ([]byte, error) {
		extracted.Add(1)
		return []byte(`{"protocol":"https","title":"song","url":"https://audio.invalid/song?expire=2000000000"}`), nil
	}
	f := &Finder{signedIn: dl.SignedIn("unused-cookies"), checked: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	link := songs.WatchLink("aaaaaaaaaaa")
	// During a bot check, a link being chosen isn't looked up through the
	// spare account.
	f.FindAhead(ctx, link)
	if extracted.Load() != 0 {
		t.Fatal("looked a link up ahead with signed-in yt-dlp")
	}
	// Asked for, it is.
	if _, err := f.Find(ctx, link); err != nil || extracted.Load() != 1 {
		t.Errorf("Find = %v after %d lookups, want one signed in", err, extracted.Load())
	}
}

func TestYTDLPLimitIncludesTheWait(t *testing.T) {
	defer func(d time.Duration) { lookupFor = d }(lookupFor)
	lookupFor = 200 * time.Millisecond
	y := NewYTDLP("must-not-run", "", "").SignedIn("unused-cookies")
	// Another signed-in lookup owns the cookie file. No process or network
	// operation should be reached by this waiter.
	y.turn <- struct{}{}
	defer func() { <-y.turn }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := y.Lookup(ctx, songs.WatchLink("aaaaaaaaaaa")); done <- err }()
	missed := false
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued lookup: %v", err)
		}
	case <-time.After(lookupFor + 2*time.Second):
		missed = true
	}
	if missed {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("lookup did not cancel")
		}
		t.Fatal("a signed-in lookup waited for its turn past lookupFor")
	}
}
