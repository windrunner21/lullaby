package audio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// noise is n bytes that stand in for a song.
func noise(n int) []byte {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{}).Read(b)
	return b
}

// serve starts a server that answers with h and counts the requests.
func serve(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// ranges answers range requests for data, like YouTube's video servers.
func ranges(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "song.webm", time.Time{}, bytes.NewReader(data))
	}
}

func readAll(t *testing.T, r io.ReadCloser) []byte {
	t.Helper()
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestFetchInPieces(t *testing.T) {
	data := noise(2*piece + piece/2)
	srv, n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "test-agent" {
			http.Error(w, "the site's headers went missing", http.StatusForbidden)
			return
		}
		ranges(data)(w, r)
	})
	got := readAll(t, Fetch(context.Background(), srv.Client(), Source{URL: srv.URL, Header: http.Header{"User-Agent": {"test-agent"}}}))
	if !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
	if n.Load() != 3 {
		t.Errorf("%d requests, want 3 pieces", n.Load())
	}
}

func TestFetcherKeepsOrder(t *testing.T) {
	// Bytes written and read in every size, across slabs, come out as they went in.
	data := noise(5*chunk + 123)
	f := &fetcher{cancel: func() {}}
	f.changed.L = &f.mu
	go func() {
		rest := data
		for i := 0; len(rest) > 0; i++ {
			n := min([]int{1, chunk - 1, 2, chunk, 3 * chunk / 2, 77}[i%6], len(rest))
			f.write(rest[:n])
			rest = rest[n:]
		}
		f.end(nil)
	}()
	var got []byte
	for i := 0; ; i++ {
		buf := make([]byte, []int{1, 1000, chunk, chunk + 5, 3}[i%5])
		n, err := f.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
}

func TestFetchExactPieces(t *testing.T) {
	data := noise(2 * piece)
	srv, n := serve(t, ranges(data))
	if got := readAll(t, Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
	if n.Load() != 2 {
		t.Errorf("%d requests, want 2 and none past the end", n.Load())
	}
}

func TestFetchWithoutRanges(t *testing.T) {
	data := noise(piece + 1234)
	srv, n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(data) // ignores the Range header
	})
	if got := readAll(t, Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
	if n.Load() != 1 {
		t.Errorf("%d requests, want 1", n.Load())
	}
}

func TestFetchRetriesABusyServer(t *testing.T) {
	data := noise(piece + piece/2)
	var failed atomic.Bool
	srv, n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-1048575" && !failed.Swap(true) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		ranges(data)(w, r)
	})
	if got := readAll(t, Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
	if n.Load() != 3 {
		t.Errorf("%d requests, want 2 pieces and the busy one again", n.Load())
	}
}

func TestFetchPicksUpWhereItWasCut(t *testing.T) {
	data := noise(piece + piece/2)
	var cut atomic.Bool
	var asked []string
	var mu sync.Mutex
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Header.Get("Range"))
		mu.Unlock()
		if !cut.Swap(true) {
			// Promise the whole piece, send a third of it, and hang up.
			w.Header().Set("Content-Range", "bytes 0-1048575/"+strconv.Itoa(len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(piece))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[:300000])
			return
		}
		ranges(data)(w, r)
	})
	if got := readAll(t, Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 2 || asked[1] != "bytes=300000-1348575" {
		t.Errorf("asked for %q, want the second request to start where the first was cut", asked)
	}
}

func TestFetchPassesBytesOnRightAway(t *testing.T) {
	data := noise(piece)
	release := make(chan struct{})
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-1048575/"+strconv.Itoa(len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(piece))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[:4096])
		w.(http.Flusher).Flush()
		<-release // the rest of the piece is slow to come
		w.Write(data[4096:])
	})
	defer close(release)
	f := Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})
	defer f.Close()

	got := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(f, make([]byte, 4096))
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first bytes waited for the rest of the piece")
	}
}

func TestFetchGivesUp(t *testing.T) {
	srv, n := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	})
	f := Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})
	defer f.Close()
	if _, err := io.ReadAll(f); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want the 403", err)
	}
	if n.Load() != 1 {
		t.Errorf("%d requests, want 1: a site that says no won't change its mind", n.Load())
	}
}

func TestFetchWaitsForANewLink(t *testing.T) {
	data := noise(piece / 2)
	var refused atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if refused.Add(1) <= 2 {
			http.Error(w, "not yet", http.StatusForbidden)
			return
		}
		ranges(data)(w, r)
	})
	src := Source{URL: srv.URL, Ready: time.Now().Add(5 * time.Second)}
	start := time.Now()
	if got := readAll(t, Fetch(context.Background(), srv.Client(), src)); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
	// Two refusals cost two short waits, not the 5 s until Ready.
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v, want it to start as soon as the link worked", took)
	}
}

func TestFetchStaysAhead(t *testing.T) {
	data := noise(24 * piece)
	srv, n := serve(t, ranges(data))
	f := Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})

	// Nobody reads yet, so the download should stop about 16 pieces ahead.
	last, stable := int32(-1), time.Now()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && time.Since(stable) < 200*time.Millisecond; {
		if now := n.Load(); now != last {
			last, stable = now, time.Now()
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last > ahead/piece+2 {
		t.Errorf("downloaded %d pieces with nobody reading, want at most %d", last, ahead/piece+2)
	}
	if got := readAll(t, f); !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want the same %d", len(got), len(data))
	}
}

func TestFetchClose(t *testing.T) {
	srv, _ := serve(t, ranges(noise(24*piece)))
	f := Fetch(context.Background(), srv.Client(), Source{URL: srv.URL})
	f.Close()
	if _, err := io.ReadAll(f); !errors.Is(err, context.Canceled) {
		t.Errorf("reading after Close: %v, want context.Canceled", err)
	}
}

func TestContentRange(t *testing.T) {
	tests := []struct {
		in                string
		start, end, total int64
		ok                bool
	}{
		{"bytes 0-1048575/4767222", 0, 1048575, 4767222, true},
		{"bytes 1048576-2097151/*", 1048576, 2097151, -1, true},
		{"", 0, 0, 0, false},
		{"bytes */4767222", 0, 0, 0, false},
	}
	for _, tt := range tests {
		start, end, total, ok := contentRange(tt.in)
		if start != tt.start || end != tt.end || total != tt.total || ok != tt.ok {
			t.Errorf("contentRange(%q) = %d, %d, %d, %v", tt.in, start, end, total, ok)
		}
	}
}
