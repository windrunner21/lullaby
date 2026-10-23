// Package songs is what lullaby says about songs: a Song and its Audio, the
// errors a song can't be played with, and helpers for YouTube and SoundCloud
// links.
package songs

import (
	"errors"
	"net/http"
	"time"
)

// Song is something that can be played.
type Song struct {
	// ID is the YouTube video ID, or empty for other sites.
	ID string
	// Link is the page people share, like https://www.youtube.com/watch?v=ID.
	Link string
	// Title is the song's name: YouTube Music's for a song found there, like
	// "Snooze" rather than "SZA - Snooze (Official Video)".
	Title string
	// Artist is who made it, or who uploaded it when the site doesn't say.
	Artist string
	// Length is 0 when the site doesn't say.
	Length time.Duration
	// Thumbnail is a link to its cover.
	Thumbnail string
	// Audio is where to download the song. Search results don't have it
	// until they're looked up.
	Audio Audio
}

// Audio is where a song's audio can be downloaded.
type Audio struct {
	// URL is the audio itself, or a playlist of its pieces.
	URL string
	// Header holds what the site wants sent with every download request.
	Header http.Header
	// Size is in bytes, or 0 when unknown.
	Size int64
	// Playlist is true when URL is an HLS playlist of short pieces, the way
	// SoundCloud and live streams come.
	Playlist bool
	// Ready is when YouTube says URL starts working, or zero. It turns links
	// from yt-dlp down for a moment after handing them out.
	Ready time.Time
	// Expires is when URL stops working, or zero when unknown.
	Expires time.Time
}

// The errors Play can fail with for a song, to check with errors.Is.
var (
	// ErrUnplayable is returned for a song the site won't play, like a
	// removed or private video.
	ErrUnplayable = errors.New("can't be played")
	// ErrNoYTDLP is returned for a link that needs yt-dlp when it isn't
	// installed.
	ErrNoYTDLP = errors.New("needs yt-dlp")
	// ErrBotCheck is returned when YouTube wants a sign-in to prove the app
	// isn't a bot, a bot check, and there's no spare account to sign in
	// with, or that didn't help. It's about the internet address asking, not the song: after many
	// requests in a day, YouTube asks this for most videos for hours.
	ErrBotCheck = errors.New("youtube wants proof this isn't a bot")
	// ErrNoAudio is returned when YouTube gives no plain audio link, as when it
	// answers with SABR, its own streaming protocol, only, and there's no
	// yt-dlp to try instead.
	ErrNoAudio = errors.New("no plain audio link")
	// ErrPlaylist is returned for a link to a playlist or album rather than a song.
	ErrPlaylist = errors.New("is a playlist")
	// ErrNoMatch is returned for a song from a catalog like Spotify when YouTube
	// Music has nothing that's clearly the same song.
	ErrNoMatch = errors.New("not found on youtube music")
)
