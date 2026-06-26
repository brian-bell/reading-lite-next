package bookmarks_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/bbell/reading-lite/internal/bookmarks"
)

func TestParse_NetscapeHTMLFixtureExtractsURLs(t *testing.T) {
	t.Parallel()

	in := `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<DL><p>
<DT><A HREF="https://example.com/one">One</A>
<DT><A HREF="https://example.com/two?x=1">Two</A>
</DL>`

	got, err := bookmarks.Parse([]byte(in), "text/html")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []string{"https://example.com/one", "https://example.com/two?x=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse() = %#v, want %#v", got, want)
	}
}

func TestParse_JSONArrayOfObjectsExtractsURLs(t *testing.T) {
	t.Parallel()

	got, err := bookmarks.Parse([]byte(`[{"url":"https://a.test"},{"url":"https://b.test"}]`), "application/json")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []string{"https://a.test", "https://b.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse() = %#v, want %#v", got, want)
	}
}

func TestParse_JSONObjectPreservesBookmarksThenHTMLAndDuplicates(t *testing.T) {
	t.Parallel()

	in := []byte(`{
		"bookmarks": [
			{"url":"https://one.test"},
			{"url":"https://dup.test"}
		],
		"html": "<a href=\"https://dup.test\">dupe</a><a href=\"https://two.test\">two</a>"
	}`)
	got, err := bookmarks.Parse(in, "application/json")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []string{"https://one.test", "https://dup.test", "https://dup.test", "https://two.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse() = %#v, want %#v", got, want)
	}
}

func TestParse_MalformedJSONReturnsValidationError(t *testing.T) {
	t.Parallel()

	_, err := bookmarks.Parse([]byte(`{"bookmarks":[`), "application/json")
	if !errors.Is(err, bookmarks.ErrInvalidJSON) {
		t.Fatalf("Parse() error = %v, want ErrInvalidJSON", err)
	}
}

func TestParse_EmptyOrNoHrefReturnsEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        string
		contentType string
	}{
		{"empty", "", "application/json"},
		{"html without href", "<html><body><p>none</p></body></html>", "text/html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := bookmarks.Parse([]byte(tc.body), tc.contentType)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("Parse() = %#v, want empty", got)
			}
		})
	}
}
