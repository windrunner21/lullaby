package sources

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// maxPage is far more than any page or answer the sites send about one song.
const maxPage = 4 << 20

// host is a link's host in lower case, without "www.", or empty if it isn't
// a link.
func host(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// followShort turns a short share link into the link it stands for, by
// reading where it redirects to without following it: the page it leads to
// isn't needed.
func followShort(ctx context.Context, client *http.Client, link string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(link), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserAgent)
	only := *client
	only.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := only.Do(req)
	if err != nil {
		return "", fmt.Errorf("short link: %w", err)
	}
	resp.Body.Close()
	to, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("short link: %s", resp.Status)
	}
	return to.String(), nil
}

// getPage asks for target as a browser would, and returns the answer and its
// status.
func getPage(ctx context.Context, client *http.Client, target string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", browserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	return body, resp.StatusCode, err
}

// logger is l, or slog's default logger when l is nil.
func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
