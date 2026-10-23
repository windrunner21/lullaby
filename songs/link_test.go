package songs

import "testing"

func TestYouTubeID(t *testing.T) {
	tests := []struct {
		link string
		want string // empty when it isn't a YouTube video link
	}{
		{"https://www.youtube.com/watch?v=LDY_XyxBu8A", "LDY_XyxBu8A"},
		{"https://www.youtube.com/watch?v=LDY_XyxBu8A&list=RDLDY_XyxBu8A&t=42s", "LDY_XyxBu8A"},
		{"https://m.youtube.com/watch?v=LDY_XyxBu8A", "LDY_XyxBu8A"},
		{"https://music.youtube.com/watch?v=ZqSlV5LmrTg&si=abc", "ZqSlV5LmrTg"},
		{"https://youtu.be/LDY_XyxBu8A?si=abc", "LDY_XyxBu8A"},
		{"https://www.youtube.com/shorts/LDY_XyxBu8A", "LDY_XyxBu8A"},
		{"https://www.youtube.com/live/LDY_XyxBu8A?feature=share", "LDY_XyxBu8A"},
		{"https://www.youtube-nocookie.com/embed/LDY_XyxBu8A", "LDY_XyxBu8A"},
		{"youtu.be/LDY_XyxBu8A", "LDY_XyxBu8A"},
		{"  https://YouTube.com/watch?v=LDY_XyxBu8A  ", "LDY_XyxBu8A"},

		{"https://www.youtube.com/playlist?list=PL123", ""},
		{"https://www.youtube.com/@SZA", ""},
		{"https://www.youtube.com/watch?v=short", ""},
		{"https://www.youtube.com/watch?v=LDY_XyxBu8A%22", ""},
		{"https://notyoutube.com/watch?v=LDY_XyxBu8A", ""},
		{"https://soundcloud.com/sza/snooze", ""},
		{"sza snooze", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got, ok := YouTubeID(tt.link)
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("YouTubeID(%q) = %q, %v; want %q", tt.link, got, ok, tt.want)
		}
	}
}
