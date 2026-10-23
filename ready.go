package lullaby

import (
	"context"
	"time"

	"github.com/disgoorg/snowflake/v2"

	"github.com/windrunner21/lullaby/internal/audio"
	"github.com/windrunner21/lullaby/internal/redact"
)

// JoinEarly starts joining a voice channel for someone typing a song's name,
// so the player is there by the time they pick one. Joining took about
// 1.1 s, which came before the first song's first sound. If no song comes,
// it leaves after 30 seconds. It does nothing while the player is in voice in
// the server already.
func (e *Engine) JoinEarly(guildID, channelID snowflake.ID) {
	if _, busy := e.Channel(guildID); busy {
		return
	}
	go func() {
		if _, err := e.join(e.life(), guildID, channelID, earlyFor); err != nil {
			e.logger().Debug("joining voice early failed", redact.Attr(err))
		}
	}()
}

// GetReady gets the rest ready for someone typing a song's name, beside
// joining voice: an ffmpeg for their song, and a connection to YouTube's
// API, which Go closes when it's been unused for a while.
func (e *Engine) GetReady() {
	e.mu.Lock()
	touch := time.Since(e.touched) > touchEvery
	if touch {
		e.touched = time.Now()
	}
	spare := e.spare == nil && !e.sparing
	if spare {
		e.sparing = true
	}
	e.mu.Unlock()
	if touch {
		go e.yt.Touch(e.life())
	}
	if spare {
		go e.startSpare()
	}
}

// startSpare starts an ffmpeg for a song someone is typing. When the song is
// found at once, like one played before, the lookup no longer hides the
// 95 ms ffmpeg takes to start. Unused after earlyFor, it's closed.
func (e *Engine) startSpare() {
	s, err := audio.Start(e.life(), e.ffmpeg)
	e.mu.Lock()
	e.sparing = false
	if err == nil {
		e.spare = s
	}
	e.mu.Unlock()
	if err != nil {
		e.logger().Debug("spare ffmpeg failed", redact.Attr(err))
		return
	}
	time.AfterFunc(earlyFor, func() {
		e.mu.Lock()
		unused := e.spare == s
		if unused {
			e.spare = nil
		}
		e.mu.Unlock()
		if unused {
			s.Close()
		}
	})
}

// ffmpegFor returns an ffmpeg for a song about to play: the spare, if one is
// waiting and still running, or a new one.
func (e *Engine) ffmpegFor(ctx context.Context) (*audio.Stream, error) {
	e.mu.Lock()
	s := e.spare
	e.spare = nil
	e.mu.Unlock()
	if s != nil {
		select {
		case <-s.Done():
			s.Close() // it stopped by itself
		default:
			return s, nil
		}
	}
	return audio.Start(ctx, e.ffmpeg)
}
