package reading_test

import (
	"testing"

	"github.com/bbell/reading-lite/internal/reading"
)

func TestURLKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "lowercase scheme and host, upgrade http, keep path case",
			input: "HTTP://Example.COM/Path",
			want:  "https://example.com/Path",
		},
		{
			name:  "strip tracking params",
			input: "https://example.com/a?UTM_SOURCE=x&id=7",
			want:  "https://example.com/a?id=7",
		},
		{
			name:  "trim trailing slash on non-root",
			input: "https://example.com/a/",
			want:  "https://example.com/a",
		},
		{
			name:  "drop fragment",
			input: "https://example.com/a#frag",
			want:  "https://example.com/a",
		},
		{
			name:  "root path canonical",
			input: "https://example.com",
			want:  "https://example.com/",
		},
		{
			name:  "strip default http port before https upgrade",
			input: "http://example.com:80/a",
			want:  "https://example.com/a",
		},
		{
			name:  "strip default https port",
			input: "https://example.com:443/a",
			want:  "https://example.com/a",
		},
		{
			name:  "strip trailing dot with default port",
			input: "https://example.com.:443/a",
			want:  "https://example.com/a",
		},
		{
			name:  "preserve explicit non-default port",
			input: "https://example.com:80/a",
			want:  "https://example.com:80/a",
		},
		{
			name:  "strip trailing dot with non-default port",
			input: "https://example.com.:8443/a",
			want:  "https://example.com:8443/a",
		},
		{
			name:  "preserve bracketed ipv6 host",
			input: "https://[::1]/a",
			want:  "https://[::1]/a",
		},
		{
			name:  "strip default ipv6 port without losing brackets",
			input: "https://[::1]:443/a",
			want:  "https://[::1]/a",
		},
		{
			name:  "canonicalize mobile youtube watch urls",
			input: "https://m.youtube.com/watch?v=ID&t=10",
			want:  "https://www.youtube.com/watch?v=ID",
		},
		{
			name:  "do not collapse youtube non-watch pages with video query",
			input: "https://www.youtube.com/playlist?v=ID&list=PL",
			want:  "https://www.youtube.com/playlist?list=PL&v=ID",
		},
		{
			name:  "expand youtu.be short links",
			input: "https://youtu.be/ID",
			want:  "https://www.youtube.com/watch?v=ID",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := reading.URLKey(tc.input)
			if err != nil {
				t.Fatalf("URLKey(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("URLKey(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestURLKey_PreservesDistinctEscapedPaths(t *testing.T) {
	t.Parallel()

	encodedSlash, err := reading.URLKey("https://example.com/a%2Fb")
	if err != nil {
		t.Fatalf("URLKey encoded slash: %v", err)
	}
	pathSlash, err := reading.URLKey("https://example.com/a/b")
	if err != nil {
		t.Fatalf("URLKey path slash: %v", err)
	}
	if encodedSlash == pathSlash {
		t.Fatalf("URLKey encoded slash collapsed into path slash: both %q", encodedSlash)
	}

	if got, want := encodedSlash, "https://example.com/a%2Fb"; got != want {
		t.Fatalf("URLKey encoded slash = %q, want %q", got, want)
	}
}

func TestURLKey_PreservesEscapedSpaces(t *testing.T) {
	t.Parallel()

	got, err := reading.URLKey("https://example.com/a%20b")
	if err != nil {
		t.Fatalf("URLKey escaped space: %v", err)
	}
	if want := "https://example.com/a%20b"; got != want {
		t.Fatalf("URLKey escaped space = %q, want %q", got, want)
	}
}

func TestURLKey_PreservesDuplicateSlashesAndDotSegments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{"https://example.com/a//b", "https://example.com/a//b"},
		{"https://example.com/a/./b", "https://example.com/a/./b"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			got, err := reading.URLKey(tc.input)
			if err != nil {
				t.Fatalf("URLKey(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("URLKey(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestURLKey_TrailingSlashMatchesBarePath(t *testing.T) {
	t.Parallel()

	withSlash, err := reading.URLKey("https://example.com/a/")
	if err != nil {
		t.Fatalf("URLKey with slash: %v", err)
	}
	withoutSlash, err := reading.URLKey("https://example.com/a")
	if err != nil {
		t.Fatalf("URLKey without slash: %v", err)
	}

	if withSlash != withoutSlash {
		t.Fatalf("URLKey trailing slash mismatch: %q != %q", withSlash, withoutSlash)
	}
}

func TestURLKey_RejectsUnsupportedInputs(t *testing.T) {
	t.Parallel()

	cases := []string{
		"not a url",
		"ftp://x",
		"javascript:alert(1)",
		"https://:443/a",
		"https://example.com:bad/a",
		"https://user:pass@example.com/a",
		"https://youtu.be/%41",
		"https://youtu.be/a%20b",
		"https://youtu.be/a/b",
		"https://youtu.be/a%2Fb",
	}

	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			if got, err := reading.URLKey(input); err == nil {
				t.Fatalf("URLKey(%q) = %q, nil error; want error", input, got)
			}
		})
	}
}

func TestClassifySource(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key  string
		want reading.SourceKind
	}{
		{"https://example.com/post", reading.SourceWeb},
		{"https://www.youtube.com/watch?v=ID", reading.SourceYouTube},
		{"https://reddit.com/r/golang/comments/abc/title", reading.SourceReddit},
		{"https://example.com/notes.md", reading.SourceMarkdown},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			if got := reading.ClassifySource(tc.key); got != tc.want {
				t.Fatalf("ClassifySource(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}
