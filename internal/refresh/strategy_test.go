package refresh

import (
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestWithoutURLName(t *testing.T) {
	tests := []struct {
		name string
		fp   domain.Fingerprint
		urls []string
		want string
	}{
		{"name from the path", domain.Fingerprint{Filename: "movie.mp4"}, []string{"https://x/dl/movie.mp4?sig=1"}, ""},
		{"case insensitive", domain.Fingerprint{Filename: "Movie.MP4"}, []string{"https://x/movie.mp4"}, ""},
		{"escaped path", domain.Fingerprint{Filename: "my movie.mp4"}, []string{"https://x/my%20movie.mp4"}, ""},
		{"name from a header", domain.Fingerprint{Filename: "movie.mp4"}, []string{"https://x/download?id=7"}, "movie.mp4"},
		{"any of the urls", domain.Fingerprint{Filename: "a.bin"}, []string{"https://x/other", "https://y/a.bin"}, ""},
		{"no name", domain.Fingerprint{Size: 5}, []string{"https://x/a"}, ""},
		{"bad url", domain.Fingerprint{Filename: "a.bin"}, []string{"://bad"}, "a.bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withoutURLName(tt.fp, tt.urls...).Filename; got != tt.want {
				t.Errorf("Filename = %q, want %q", got, tt.want)
			}
		})
	}
}
