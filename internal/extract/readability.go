package extract

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	readability "github.com/go-shiori/go-readability"
	"golang.org/x/net/html"

	"github.com/bbell/reading-lite/internal/dispatch"
	"github.com/bbell/reading-lite/internal/fetch"
)

// Readability is the production [Extractor]. It runs a three-tier salvage ladder
// over fetched HTML:
//
//  1. readability — a clean readability extraction (a semantic article is found),
//     converted to markdown. Mode [ModeReadability].
//  2. raw_dom — when the page is not readerable, the whole DOM is converted to
//     markdown as a best-effort salvage. Mode [ModeRawDOM].
//  3. raw_only — the floor: every text node (including script/style bodies) is
//     collected from the parsed body. It yields text for any page that carries
//     some; a contentless body fails the extraction permanently. Mode [ModeRawOnly].
//
// The ordering and the sufficiency gate live in the pure [selectTier]; each tier
// wraps the HTML libraries behind a small function.
type Readability struct{}

// NewReadability builds the production extractor.
func NewReadability() *Readability { return &Readability{} }

var _ Extractor = (*Readability)(nil)

// Extract runs the tier ladder over the fetched resource and returns the first
// tier that yields sufficient content, falling through to the raw_only floor.
func (e *Readability) Extract(ctx context.Context, r fetch.Resource) (Article, error) {
	if err := ctx.Err(); err != nil {
		return Article{}, err
	}

	a := selectTier(
		func() (Article, bool) { return readabilityTier(r) },
		func() (Article, bool) { return rawDOMTier(r) },
		func() (Article, bool) { return rawOnlyTier(r), true },
	)
	// Even the floor came up empty (a blank or contentless body): there is nothing
	// to embed or summarize, so fail permanently rather than mark the reading ready
	// with empty content. Re-fetching a contentless 200 will not help.
	if a.WordCount == 0 {
		return Article{}, fmt.Errorf("%w: extract: no extractable content", dispatch.ErrPermanent)
	}
	return a, nil
}

// tierFunc attempts one extraction tier, reporting whether it produced
// sufficient content. The floor tier always reports true.
type tierFunc func() (Article, bool)

// selectTier returns the first tier in ladder order that reports sufficient
// content. The final tier is the floor and must always report true, so
// selectTier always returns an Article. It is pure: the HTML work lives inside
// the tier funcs, so the ladder ordering is testable with synthetic tiers.
func selectTier(tiers ...tierFunc) Article {
	var last Article
	for _, tier := range tiers {
		a, ok := tier()
		if ok {
			return a
		}
		last = a
	}
	return last
}

// readabilityTier runs go-readability when the page is readerable and converts
// the cleaned article HTML to markdown. It reports ok only when a semantic
// article was found and the result carries enough text; otherwise the caller
// falls through to the salvage tiers.
func readabilityTier(r fetch.Resource) (Article, bool) {
	// Check gates the tier: it reports whether the page has an article-like body.
	// FromReader has a whole-page fallback that returns text even for
	// non-articles, so Check (not output length) is what distinguishes a clean
	// readability extraction from a page that needs raw-DOM salvage.
	if !readability.Check(bytes.NewReader(r.Body)) {
		return Article{}, false
	}

	pageURL, err := url.Parse(r.FinalURL)
	if err != nil {
		pageURL = &url.URL{}
	}
	art, err := readability.FromReader(bytes.NewReader(r.Body), pageURL)
	if err != nil {
		return Article{}, false
	}

	md, err := toMarkdown(art.Content)
	if err != nil {
		return Article{}, false
	}

	a := Article{
		Title:     strings.TrimSpace(art.Title),
		Author:    strings.TrimSpace(art.Byline),
		Site:      strings.TrimSpace(art.SiteName),
		Lang:      strings.TrimSpace(art.Language),
		Markdown:  md,
		Mode:      ModeReadability,
		WordCount: wordCount(md),
	}
	return a, sufficient(md)
}

// rawDOMTier salvages a non-readerable page by converting its whole DOM to
// markdown. The HTML-to-markdown converter drops script/style, so a page whose
// only text is script content yields nothing here and falls through to raw_only.
func rawDOMTier(r fetch.Resource) (Article, bool) {
	doc, err := html.Parse(bytes.NewReader(r.Body))
	if err != nil {
		return Article{}, false
	}

	// Read title/lang BEFORE converting: ConvertNode mutates the DOM (it removes
	// the <head> subtree during pre-render), so reading the <title> afterward
	// would always come back empty.
	title := documentTitle(doc)
	lang := documentLang(doc)

	out, err := htmltomarkdown.ConvertNode(doc)
	if err != nil {
		return Article{}, false
	}
	md := strings.TrimSpace(string(out))

	a := Article{
		Title:     title,
		Lang:      lang,
		Markdown:  md,
		Mode:      ModeRawDOM,
		WordCount: wordCount(md),
	}
	return a, sufficient(md)
}

// rawOnlyTier is the floor: it collects every text node from the parsed body,
// including script/style raw-text bodies, so even a JS-only or near-binary page
// yields something. It always succeeds.
func rawOnlyTier(r fetch.Resource) Article {
	text := rawText(r.Body)
	return Article{
		Markdown:  text,
		Mode:      ModeRawOnly,
		WordCount: wordCount(text),
	}
}

// toMarkdown converts a cleaned HTML fragment to trimmed markdown.
func toMarkdown(htmlFragment string) (string, error) {
	md, err := htmltomarkdown.ConvertString(htmlFragment)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(md), nil
}

// minReadableWords is the floor below which a tier's output is treated as too
// thin to be the article — the ladder falls through to the next tier.
const minReadableWords = 25

// sufficient reports whether extracted markdown carries enough text to stand as
// the article body. It is the pure heuristic the tier ladder branches on.
func sufficient(markdown string) bool {
	return wordCount(markdown) >= minReadableWords
}

// wordCount counts whitespace-separated tokens.
func wordCount(s string) int {
	return len(strings.Fields(s))
}

// rawText collects every text node from the parsed document's <body> and
// collapses whitespace. It walks script/style raw-text bodies too, so a JS-only
// or near-binary page still yields its text. Unlike a hand-rolled tag stripper it
// relies on the HTML tokenizer, which preserves literal '<'/'>' inside raw-text
// elements and free text (e.g. `for (i < n)`), so the floor does not mangle the
// JS-heavy pages it exists to salvage.
//
// It walks only the <body>, not <head>: head text (a <title>, meta) is page
// metadata, not article content, so a bodyless page yields no words and the
// caller's empty-content guard fails it permanently instead of marking it ready
// with just its title.
func rawText(body []byte) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		// html.Parse does not error on a byte slice, but if that ever changes,
		// fall back to the collapsed raw bytes so the floor still yields text.
		return strings.Join(strings.Fields(string(body)), " ")
	}

	root := bodyNode(doc)
	if root == nil {
		// html.Parse always synthesizes a <body>; if some future input does not,
		// fall back to the whole document rather than silently returning nothing.
		root = doc
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return strings.Join(strings.Fields(b.String()), " ")
}

// bodyNode returns the document's <body> element, or nil if there is none.
func bodyNode(doc *html.Node) *html.Node {
	var found *html.Node
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "body" {
			found = n
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(doc)
	return found
}

// documentTitle returns the trimmed text of the first <title> element, or "".
func documentTitle(doc *html.Node) string {
	var title string
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "title" {
			if n.FirstChild != nil {
				title = strings.TrimSpace(n.FirstChild.Data)
			}
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(doc)
	return title
}

// documentLang returns the lang attribute of the first <html> element, or "".
func documentLang(doc *html.Node) string {
	var lang string
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "html" {
			for _, attr := range n.Attr {
				if attr.Key == "lang" {
					lang = strings.TrimSpace(attr.Val)
				}
			}
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(doc)
	return lang
}
