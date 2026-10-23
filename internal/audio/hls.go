package audio

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// piecesAhead is how many of a playlist's pieces download at once, so
	// one's wait overlaps the others'.
	piecesAhead = 3
	// maxPiece is far more than one piece of a song, about 120 KB at
	// SoundCloud.
	maxPiece = 8 << 20
	// maxPlaylist is far more than a playlist: a 3-hour mix's, of about
	// 1,100 pieces with long signed links, is under 0.5 MB.
	maxPlaylist = 2 << 20
	// maxPieces is far more pieces than a song has: over 27 hours of
	// SoundCloud's, the header piece included.
	maxPieces = 10000
	// maxLink and maxLinks are far longer than a piece's link, signed ones
	// included, and than all of a playlist's links together.
	maxLink  = 16 << 10
	maxLinks = 16 << 20
	// pieceTimeout is how long one piece or playlist may take.
	pieceTimeout = 30 * time.Second
)

var (
	// ErrNotSimple is returned by FetchHLS for playlists it leaves to ffmpeg:
	// live ones, encrypted ones, and ones made of byte ranges.
	ErrNotSimple = errors.New("not a finished, plain playlist")
	// ErrTooBig is returned for a playlist or piece far bigger than any
	// song's, which isn't left to ffmpeg either.
	ErrTooBig = errors.New("too big for a song")
)

// FetchHLS downloads the pieces of a finished HLS playlist at src, the way
// SoundCloud sends songs, and passes them on as one stream. ffmpeg, started
// while the song was looked up, can then read it from Feed instead of
// starting late to read the playlist itself: about half a second sooner.
// It returns ErrNotSimple for playlists it leaves to Open.
func FetchHLS(ctx context.Context, client *http.Client, src Source) (io.ReadCloser, error) {
	pieces, err := hlsPieces(ctx, client, src)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &fetcher{cancel: cancel}
	f.changed.L = &f.mu
	context.AfterFunc(ctx, func() { f.stop(ctx.Err()) })
	go func() { f.end(getPieces(ctx, client, src.Header, pieces, f.write)) }()
	return f, nil
}

// hlsPieces reads the playlist at src, following a playlist of playlists to
// its first, and returns its pieces in playing order: the header piece
// first, if it has one.
func hlsPieces(ctx context.Context, client *http.Client, src Source) ([]string, error) {
	link := src.URL
	for range 2 {
		if len(link) > maxLink {
			return nil, fmt.Errorf("hls: %w: a link of %d bytes", ErrTooBig, len(link))
		}
		body, err := getWhole(ctx, client, src.Header, link, maxPlaylist)
		if err != nil {
			return nil, err
		}
		base, err := url.Parse(link)
		if err != nil {
			return nil, err
		}
		pieces, variant, err := parsePlaylist(string(body), base)
		if err != nil || variant == "" {
			return pieces, err
		}
		link = variant
	}
	return nil, fmt.Errorf("hls: %w: playlists of playlists of playlists", ErrNotSimple)
}

// parsePlaylist returns a media playlist's pieces, or the first playlist a
// playlist of playlists points to, with relative links made whole.
func parsePlaylist(text string, base *url.URL) (pieces []string, variant string, err error) {
	resolve := func(ref string) (string, error) {
		u, err := base.Parse(strings.TrimSpace(ref))
		if err != nil {
			return "", fmt.Errorf("hls: %w", err)
		}
		link := u.String()
		if len(link) > maxLink {
			return "", fmt.Errorf("hls: %w: a link of %d bytes", ErrTooBig, len(link))
		}
		return link, nil
	}
	// The pieces' links, with the playlist's own in front of each relative
	// one, can come to far more than the playlist.
	size := 0
	add := func(ref string) error {
		link, err := resolve(ref)
		if err != nil {
			return err
		}
		if size += len(link); size > maxLinks {
			return fmt.Errorf("hls: %w: over %d MB of links", ErrTooBig, maxLinks>>20)
		}
		if pieces = append(pieces, link); len(pieces) > maxPieces {
			return fmt.Errorf("hls: %w: over %d pieces", ErrTooBig, maxPieces)
		}
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(text), "#EXTM3U") {
		return nil, "", errors.New("hls: not a playlist")
	}

	// A playlist left to ffmpeg is still read to the end, so the limits hold
	// for it too.
	var notSimple error
	finished, master := false, false
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 64<<10), maxPlaylist)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			master = true
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			finished = true
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE"):
			notSimple = cmp.Or(notSimple, fmt.Errorf("hls: %w: byte ranges", ErrNotSimple))
		case strings.HasPrefix(line, "#EXT-X-KEY") && !strings.Contains(line, "METHOD=NONE"):
			notSimple = cmp.Or(notSimple, fmt.Errorf("hls: %w: encrypted", ErrNotSimple))
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			ref, ok := attribute(line, "URI")
			if !ok || strings.Contains(line, "BYTERANGE=") {
				notSimple = cmp.Or(notSimple, fmt.Errorf("hls: %w: header piece %q", ErrNotSimple, line))
			}
			if !ok {
				continue
			}
			// Counted even when it's left to ffmpeg, so the limits hold.
			if err := add(ref); err != nil {
				return nil, "", err
			}
		case strings.HasPrefix(line, "#"):
		case master:
			link, err := resolve(line)
			if err != nil {
				return nil, "", err
			}
			return nil, link, nil
		default:
			if err := add(line); err != nil {
				return nil, "", err
			}
		}
	}
	switch err := scanner.Err(); {
	case err != nil:
		return nil, "", fmt.Errorf("hls: %w", err)
	case notSimple != nil:
		return nil, "", notSimple
	case !finished:
		return nil, "", fmt.Errorf("hls: %w: live", ErrNotSimple)
	case len(pieces) == 0:
		return nil, "", errors.New("hls: an empty playlist")
	}
	return pieces, "", nil
}

// attribute reads a quoted attribute like URI="..." from a playlist tag.
func attribute(line, name string) (string, bool) {
	_, after, ok := strings.Cut(line, name+`="`)
	if !ok {
		return "", false
	}
	value, _, ok := strings.Cut(after, `"`)
	return value, ok
}

// getPieces downloads pieces, piecesAhead at a time, and writes them in order.
func getPieces(ctx context.Context, client *http.Client, header http.Header, pieces []string, write func([]byte) error) error {
	type got struct {
		data []byte
		err  error
	}
	// Piece i's answer comes in results[i%piecesAhead], which is free again
	// once piece i-piecesAhead is written.
	results := make([]chan got, piecesAhead)
	start := func(i int) {
		answer := make(chan got, 1)
		results[i%piecesAhead] = answer
		go func() {
			data, err := getWhole(ctx, client, header, pieces[i], maxPiece)
			answer <- got{data, err}
		}()
	}
	for i := range min(piecesAhead, len(pieces)) {
		start(i)
	}
	for i := range pieces {
		var r got
		select {
		case r = <-results[i%piecesAhead]:
		case <-ctx.Done():
			return ctx.Err()
		}
		if r.err != nil {
			return r.err
		}
		if err := write(r.data); err != nil {
			return err
		}
		if next := i + piecesAhead; next < len(pieces) {
			start(next)
		}
	}
	return nil
}

// getWhole downloads one piece or playlist of at most limit bytes, trying
// again a few times.
func getWhole(ctx context.Context, client *http.Client, header http.Header, link string, limit int64) ([]byte, error) {
	var data []byte
	err := retry(ctx, func() error {
		ctx, cancel := context.WithTimeout(ctx, pieceTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if err != nil {
			return err
		}
		if header != nil {
			req.Header = header.Clone()
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return &statusError{resp.StatusCode}
		}
		switch n := resp.ContentLength; {
		case n > limit:
			return fmt.Errorf("hls: %w: %d bytes", ErrTooBig, n)
		case n >= 0:
			// In one go, instead of growing to fit.
			data = make([]byte, n)
			_, err = io.ReadFull(resp.Body, data)
			return err
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err == nil && int64(len(data)) > limit {
			return fmt.Errorf("hls: %w: over %d bytes", ErrTooBig, limit)
		}
		return err
	})
	return data, err
}
