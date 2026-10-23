package lullaby

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSpareFFmpeg(t *testing.T) {
	// An "ffmpeg" that waits for its song, as a real one does.
	fake := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec cat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &Engine{ctx: context.Background(), ffmpeg: fake}
	e.startSpare()
	e.mu.Lock()
	spare := e.spare
	e.mu.Unlock()
	if spare == nil {
		t.Fatal("no spare")
	}
	got, err := e.ffmpegFor(context.Background())
	if err != nil || got != spare {
		t.Fatalf("ffmpegFor = %p, %v; want the spare", got, err)
	}
	defer got.Close()
	again, err := e.ffmpegFor(context.Background())
	if err != nil || again == spare {
		t.Fatalf("ffmpegFor again = %p, %v; want a new one", again, err)
	}
	again.Close()

	// A spare that stopped by itself isn't handed out.
	e.startSpare()
	e.mu.Lock()
	e.spare.Close()
	stopped := e.spare
	e.mu.Unlock()
	<-stopped.Done()
	if s, err := e.ffmpegFor(context.Background()); err != nil || s == stopped {
		t.Errorf("handed out a stopped spare (%v)", err)
	} else {
		s.Close()
	}
}
