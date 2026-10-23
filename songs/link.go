package songs

import (
	"net/url"
	"strings"
)

// YouTubeID returns the video ID in a YouTube link, like youtube.com/watch?v=ID,
// youtu.be/ID, music.youtube.com/watch?v=ID or youtube.com/shorts/ID.
func YouTubeID(link string) (string, bool) {
	link = strings.TrimSpace(link)
	if !strings.Contains(link, "://") {
		link = "https://" + link
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	host = strings.TrimPrefix(host, "m.")
	path := strings.Split(strings.Trim(u.Path, "/"), "/")

	var id string
	switch host {
	case "youtu.be":
		id = path[0]
	case "youtube.com", "music.youtube.com", "youtube-nocookie.com":
		switch {
		case len(path) == 1 && path[0] == "watch":
			id = u.Query().Get("v")
		case len(path) >= 2 && (path[0] == "shorts" || path[0] == "live" || path[0] == "embed"):
			id = path[1]
		}
	}
	if !validID(id) {
		return "", false
	}
	return id, true
}

// IsYouTube reports whether link is YouTube's: a video, or anything else
// there, like a playlist or a channel.
func IsYouTube(link string) bool {
	switch strings.TrimPrefix(host(link), "m.") {
	case "youtube.com", "music.youtube.com", "youtu.be", "youtube-nocookie.com":
		return true
	}
	return false
}

// WatchLink is the link to the YouTube video with the given ID.
func WatchLink(id string) string {
	return "https://www.youtube.com/watch?v=" + id
}

// validID reports whether id looks like a YouTube video ID: 11 letters,
// digits, dashes and underscores.
func validID(id string) bool {
	if len(id) != 11 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// IsSoundCloud reports whether link is a SoundCloud link, short ones included.
func IsSoundCloud(link string) bool {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return false
	}
	switch strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") {
	case "soundcloud.com", "m.soundcloud.com", "on.soundcloud.com":
		return true
	}
	return false
}

// host is a link's host in lower case, without "www.", or empty if it isn't
// a link.
func host(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}
