package sources

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windrunner21/lullaby/internal/netguard"
	"github.com/windrunner21/lullaby/songs"
)

// script is a shell script in a new file, for a program that misbehaves.
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestYTDLPAnswerIsBounded(t *testing.T) {
	// An answer far bigger than any song's stops the program, rather than
	// filling memory.
	start := time.Now()
	out, err := run(context.Background(), script(t, "head -c 20000000 /dev/zero; sleep 30"))
	if !errors.Is(err, songs.ErrUnplayable) || out != nil {
		t.Errorf("run = %d bytes, %v; want ErrUnplayable", len(out), err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %v: the program wasn't stopped when its answer overflowed", took)
	}
}

func TestYTDLPNoticesStaleCookiesAnywhere(t *testing.T) {
	// The warning comes first, then far more than the tail keeps.
	body := `echo "WARNING: The provided YouTube account cookies are no longer valid" >&2; head -c 200000 /dev/zero | tr '\0' x >&2; echo >&2; echo "ERROR: sign in" >&2; exit 1`
	if _, err := run(context.Background(), script(t, body)); !errors.Is(err, errOldCookies) {
		t.Errorf("run = %v, want errOldCookies", err)
	}
}

func TestDeezerShortLinkStaysOnDeezer(t *testing.T) {
	var private bool
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "169.254.169.254" {
			private = true
			return nil, errors.New("reached a cloud's metadata service")
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{
			"Location": {"http://169.254.169.254/latest/meta-data/"},
		}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if _, err := NewDeezer(client, DeezerAPI).Track(context.Background(), "https://deezer.page.link/example"); err == nil {
		t.Error("a short link to elsewhere was taken")
	}
	if private {
		t.Error("followed a short link off Deezer")
	}
}

func TestFinderKeepsOffThePrivateNetwork(t *testing.T) {
	var ran bool
	dl := NewYTDLP("/usr/bin/yt-dlp", "", "")
	dl.run = func(context.Context, string, ...string) ([]byte, error) {
		ran = true
		return []byte(ytdlpSnooze), nil
	}
	f := NewFinder(nil, nil, dl, nil)
	f.GuardLinks = true
	for _, link := range []string{"http://127.0.0.1:2333/v4/info", "http://192.168.0.1/", "http://[::1]/song.mp3"} {
		if _, err := f.Find(context.Background(), link); !errors.Is(err, songs.ErrUnplayable) || !errors.Is(err, netguard.ErrPrivate) {
			t.Errorf("Find(%s) = %v, want ErrUnplayable for a private address", link, err)
		}
	}
	if ran {
		t.Error("yt-dlp was asked about a link on the private network")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
