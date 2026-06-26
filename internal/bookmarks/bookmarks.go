// Package bookmarks parses bookmark import payloads into URL lists.
package bookmarks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
)

var (
	// ErrInvalidJSON reports malformed JSON or extra JSON values.
	ErrInvalidJSON = errors.New("invalid bookmark JSON")
	// ErrMultipleJSONValues reports a payload with more than one top-level JSON value.
	ErrMultipleJSONValues = errors.New("bookmark JSON must contain one value")
	// ErrInvalidShape reports syntactically valid JSON that is not a supported
	// bookmark import shape.
	ErrInvalidShape = errors.New("invalid bookmark shape")
)

// Parse extracts bookmark URLs from Netscape/HTML, a JSON array of {"url":...}
// objects, or a JSON object with bookmarks and optional html fields. Duplicates
// are retained in input order.
func Parse(data []byte, contentType string) ([]string, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}

	if strings.Contains(strings.ToLower(contentType), "text/html") || trimmed[0] == '<' {
		return HREFs(string(data)), nil
	}
	if trimmed[0] == '[' {
		var bookmarks []bookmarkInput
		if err := decodeOne(data, &bookmarks); err != nil {
			return nil, err
		}
		urls := make([]string, 0, len(bookmarks))
		for _, b := range bookmarks {
			urls = append(urls, b.URL)
		}
		return urls, nil
	}
	if trimmed[0] != '{' {
		return nil, ErrInvalidShape
	}

	var req struct {
		HTML      string          `json:"html"`
		Bookmarks []bookmarkInput `json:"bookmarks"`
	}
	if err := decodeOne(data, &req); err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(req.Bookmarks))
	for _, b := range req.Bookmarks {
		urls = append(urls, b.URL)
	}
	if req.HTML != "" {
		urls = append(urls, HREFs(req.HTML)...)
	}
	return urls, nil
}

// HREFs returns href values from anchor tags in raw HTML.
func HREFs(raw string) []string {
	tokenizer := html.NewTokenizer(strings.NewReader(raw))
	var out []string
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := tokenizer.TagName()
			if string(name) != "a" || !hasAttr {
				continue
			}
			for {
				key, val, more := tokenizer.TagAttr()
				if string(key) == "href" {
					out = append(out, string(val))
					break
				}
				if !more {
					break
				}
			}
		}
	}
}

type bookmarkInput struct {
	URL string `json:"url"`
}

func decodeOne(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrMultipleJSONValues
	}
	return nil
}
