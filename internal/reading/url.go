package reading

import (
	"errors"
	"net"
	"net/url"
	"path"
	"strings"
)

// SourceKind identifies the source family for a reading.
type SourceKind string

const (
	// SourceWeb is a regular web page.
	SourceWeb SourceKind = "web"
	// SourceYouTube is a YouTube video URL.
	SourceYouTube SourceKind = "youtube"
	// SourceReddit is a Reddit URL.
	SourceReddit SourceKind = "reddit"
	// SourceMarkdown is a Markdown document URL.
	SourceMarkdown SourceKind = "markdown"
)

// ErrInvalidURL reports a URL that cannot produce a reading idempotency key.
var ErrInvalidURL = errors.New("invalid reading url")

var trackingParams = map[string]bool{
	"fbclid": true,
	"gclid":  true,
	"ref":    true,
}

var youtubeHosts = map[string]bool{
	"m.youtube.com":     true,
	"music.youtube.com": true,
	"youtube.com":       true,
	"www.youtube.com":   true,
	"youtu.be":          true,
}

var redditHosts = map[string]bool{
	"old.reddit.com": true,
	"reddit.com":     true,
	"www.reddit.com": true,
}

// URLKey returns the canonical idempotency key for a user-submitted URL.
func URLKey(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", ErrInvalidURL
	}
	if u.Hostname() == "" || hasInvalidPort(u) {
		return "", ErrInvalidURL
	}
	if u.User != nil {
		return "", ErrInvalidURL
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", ErrInvalidURL
	}

	host := canonicalHost(scheme, u.Host)
	if host == "" {
		return "", ErrInvalidURL
	}

	if host == "youtu.be" {
		return youtubeShortKey(u)
	}
	if isYouTubeHost(host) && u.Path == "/watch" {
		return youtubeWatchKey(u)
	}

	return canonicalHTTPKey(host, canonicalPath(u.EscapedPath()), filteredQuery(u.Query()).Encode()), nil
}

// ClassifySource identifies which source-specific path later pipeline stages should use.
func ClassifySource(key string) SourceKind {
	u, err := url.Parse(key)
	if err != nil {
		return SourceWeb
	}

	host := canonicalHost(strings.ToLower(u.Scheme), u.Host)
	switch {
	case isYouTubeHost(host):
		return SourceYouTube
	case redditHosts[host]:
		return SourceReddit
	case isMarkdownPath(u.Path):
		return SourceMarkdown
	default:
		return SourceWeb
	}
}

func youtubeShortKey(u *url.URL) (string, error) {
	id := strings.Trim(u.EscapedPath(), "/")
	if strings.Contains(id, "%") || strings.Contains(id, "/") {
		return "", ErrInvalidURL
	}
	if !isYouTubeID(id) {
		return "", ErrInvalidURL
	}

	out := &url.URL{
		Scheme: "https",
		Host:   "www.youtube.com",
		Path:   "/watch",
	}
	query := url.Values{}
	query.Set("v", id)
	out.RawQuery = query.Encode()

	return out.String(), nil
}

func youtubeWatchKey(u *url.URL) (string, error) {
	query := u.Query()
	videoID := query.Get("v")
	if !isYouTubeID(videoID) {
		return "", ErrInvalidURL
	}

	out := &url.URL{
		Scheme: "https",
		Host:   "www.youtube.com",
		Path:   "/watch",
	}
	values := url.Values{}
	values.Set("v", videoID)
	out.RawQuery = values.Encode()

	return out.String(), nil
}

func canonicalHost(scheme, host string) string {
	host = strings.ToLower(host)
	name, port, err := net.SplitHostPort(host)
	if err == nil {
		normalizedName := strings.TrimSuffix(name, ".")
		if port == defaultPort(scheme) && normalizedName != "" {
			return bracketIPv6(normalizedName)
		}

		return net.JoinHostPort(strings.Trim(normalizedName, "[]"), port)
	}

	return strings.TrimSuffix(host, ".")
}

func hasInvalidPort(u *url.URL) bool {
	if u.Port() != "" {
		return false
	}

	host := u.Host
	if strings.HasPrefix(host, "[") {
		end := strings.LastIndex(host, "]")
		return end >= 0 && len(host) > end+1 && host[end+1] == ':'
	}

	return strings.Contains(host, ":")
}

func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func bracketIPv6(host string) string {
	host = strings.Trim(host, "[]")
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}

	return host
}

func canonicalPath(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	if escapedPath == "/" {
		return "/"
	}

	return strings.TrimRight(escapedPath, "/")
}

func canonicalHTTPKey(host, escapedPath, rawQuery string) string {
	key := "https://" + host + escapedPath
	if rawQuery != "" {
		key += "?" + rawQuery
	}

	return key
}

func filteredQuery(values url.Values) url.Values {
	out := url.Values{}
	for key, vals := range values {
		lowerKey := strings.ToLower(key)
		if trackingParams[lowerKey] || strings.HasPrefix(lowerKey, "utm_") {
			continue
		}
		for _, value := range vals {
			out.Add(key, value)
		}
	}

	return out
}

func isYouTubeHost(host string) bool {
	return youtubeHosts[host]
}

func isYouTubeID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' {
			continue
		}

		return false
	}

	return true
}

func isMarkdownPath(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return ext == ".md" || ext == ".markdown"
}
