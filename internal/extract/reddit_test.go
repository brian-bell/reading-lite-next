package extract_test

import (
	"testing"

	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/reading"
)

func TestReddit_Guidance(t *testing.T) {
	t.Parallel()

	// The canonical operator-facing message is exact and stable: it is what the
	// user sees when a Reddit reading fails, and the pipeline reuses it verbatim.
	const want = "reddit cannot be fetched automatically; export the post or comment and import it as markdown"
	if extract.RedditGuidance != want {
		t.Fatalf("RedditGuidance = %q, want %q", extract.RedditGuidance, want)
	}

	// The source classifier routes Reddit URLs (across host variants, in their
	// canonical key form) to the reddit kind — the routing that triggers the
	// guidance path instead of a doomed fetch.
	redditURLs := []string{
		"https://www.reddit.com/r/golang/comments/abc/post",
		"https://reddit.com/r/golang/comments/abc/post/",
		"https://old.reddit.com/r/golang/comments/abc/post",
	}
	for _, raw := range redditURLs {
		key, err := reading.URLKey(raw)
		if err != nil {
			t.Fatalf("URLKey(%q): %v", raw, err)
		}
		if got := reading.ClassifySource(key); got != reading.SourceReddit {
			t.Errorf("ClassifySource(%q) = %q, want reddit", key, got)
		}
	}
}
