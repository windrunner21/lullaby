package redact

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	tests := map[string]string{
		`Get "https://api-v2.soundcloud.com/media/x/stream?client_id=abc&track_authorization=eyJ0": timeout`: `Get "https://api-v2.soundcloud.com/media/x/stream?client_id=…&track_authorization=…": timeout`,
		"soundcloud https://soundcloud.com/artist/song/s-AbCdE?secret_token=s-AbCdE: 404":                    "soundcloud https://soundcloud.com/artist/song/s-…?secret_token=…: 404",
		"see https://soundcloud.com for more":                                                                "see https://soundcloud.com for more",
		"https://soundcloud.com/artist/song/s-%41bCdE":                                                       "https://soundcloud.com/artist/song/s-…",
		"https://on.soundcloud.com/artist/song/s-AbCdE":                                                      "https://on.soundcloud.com/artist/song/s-…",
		"https://media.example/audio?v=access-secret":                                                        "https://media.example/audio?v=…",
		"https://user:password@SecretSuffix@media.example/audio":                                             "https://…@media.example/audio",
		"https://music.youtube.com/watch?v=not-an-id-at-all":                                                 "https://music.youtube.com/watch?v=…",
		"http://admin:hunter2@192.168.0.5:4533/rest/stream?u=me&p=pass&id=7":                                 "http://…@192.168.0.5:4533/rest/stream?u=…&p=…&id=…",
		"youtube https://www.youtube.com/watch?v=dQw4w9WgXcQ&si=tracking":                                    "youtube https://www.youtube.com/watch?v=dQw4w9WgXcQ&si=…",
		"no links here, just a ? and a=b":                                                                    "no links here, just a ? and a=b",
	}
	for in, want := range tests {
		if got := Text(in); got != want {
			t.Errorf("Text(%q)\n = %q\nwant %q", in, got, want)
		}
	}
}

var errSecret = errors.New("can't be played")

func TestError(t *testing.T) {
	err := Error(fmt.Errorf("https://a.example/x?token=hunter2: %w", errSecret))
	if strings.Contains(err.Error(), "hunter2") || !errors.Is(err, errSecret) {
		t.Errorf("Error = %q, is errSecret %v; want the token hidden and the error kept", err, errors.Is(err, errSecret))
	}
	plain := errors.New("nothing to hide")
	if Error(plain) != plain || Error(nil) != nil {
		t.Error("an error without links was changed")
	}
}
