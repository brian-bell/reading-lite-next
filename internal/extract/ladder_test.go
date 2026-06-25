package extract

// White-box tests for the pure tier-selection logic. These exercise the ladder
// ordering, the sufficiency gate, and the raw_only string helpers without
// touching the HTML libraries, so the selection contract is pinned independently
// of go-readability / html-to-markdown behavior.

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestSelectTier_FirstSufficientWins(t *testing.T) {
	t.Parallel()

	got := selectTier(
		func() (Article, bool) { return Article{Mode: ModeReadability}, true },
		func() (Article, bool) { return Article{Mode: ModeRawDOM}, true },
		func() (Article, bool) { return Article{Mode: ModeRawOnly}, true },
	)
	if got.Mode != ModeReadability {
		t.Fatalf("Mode = %q, want %q", got.Mode, ModeReadability)
	}
}

func TestSelectTier_FallsThroughToFloor(t *testing.T) {
	t.Parallel()

	got := selectTier(
		func() (Article, bool) { return Article{Mode: ModeReadability}, false },
		func() (Article, bool) { return Article{Mode: ModeRawDOM}, false },
		func() (Article, bool) { return Article{Mode: ModeRawOnly}, true },
	)
	if got.Mode != ModeRawOnly {
		t.Fatalf("Mode = %q, want %q (floor)", got.Mode, ModeRawOnly)
	}
}

func TestSelectTier_StopsAtFirstSuccess(t *testing.T) {
	t.Parallel()

	rawDOMRan := false
	got := selectTier(
		func() (Article, bool) { return Article{Mode: ModeReadability}, true },
		func() (Article, bool) { rawDOMRan = true; return Article{Mode: ModeRawDOM}, true },
		func() (Article, bool) { return Article{Mode: ModeRawOnly}, true },
	)
	if got.Mode != ModeReadability {
		t.Fatalf("Mode = %q, want %q", got.Mode, ModeReadability)
	}
	if rawDOMRan {
		t.Fatal("selectTier evaluated a later tier after an earlier one succeeded")
	}
}

func TestSufficient_WordThreshold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", false},
		{"one below threshold", words(minReadableWords - 1), false},
		{"at threshold", words(minReadableWords), true},
		{"above threshold", words(minReadableWords + 50), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := sufficient(c.text); got != c.want {
				t.Fatalf("sufficient(%d words) = %v, want %v", wordCount(c.text), got, c.want)
			}
		})
	}
}

func TestWordCount_CountsFields(t *testing.T) {
	t.Parallel()

	cases := map[string]int{
		"":                      0,
		"one":                   1,
		"  spaced   out  words": 3,
		"line\nbreaks\ttabs":    3,
	}
	for in, want := range cases {
		if got := wordCount(in); got != want {
			t.Errorf("wordCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBodyText_KeepsTextStripsTags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "<p>hello <b>world</b></p>", "hello world"},
		{"keeps script body", "<body><script>var x = 1;</script></body>", "var x = 1;"},
		{"collapses whitespace", "<div>a\n\n   b\t c</div>", "a b c"},
		{"no tags passes through", "plain bytes", "plain bytes"},
		// The floor exists for JS-heavy pages, and JS is full of bare '<'/'>'
		// (`i < n`). The HTML tokenizer preserves them inside raw-text elements, so
		// a body script must survive intact — a hand-rolled tag stripper would have
		// dropped everything between the '<' and the next '>'. (Scripts are wrapped
		// in <body>: a bare top-level <script> is parsed into <head>, which the
		// body-only floor walk intentionally skips.)
		{"keeps angle brackets in script", "<body><script>for (i = 0; i < n; i++) {}</script></body>", "for (i = 0; i < n; i++) {}"},
		{"keeps angle brackets in style", "<body><style>a < b { color: red }</style></body>", "a < b { color: red }"},
		{"keeps free-text comparison", "price > 5 and qty < 3", "price > 5 and qty < 3"},
		// Head text is metadata, not content: a <title> is excluded from body text.
		{"skips head title", "<html><head><title>Login</title></head><body>real content</body></html>", "real content"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			doc, err := html.Parse(strings.NewReader(c.in))
			if err != nil {
				t.Fatalf("parse %q: %v", c.in, err)
			}
			if got := bodyText(doc); got != c.want {
				t.Fatalf("bodyText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// words builds a string of n single-character whitespace-separated tokens.
func words(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2)
	for i := range n {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, 'w')
	}
	return string(b)
}
