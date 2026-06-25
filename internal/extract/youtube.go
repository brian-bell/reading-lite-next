package extract

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/httpx"
)

// YouTube request defaults.
const (
	youtubeDefaultBaseURL = "https://www.youtube.com"
	youtubeOEmbedPath     = "/oembed"
	youtubeTimedTextPath  = "/api/timedtext"
	youtubeProviderName   = "YouTube"
	youtubeUserAgent      = "reading-lite/1.0 (+https://github.com/bbell/reading-lite)"
	youtubeOEmbedMax      = 1 << 20 // 1 MiB cap on the oEmbed JSON body
	youtubeTranscriptMax  = 1 << 20 // 1 MiB cap on a best-effort transcript body
)

// YouTube extracts a YouTube video's metadata floor via the oEmbed endpoint
// (title/author), augmented best-effort with a timed-text transcript. It is not
// an [Extractor]: oEmbed needs the video URL and makes its own requests, so it
// sits beside the HTML ladder rather than inside [Readability].
//
// The result carries [ModeRawOnly]: the oEmbed floor is flat title/author text
// (plus an optional transcript) with no semantic article structure, so it is the
// floor tier rather than a readability extraction. A failed oEmbed lookup is
// classified for the dispatcher through [httpx]; a failed transcript fetch is
// swallowed (the floor still stands).
type YouTube struct {
	baseURL string
	client  *http.Client
}

// YouTubeOption configures a [YouTube] extractor.
type YouTubeOption func(*YouTube)

// WithYouTubeBaseURL overrides the oEmbed/timed-text base URL (used to point at
// a test server).
func WithYouTubeBaseURL(u string) YouTubeOption {
	return func(y *YouTube) { y.baseURL = u }
}

// WithYouTubeHTTPClient overrides the underlying http.Client.
func WithYouTubeHTTPClient(c *http.Client) YouTubeOption {
	return func(y *YouTube) { y.client = c }
}

// NewYouTube returns a YouTube extractor.
func NewYouTube(opts ...YouTubeOption) *YouTube {
	y := &YouTube{
		baseURL: youtubeDefaultBaseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(y)
	}
	return y
}

type oembedResponse struct {
	Title        string `json:"title"`
	AuthorName   string `json:"author_name"`
	ProviderName string `json:"provider_name"`
}

// Extract returns the oEmbed floor (title/author) for videoURL, with the
// transcript folded into the markdown when one is available.
//
// videoURL should be the canonical watch URL — "https://www.youtube.com/watch?v=<id>",
// the form reading.URLKey produces from every supported YouTube link (youtu.be,
// /shorts, /embed, m./music. hosts). oEmbed itself accepts any of those forms, but
// the transcript lookup reads the "v" parameter, so a non-canonical URL still
// yields the floor while silently skipping the (best-effort) transcript. Callers
// pass the reading's normalized key, not its raw submitted URL, to keep YouTube URL
// canonicalization in one place (reading.URLKey) rather than duplicated here.
func (y *YouTube) Extract(ctx context.Context, videoURL string) (Article, error) {
	if err := ctx.Err(); err != nil {
		return Article{}, err
	}

	meta, err := y.oembed(ctx, videoURL)
	if err != nil {
		return Article{}, err
	}

	site := meta.ProviderName
	if site == "" {
		site = youtubeProviderName
	}

	// Best-effort transcript: failure (or no caption track) leaves the floor intact.
	transcript := y.transcript(ctx, videoURL)

	// transcript swallows ordinary misses, but a cancelled or expired context is
	// not a missing-transcript case — it means the run is aborting. Propagate it
	// rather than returning a "successful" floor-only article, so a shutting-down
	// run is not classified Done once this adapter is wired into the pipeline.
	if err := ctx.Err(); err != nil {
		return Article{}, err
	}

	md := youtubeMarkdown(meta.Title, meta.AuthorName, transcript)

	// An oEmbed 200 with no title, author, or transcript leaves nothing to embed
	// or summarize. Fail permanently rather than produce an empty, "ready" reading
	// — symmetric with Readability.Extract's empty-content guard.
	if wordCount(md) == 0 {
		return Article{}, fmt.Errorf("%w: youtube: oembed returned no title, author, or transcript", dispatch.ErrPermanent)
	}

	return Article{
		Title:     strings.TrimSpace(meta.Title),
		Author:    strings.TrimSpace(meta.AuthorName),
		Site:      site,
		Markdown:  md,
		Mode:      ModeRawOnly,
		WordCount: wordCount(md),
	}, nil
}

// oembed fetches and parses the oEmbed JSON for videoURL. A non-2xx response is
// classified for the dispatcher (404 → permanent, 429 → rate-limited, 5xx →
// transient) so the floor lookup follows the same retry semantics as the other
// HTTP adapters.
func (y *YouTube) oembed(ctx context.Context, videoURL string) (oembedResponse, error) {
	endpoint := y.baseURL + youtubeOEmbedPath + "?" + url.Values{
		"url":    {videoURL},
		"format": {"json"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return oembedResponse{}, fmt.Errorf("youtube: build oembed request: %w", err)
	}
	req.Header.Set("User-Agent", youtubeUserAgent)

	resp, err := y.client.Do(req)
	if err != nil {
		return oembedResponse{}, fmt.Errorf("youtube: oembed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return oembedResponse{}, httpx.ClassifyResponse("youtube oembed", resp)
	}

	// Cap the body: the base URL is operator-configurable and fetch.HTTP's cap
	// does not cover the adapter's own requests, so a misbehaving origin must not
	// be able to stream an unbounded JSON value into memory.
	var meta oembedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, youtubeOEmbedMax)).Decode(&meta); err != nil {
		return oembedResponse{}, fmt.Errorf("youtube: decode oembed: %w", err)
	}
	return meta, nil
}

// transcript fetches the timed-text caption track for videoURL and returns the
// joined caption lines, or "" when no transcript is available. Every failure
// path is swallowed: the transcript is an enhancement over the oEmbed floor.
func (y *YouTube) transcript(ctx context.Context, videoURL string) string {
	videoID := youtubeVideoID(videoURL)
	if videoID == "" {
		return ""
	}

	endpoint := y.baseURL + youtubeTimedTextPath + "?" + url.Values{
		"v":    {videoID},
		"lang": {"en"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", youtubeUserAgent)

	resp, err := y.client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, youtubeTranscriptMax))
	if err != nil {
		return ""
	}
	return parseTimedText(body)
}

// timedText is the YouTube caption track format.
type timedText struct {
	Lines []struct {
		Text string `xml:",chardata"`
	} `xml:"text"`
}

// parseTimedText joins the caption lines of a timed-text document into a single
// whitespace-collapsed transcript, or "" if it cannot be parsed.
func parseTimedText(body []byte) string {
	var doc timedText
	if err := xml.Unmarshal(body, &doc); err != nil {
		return ""
	}
	var parts []string
	for _, line := range doc.Lines {
		if t := strings.TrimSpace(line.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// youtubeMarkdown composes the floor markdown: a title heading, a byline, and
// the transcript when present.
func youtubeMarkdown(title, author, transcript string) string {
	var b strings.Builder
	if title = strings.TrimSpace(title); title != "" {
		b.WriteString("# " + title + "\n")
	}
	if author = strings.TrimSpace(author); author != "" {
		b.WriteString("\nBy " + author + "\n")
	}
	if transcript = strings.TrimSpace(transcript); transcript != "" {
		b.WriteString("\n" + transcript + "\n")
	}
	return strings.TrimSpace(b.String())
}

// youtubeVideoID returns the v query parameter of a canonical YouTube watch URL,
// or "". It deliberately does not re-derive ids from youtu.be / shorts / embed
// forms: that canonicalization is reading.URLKey's job, and Extract documents
// that it expects the canonical watch URL. A non-canonical URL yields "" here and
// the transcript is skipped (the floor still stands).
func youtubeVideoID(videoURL string) string {
	u, err := url.Parse(videoURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("v")
}
