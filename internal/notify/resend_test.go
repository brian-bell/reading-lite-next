package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bbell/reading-lite/internal/notify"
)

func TestResend_RequestShape(t *testing.T) {
	t.Parallel()

	var gotAuth, gotPath string
	var gotBody struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"email-123"}`))
	}))
	defer srv.Close()

	n := notify.NewResend("test-key", notify.WithBaseURL(srv.URL))
	err := n.Notify(context.Background(), notify.Email{
		From:    "reader@example.com",
		To:      "me@example.com",
		Subject: "Reading ready",
		HTML:    "<p>done</p>",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if gotPath != "/emails" {
		t.Errorf("path = %q, want /emails", gotPath)
	}
	if gotBody.From != "reader@example.com" || gotBody.Subject != "Reading ready" || gotBody.HTML != "<p>done</p>" {
		t.Errorf("body = %+v, want the email fields", gotBody)
	}
	if len(gotBody.To) != 1 || gotBody.To[0] != "me@example.com" {
		t.Errorf("to = %v, want [me@example.com]", gotBody.To)
	}
}

func TestResend_Non2xxIsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"invalid from address"}`))
	}))
	defer srv.Close()

	n := notify.NewResend("k", notify.WithBaseURL(srv.URL))
	err := n.Notify(context.Background(), notify.Email{To: "x@example.com"})
	if err == nil {
		t.Fatal("non-2xx = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("error = %v, want it to name the 422 status", err)
	}
}
