package audio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// frame is how much sound one Opus frame holds, the length Discord wants.
	frame = 20 * time.Millisecond
	// buffered is how many frames wait ready for Discord: 10 seconds.
	buffered = 500
)

// Stream is a song on its way to Discord as 20 ms Opus frames. It fits disgo's
// voice.OpusFrameProvider.
type Stream struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	cancel context.CancelFunc
	frames chan []byte
	done   chan struct{}
	played atomic.Int64
	closed atomic.Bool
	stderr capped

	mu         sync.Mutex
	err        error
	closeAudio func() // closes what Feed was given, once
}

// Start starts ffmpeg, at the path ffmpeg, for a song whose audio comes later
// through Feed. ffmpeg takes about 95 ms just to start on the Pi, so start it
// while the song is being looked up. Close the stream when done with it, even
// if it was never fed.
func Start(ctx context.Context, ffmpeg string) (*Stream, error) {
	return start(ctx, ffmpeg, []string{"-i", "pipe:0"})
}

// Open starts ffmpeg reading an HLS playlist at src itself, the way SoundCloud
// and live streams come: Fetch can't download those in pieces. It only follows
// web addresses, so a playlist can't point ffmpeg at files on the machine.
func Open(ctx context.Context, ffmpeg string, src Source) (*Stream, error) {
	input := []string{"-protocol_whitelist", "http,https,tls,tcp,crypto"}
	var headers strings.Builder
	for k, vs := range src.Header {
		for _, v := range vs {
			headers.WriteString(k + ": " + v + "\r\n")
		}
	}
	if headers.Len() > 0 {
		input = append(input, "-headers", headers.String())
	}
	return start(ctx, ffmpeg, append(input, "-i", src.URL))
}

// start runs ffmpeg with the given input options, ready to hand out frames.
// It reads stdin when the input is pipe:0. Opus that's already Opus is
// encoded again too: Discord wants 20 ms frames, and other sites' Opus can
// come in other lengths. It costs about 2% of one core on the Pi.
func start(ctx context.Context, ffmpeg string, input []string) (*Stream, error) {
	codec := []string{"-c:a", "libopus", "-b:a", "128k", "-ar", "48000", "-ac", "2", "-frame_duration", "20"}
	args := append([]string{"-hide_banner", "-loglevel", "error"}, input...)
	args = append(append(args, "-map", "0:a:0"), codec...)
	// Small pages hand each frame out as soon as it's encoded.
	args = append(args, "-page_duration", "20000", "-f", "ogg", "pipe:1")

	ctx, cancel := context.WithCancel(ctx)
	s := &Stream{cancel: cancel, frames: make(chan []byte, buffered), done: make(chan struct{})}
	s.cmd = exec.CommandContext(ctx, ffmpeg, args...)
	s.cmd.Stderr = &s.stderr
	if input[len(input)-1] == "pipe:0" {
		stdin, err := s.cmd.StdinPipe()
		if err != nil {
			cancel()
			return nil, err
		}
		s.stdin = stdin
	}
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := s.cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	go s.read(ctx, stdout)
	return s, nil
}

// Feed gives ffmpeg the song's audio and closes audio once it's all in, or when
// the stream is closed. If the download fails, what came before it still plays.
func (s *Stream) Feed(audio io.ReadCloser) {
	if s.stdin == nil {
		audio.Close() // a stream from Open reads its audio itself
		return
	}
	closeAudio := sync.OnceFunc(func() { audio.Close() })
	s.mu.Lock()
	s.closeAudio = closeAudio
	s.mu.Unlock()
	go func() {
		defer closeAudio()
		defer s.stdin.Close()
		buf := make([]byte, chunk)
		for {
			n, err := audio.Read(buf)
			if n > 0 {
				if _, err := s.stdin.Write(buf[:n]); err != nil {
					return // ffmpeg stopped, and read says why
				}
			}
			if err == io.EOF {
				return
			}
			if err != nil {
				s.fail(fmt.Errorf("download: %w", err))
				return
			}
		}
	}()
}

// read takes ffmpeg's frames as fast as it makes them, up to buffered ahead.
func (s *Stream) read(ctx context.Context, stdout io.Reader) {
	defer close(s.done)
	defer close(s.frames)

	ogg := newOggReader(stdout)
	var readErr error
loop:
	for {
		p, err := ogg.next()
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
		select {
		case s.frames <- p:
		case <-ctx.Done():
			break loop
		}
	}

	broken := readErr != nil && ctx.Err() == nil
	if broken {
		// Nothing reads ffmpeg's output anymore, so it could wait forever to
		// write the rest.
		s.cancel()
	}
	err := s.cmd.Wait()
	switch {
	case broken:
		s.fail(fmt.Errorf("ffmpeg's output: %w", readErr))
	case ctx.Err() != nil:
		// Closed on purpose.
	case err != nil:
		s.fail(fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(s.stderr.String())))
	}
}

// ProvideOpusFrame returns the next frame. When none is ready yet, it returns
// nothing right away, so Discord gets a moment of silence instead of a late
// frame. After the last frame, or once the stream is closed, it returns
// io.EOF, and Err says whether the song ended early.
func (s *Stream) ProvideOpusFrame() ([]byte, error) {
	if s.closed.Load() {
		return nil, io.EOF // what's buffered is dropped, so a skip is a skip
	}
	select {
	case f, ok := <-s.frames:
		if !ok {
			return nil, io.EOF
		}
		s.played.Add(1)
		return f, nil
	default:
		return nil, nil
	}
}

// Played is how much of the song has gone to Discord, exact to the frame.
func (s *Stream) Played() time.Duration {
	return time.Duration(s.played.Load()) * frame
}

// Done is closed once ffmpeg has stopped and every frame it made is waiting
// in the stream.
func (s *Stream) Done() <-chan struct{} {
	return s.done
}

// Err says why the song stopped early, once Done is closed. It's nil for a
// song that played to the end or was closed.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close stops ffmpeg and the download. The frames still waiting are dropped
// once ffmpeg has stopped, so a stream kept around after its song doesn't
// keep them.
func (s *Stream) Close() {
	if s.closed.Swap(true) {
		return
	}
	s.cancel()
	go func() {
		<-s.done
		for range s.frames {
		}
	}()
	s.mu.Lock()
	closeAudio := s.closeAudio
	s.mu.Unlock()
	if closeAudio != nil {
		closeAudio()
	}
}

// fail keeps the first reason the song stopped early. Whatever breaks after
// Close is because of it.
func (s *Stream) fail(err error) {
	if s.closed.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// capped keeps the start of what ffmpeg says, where the cause of an error is,
// and drops the rest.
type capped struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := 2048 - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
