// Package audio turns a song's audio into the 20 ms Opus frames Discord
// plays: it downloads the audio in pieces, has ffmpeg encode it, and hands the
// frames out without ever making Discord wait.
package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/windrunner21/lullaby/internal/netguard"
)

const (
	// piece is how much each download request asks for. YouTube sends a 1 MB
	// piece at full speed, but throttles one long download to about twice
	// playback speed.
	piece = 1 << 20
	// ahead is how much is downloaded ahead of playback: all of a normal song,
	// and about 15 minutes of a long mix.
	ahead = 16 * piece
	// chunk is the most that's passed on at a time.
	chunk = 64 << 10
	// notReadyWait is how long to wait before asking again for a link that
	// isn't ready yet.
	notReadyWait = 250 * time.Millisecond
)

// quietFor is how long a server may send nothing before its request is given
// up and asked again. Waiting for room ahead doesn't count.
var quietFor = 30 * time.Second

// retries are how long to wait before asking for a piece again.
var retries = []time.Duration{250 * time.Millisecond, time.Second, 3 * time.Second}

// Source is where to download a song's audio.
type Source struct {
	URL string
	// Header is sent with every request.
	Header http.Header
	// Ready is when the link starts working. YouTube turns some links down
	// for a moment after handing them out, so until then a refusal is tried
	// again. On the Pi they worked 0 to 0.6 s after yt-dlp gave them, well
	// before the time YouTube says.
	Ready time.Time
}

// Fetch downloads src in pieces and passes the bytes on as they arrive, up to
// ahead of whoever reads them, until the end, until ctx is done or until the
// reader is closed. Reading returns the download's error if it failed. Close
// the reader when done with it.
func Fetch(ctx context.Context, client *http.Client, src Source) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	f := &fetcher{cancel: cancel}
	f.changed.L = &f.mu
	context.AfterFunc(ctx, func() { f.stop(ctx.Err()) })
	d := &download{client: client, src: src, total: -1, buf: make([]byte, chunk), write: f.write}
	go func() { f.end(d.run(ctx)) }()
	return f
}

// slabs hold downloads until ffmpeg reads them, and are used again from one
// song to the next. With one growing buffer instead, a 3.2 MB song took the
// process to 19 MB on the Pi, rather than 13 MB, and stayed there until it
// was over.
var slabs = sync.Pool{New: func() any { return new([chunk]byte) }}

// fetcher hands a download out as it arrives.
type fetcher struct {
	cancel context.CancelFunc

	mu      sync.Mutex
	changed sync.Cond // when bytes arrive or are read, and when the download ends
	waiting [][]byte  // arrived but not read yet, in slabs
	read    int       // how much of the first slab has been read
	size    int       // how many bytes are waiting
	done    bool      // the download has ended
	stopped bool      // closed, so what's left is dropped
	err     error     // why the download ended early, or was stopped
}

func (f *fetcher) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.size == 0 && !f.done && !f.stopped {
		f.changed.Wait()
	}
	switch {
	case f.stopped:
		return 0, f.err
	case f.size > 0:
		first := f.waiting[0]
		n := copy(p, first[f.read:])
		f.read += n
		f.size -= n
		if f.read == len(first) {
			slabs.Put((*[chunk]byte)(first[:chunk]))
			f.waiting[0] = nil
			f.waiting, f.read = f.waiting[1:], 0
		}
		f.changed.Broadcast() // room for more
		return n, nil
	case f.err != nil:
		return 0, f.err
	}
	return 0, io.EOF
}

// Close stops the download.
func (f *fetcher) Close() error {
	f.cancel()
	return nil
}

// write keeps downloaded bytes, waiting while ahead of them wait unread.
func (f *fetcher) write(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.size >= ahead && !f.stopped {
		f.changed.Wait()
	}
	if f.stopped {
		return f.err
	}
	f.size += len(b)
	for len(b) > 0 {
		last := len(f.waiting) - 1
		if last < 0 || len(f.waiting[last]) == chunk {
			f.waiting = append(f.waiting, slabs.Get().(*[chunk]byte)[:0])
			last++
		}
		n := copy(f.waiting[last][len(f.waiting[last]):chunk], b)
		f.waiting[last] = f.waiting[last][:len(f.waiting[last])+n]
		b = b[n:]
	}
	f.changed.Broadcast()
	return nil
}

// end marks the download as over; reads get what's left, then err.
func (f *fetcher) end(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.done && !f.stopped {
		f.done, f.err = true, err
	}
	f.changed.Broadcast()
}

// stop drops what's left once the download is stopped.
func (f *fetcher) stop(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped, f.err = true, err
	for _, slab := range f.waiting {
		slabs.Put((*[chunk]byte)(slab[:chunk]))
	}
	f.waiting, f.read, f.size = nil, 0, 0
	f.changed.Broadcast()
}

// download asks for a file a piece at a time.
type download struct {
	client *http.Client
	src    Source
	offset int64 // how much has been passed on
	total  int64 // the file's size, or -1 when unknown
	buf    []byte
	write  func([]byte) error
}

func (d *download) run(ctx context.Context) error {
	for d.total < 0 || d.offset < d.total {
		var done bool
		err := retry(ctx, func() error {
			var err error
			done, err = d.whenReady(ctx)
			return err
		})
		if err != nil || done {
			return err
		}
	}
	return nil
}

// whenReady asks for the next piece, and again every notReadyWait while
// YouTube turns down a link that isn't ready yet.
func (d *download) whenReady(ctx context.Context) (bool, error) {
	for {
		done, err := d.piece(ctx)
		var s *statusError
		if !errors.As(err, &s) || s.code != http.StatusForbidden || !time.Now().Before(d.src.Ready) {
			return done, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(notReadyWait):
		}
	}
}

// piece asks for the next piece and passes it on as it arrives. A piece cut
// short is picked up where it stopped by the next try. done is true once the
// whole file is in.
func (d *download) piece(ctx context.Context) (done bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	quiet := time.AfterFunc(quietFor, cancel)
	defer quiet.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.src.URL, nil)
	if err != nil {
		return false, err
	}
	if d.src.Header != nil {
		req.Header = d.src.Header.Clone()
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", d.offset, d.offset+piece-1))
	resp, err := d.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	want := int64(-1) // how much this answer holds, when it says
	switch {
	case resp.StatusCode == http.StatusOK && d.offset == 0:
		// The server ignores ranges and sends everything in one go.
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && d.offset > 0:
		return true, nil // asked past the end
	case resp.StatusCode != http.StatusPartialContent:
		return false, &statusError{resp.StatusCode}
	default:
		if start, end, total, ok := contentRange(resp.Header.Get("Content-Range")); ok {
			if start != d.offset || end < start || end-start >= piece {
				return false, fmt.Errorf("asked for bytes from %d, got %d-%d", d.offset, start, end)
			}
			d.total, want = total, end-start+1
		}
	}

	n, err := d.pass(resp.Body, quiet)
	switch {
	case err != nil:
		return false, err
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case want >= 0 && n < want:
		return false, io.ErrUnexpectedEOF
	case want < 0 && n < piece:
		return true, nil // a short piece of a file whose size nobody said is the last
	}
	return false, nil
}

// pass hands body on as it arrives and returns how much there was. quiet
// gives the request up when the server goes quiet for quietFor; it's
// stopped while the bytes wait for room ahead.
func (d *download) pass(body io.Reader, quiet *time.Timer) (int64, error) {
	var n int64
	for {
		k, err := body.Read(d.buf)
		if k > 0 {
			quiet.Stop()
			if err := d.write(d.buf[:k]); err != nil {
				return n, err
			}
			quiet.Reset(quietFor)
			n += int64(k)
			d.offset += int64(k)
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// contentRange reads a Content-Range header like "bytes 0-1048575/4767222".
// total is -1 when the server doesn't know it.
func contentRange(h string) (start, end, total int64, ok bool) {
	var size string
	if _, err := fmt.Sscanf(h, "bytes %d-%d/%s", &start, &end, &size); err != nil {
		return 0, 0, 0, false
	}
	total = -1
	if size != "*" {
		if _, err := fmt.Sscan(size, &total); err != nil {
			return 0, 0, 0, false
		}
	}
	return start, end, total, true
}

// statusError is a request answered with an HTTP error.
type statusError struct{ code int }

func (e *statusError) Error() string {
	return fmt.Sprintf("%d %s", e.code, http.StatusText(e.code))
}

// retry runs try until it works, it fails for good, or the retries run out.
func retry(ctx context.Context, try func() error) error {
	for attempt := 0; ; attempt++ {
		err := try()
		if err == nil || !temporary(err) || attempt == len(retries) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retries[attempt]):
		}
	}
}

// temporary reports whether asking again might work. A server that says no,
// like YouTube once a link has expired, won't change its mind, and an answer
// too big stays too big, as does an address refused.
func temporary(err error) bool {
	if errors.Is(err, ErrTooBig) || errors.Is(err, netguard.ErrPrivate) {
		return false
	}
	var s *statusError
	if errors.As(err, &s) {
		return s.code == http.StatusTooManyRequests || s.code >= 500
	}
	return true
}
