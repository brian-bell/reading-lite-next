package extract_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/extract"
)

const youtubeVideoURL = "https://www.youtube.com/watch?v=abcdEFGHijk"

const timedTextXML = `<?xml version="1.0" encoding="utf-8" ?>
<transcript>
  <text start="0" dur="3.5">Welcome back to the channel.</text>
  <text start="3.5" dur="4.2">Today we restore a dump into a throwaway container.</text>
</transcript>`

func TestYouTube_OEmbed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		transcript        bool
		wantInMarkdown    string
		wantNotInMarkdown string
	}{
		{"transcript present", true, "throwaway container", ""},
		{"transcript absent", false, "Backing up Postgres", "throwaway container"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			var oembedQuery url.Values
			var oembedUA, transcriptUA string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oembed":
					oembedQuery = r.URL.Query()
					oembedUA = r.Header.Get("User-Agent")
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"title":"Backing up Postgres","author_name":"Ada Ops","provider_name":"YouTube"}`)
				case "/api/timedtext":
					transcriptUA = r.Header.Get("User-Agent")
					if !c.transcript {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "text/xml; charset=utf-8")
					_, _ = io.WriteString(w, timedTextXML)
				default:
					t.Errorf("unexpected request path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			// Exercise both seams: a custom HTTP client alongside the test base URL.
			yt := extract.NewYouTube(
				extract.WithYouTubeBaseURL(srv.URL),
				extract.WithYouTubeHTTPClient(srv.Client()),
			)
			got, err := yt.Extract(context.Background(), youtubeVideoURL)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			// Request shape: oEmbed is asked for the video URL as JSON.
			if oembedQuery.Get("url") != youtubeVideoURL {
				t.Errorf("oembed url = %q, want %q", oembedQuery.Get("url"), youtubeVideoURL)
			}
			if oembedQuery.Get("format") != "json" {
				t.Errorf("oembed format = %q, want json", oembedQuery.Get("format"))
			}
			if oembedUA == "" {
				t.Error("oembed request sent no User-Agent")
			}
			if transcriptUA == "" {
				t.Error("timedtext request sent no User-Agent")
			}

			// Floor: title/author/site come from oEmbed regardless of transcript.
			if got.Title != "Backing up Postgres" {
				t.Errorf("Title = %q", got.Title)
			}
			if got.Author != "Ada Ops" {
				t.Errorf("Author = %q", got.Author)
			}
			if got.Site != "YouTube" {
				t.Errorf("Site = %q, want YouTube", got.Site)
			}
			if got.Mode != extract.ModeRawOnly {
				t.Errorf("Mode = %q, want raw_only (oEmbed floor)", got.Mode)
			}

			if !strings.Contains(got.Markdown, c.wantInMarkdown) {
				t.Errorf("Markdown = %q, want it to contain %q", got.Markdown, c.wantInMarkdown)
			}
			if c.wantNotInMarkdown != "" && strings.Contains(got.Markdown, c.wantNotInMarkdown) {
				t.Errorf("Markdown = %q, should not contain %q", got.Markdown, c.wantNotInMarkdown)
			}
		})
	}
}

func TestYouTube_OEmbedErrorIsPermanent(t *testing.T) {
	t.Parallel()

	// A deleted/private/unembeddable video returns 404 from oEmbed; that is a
	// permanent failure (no point retrying), classified through httpx.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	yt := extract.NewYouTube(extract.WithYouTubeBaseURL(srv.URL))
	_, err := yt.Extract(context.Background(), youtubeVideoURL)
	if err == nil {
		t.Fatal("Extract = nil error, want a permanent error on oEmbed 404")
	}
	if !errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("error = %v, want it to wrap dispatch.ErrPermanent", err)
	}
}

func TestYouTube_OEmbedRateLimited(t *testing.T) {
	t.Parallel()

	// A throttled oEmbed (429) must requeue, honoring Retry-After, via httpx — the
	// same classification dispatch.Classify applies, so a rate-limited floor lookup
	// re-dispatches instead of burning the reading.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := extract.NewYouTube(extract.WithYouTubeBaseURL(srv.URL)).
		Extract(context.Background(), youtubeVideoURL)
	if got := dispatch.Classify(err); got.Outcome != dispatch.Requeue || got.After != 15*time.Second {
		t.Fatalf("Classify(oembed 429) = %v/%v, want Requeue/15s", got.Outcome, got.After)
	}
}

func TestYouTube_ContextCancelledDuringTranscriptAborts(t *testing.T) {
	t.Parallel()

	// oEmbed succeeds, then the context is cancelled while the (best-effort)
	// transcript is fetched. A cancelled context is not a missing-transcript case:
	// Extract must abort with the cancellation, not return a floor-only article.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oembed":
			_, _ = io.WriteString(w, `{"title":"A Talk","author_name":"Speaker","provider_name":"YouTube"}`)
		case "/api/timedtext":
			// Cancellation arrives during the transcript fetch. Whether the client
			// observes it as a Do error or the handler races ahead, the context is
			// now cancelled, which is what Extract's post-transcript guard checks.
			cancel()
		}
	}))
	defer srv.Close()

	_, err := extract.NewYouTube(extract.WithYouTubeBaseURL(srv.URL)).Extract(ctx, youtubeVideoURL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Extract = %v, want context.Canceled", err)
	}
}

func TestYouTube_EmptyFloorIsPermanent(t *testing.T) {
	t.Parallel()

	// An oEmbed 200 carrying no title, author, or transcript leaves nothing to
	// embed or summarize — fail permanently rather than return an empty article.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oembed" {
			_, _ = io.WriteString(w, `{"title":"","author_name":"","provider_name":"YouTube"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound) // no transcript
	}))
	defer srv.Close()

	_, err := extract.NewYouTube(extract.WithYouTubeBaseURL(srv.URL)).
		Extract(context.Background(), youtubeVideoURL)
	if err == nil {
		t.Fatal("Extract on an empty oEmbed floor = nil error, want a permanent error")
	}
	if !errors.Is(err, dispatch.ErrPermanent) {
		t.Fatalf("error = %v, want it to wrap dispatch.ErrPermanent", err)
	}
}
