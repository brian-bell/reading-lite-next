package extract_test

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/extract"
	"github.com/bbell/reading-lite/internal/fetch"
)

// update regenerates the committed golden markdown files. Run:
//
//	go test ./internal/extract -run TestReadability -update
var update = flag.Bool("update", false, "update golden files")

// loadFixture reads an HTML fixture from testdata.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// goldenMarkdown compares got against the committed golden file, rewriting it
// when -update is set.
func goldenMarkdown(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", name, err)
	}
	if diff := cmp.Diff(string(want), got); diff != "" {
		t.Fatalf("markdown mismatch for %s (-want +got):\n%s", name, diff)
	}
}

func TestReadability_ExtractsArticle(t *testing.T) {
	t.Parallel()

	res := fetch.Resource{
		Body:        loadFixture(t, "blog_article.html"),
		ContentType: "text/html; charset=utf-8",
		FinalURL:    "https://notes.example.com/postgres-personal",
		Status:      200,
	}

	got, err := extract.NewReadability().Extract(context.Background(), res)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if got.Title != "Running Postgres for a Personal Service" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Author != "Ada Lovelace" {
		t.Errorf("Author = %q", got.Author)
	}
	if got.Mode != extract.ModeReadability {
		t.Errorf("Mode = %q, want %q", got.Mode, extract.ModeReadability)
	}
	// The article body is several sentences; a sensible word count is well above
	// the salvage floor and excludes the nav/sidebar/footer clutter.
	if got.WordCount < 80 {
		t.Errorf("WordCount = %d, want a sensible article-sized count", got.WordCount)
	}

	goldenMarkdown(t, "blog_article.golden.md", got.Markdown)
}

func TestReadability_RawDOMSalvage(t *testing.T) {
	t.Parallel()

	// A discussion page whose substance lives in comment/sidebar blocks defeats
	// article-readability (it strips those as unlikely candidates), so the ladder
	// falls through to the raw-DOM salvage tier.
	res := fetch.Resource{
		Body:        loadFixture(t, "no_article.html"),
		ContentType: "text/html; charset=utf-8",
		FinalURL:    "https://forum.example.com/thread/42",
		Status:      200,
	}

	got, err := extract.NewReadability().Extract(context.Background(), res)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if got.Mode != extract.ModeRawDOM {
		t.Fatalf("Mode = %q, want %q", got.Mode, extract.ModeRawDOM)
	}
	if got.WordCount == 0 || got.Markdown == "" {
		t.Fatalf("raw_dom salvage produced empty text: %+v", got)
	}
	// The <title> must survive the markdown conversion, which mutates the DOM and
	// drops the <head>: the salvaged reading still carries its source title.
	if got.Title != "Thread: best way to back up a personal Postgres" {
		t.Errorf("Title = %q, want the document <title> preserved through raw_dom", got.Title)
	}
	if got.Lang != "en" {
		t.Errorf("Lang = %q, want en", got.Lang)
	}

	goldenMarkdown(t, "no_article.golden.md", got.Markdown)
}

func TestReadability_RawOnly(t *testing.T) {
	t.Parallel()

	// A JS-only SPA shell has no readerable article and no DOM text (the body is
	// an empty mount point), so both upper tiers come up empty and the ladder
	// hits the raw_only floor, which strips tags from the raw bytes.
	res := fetch.Resource{
		Body:        loadFixture(t, "spa_shell.html"),
		ContentType: "text/html; charset=utf-8",
		FinalURL:    "https://app.example.com/",
		Status:      200,
	}

	got, err := extract.NewReadability().Extract(context.Background(), res)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if got.Mode != extract.ModeRawOnly {
		t.Fatalf("Mode = %q, want %q", got.Mode, extract.ModeRawOnly)
	}
	if got.Markdown == "" || got.WordCount == 0 {
		t.Fatalf("raw_only floor produced empty text: %+v", got)
	}
	// The floor keeps the script body — including its bare '<' (`i < state.count`),
	// which the floor must NOT mangle — while the surrounding tags are stripped.
	if !strings.Contains(got.Markdown, "renderApp") {
		t.Errorf("raw_only text dropped the script body: %q", got.Markdown)
	}
	if !strings.Contains(got.Markdown, "i < state.count") {
		t.Errorf("raw_only text mangled the script's angle bracket: %q", got.Markdown)
	}
	for _, tag := range []string{"<script", "</script", "<div", "<html"} {
		if strings.Contains(got.Markdown, tag) {
			t.Errorf("raw_only text still contains the tag %q: %q", tag, got.Markdown)
		}
	}
}

func TestReadability_EmptyBodyIsPermanentError(t *testing.T) {
	t.Parallel()

	// A blank/contentless body produces no text in any tier; rather than mark the
	// reading ready with empty content, Extract fails it permanently.
	cases := map[string][]byte{
		"nil body":        nil,
		"whitespace only": []byte("   \n\t  "),
		"empty document":  []byte("<html><head></head><body></body></html>"),
		// Head metadata is not article content: a bodyless page carrying only a
		// <title> must still fail, not be marked ready with the title as its body.
		"title but empty body": []byte("<html><head><title>Login</title></head><body></body></html>"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := extract.NewReadability().Extract(context.Background(), fetch.Resource{Body: body})
			if err == nil {
				t.Fatal("Extract on empty body = nil error, want a permanent error")
			}
			if !errors.Is(err, dispatch.ErrPermanent) {
				t.Fatalf("error = %v, want it to wrap dispatch.ErrPermanent", err)
			}
		})
	}
}

func TestReadability_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := fetch.Resource{Body: loadFixture(t, "blog_article.html")}
	if _, err := extract.NewReadability().Extract(ctx, res); err == nil {
		t.Fatal("Extract on a cancelled context = nil error, want cancellation")
	}
}
