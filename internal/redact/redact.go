// Package redact hides what could give access in links before they're logged
// or handed back: query values, which can hold tokens like a private
// SoundCloud song's secret_token or a music server's API key, and passwords.
package redact

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

var (
	links = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)
	// userinfo runs to the last @ before the path: a password can hold one.
	userinfo  = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)[^/?#]*@`)
	authority = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://([^/?#]*)`)
	values    = regexp.MustCompile(`([?&])([^=&#]*)=([^&#]*)`)
	videoID   = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
)

// Link hides a link's query values and its password, and a private
// SoundCloud song's share token. A YouTube video's ID stays, since it never
// gives access and tells links apart.
func Link(link string) string {
	link = userinfo.ReplaceAllString(link, "${1}…@")
	host := ""
	if m := authority.FindStringSubmatch(link); m != nil {
		host = strings.ToLower(m[1])
		host = host[strings.LastIndexByte(host, '@')+1:]
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
		host = strings.TrimPrefix(strings.TrimPrefix(host, "www."), "m.")
	}
	youtube := host == "youtube.com" || host == "music.youtube.com" || host == "youtube-nocookie.com"
	path, query, hasQuery := strings.Cut(link, "?")
	if host == "soundcloud.com" || strings.HasSuffix(host, ".soundcloud.com") {
		path = hideShareTokens(path)
	}
	if !hasQuery {
		return path
	}
	return path + "?" + values.ReplaceAllStringFunc("?"+query, func(m string) string {
		parts := values.FindStringSubmatch(m)
		if youtube && parts[2] == "v" && videoID.MatchString(parts[3]) {
			return m
		}
		return parts[1] + parts[2] + "=…"
	})[1:]
}

// hideShareTokens hides the share tokens in a SoundCloud link's path, escaped
// ones like /s-%41bCdE included.
func hideShareTokens(path string) string {
	scheme, rest, ok := strings.Cut(path, "://")
	if !ok {
		return path
	}
	authority, p, ok := strings.Cut(rest, "/")
	if !ok {
		return path
	}
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if plain, err := url.PathUnescape(part); err == nil && strings.HasPrefix(plain, "s-") {
			parts[i] = "s-…"
		}
	}
	return scheme + "://" + authority + "/" + strings.Join(parts, "/")
}

// Text hides the query values and passwords of every link in s.
func Text(s string) string {
	return links.ReplaceAllStringFunc(s, func(m string) string {
		// Punctuation after a link, like the colon in "link: 404", isn't
		// the link's.
		link := strings.TrimRight(m, ".,:;)]}")
		return Link(link) + m[len(link):]
	})
}

// Error is err with the links in its message redacted. errors.Is and
// errors.As still see err.
func Error(err error) error {
	if err == nil {
		return nil
	}
	if msg := Text(err.Error()); msg != err.Error() {
		return redacted{msg, err}
	}
	return err
}

type redacted struct {
	msg string
	err error
}

func (r redacted) Error() string { return r.msg }
func (r redacted) Unwrap() error { return r.err }

// Attr is err for a log, as "err", with the links in it redacted.
func Attr(err error) slog.Attr {
	if err == nil {
		return slog.String("err", "")
	}
	return slog.String("err", Text(err.Error()))
}
