package mail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMailgun_SendsAFormPostWithBasicAuth(t *testing.T) {
	var (
		gotPath string
		gotForm url.Values
		gotUser string
		gotKey  string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUser, gotKey, _ = r.BasicAuth()
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing form: %v", err)
		}
		gotForm = r.PostForm
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := &Mailgun{
		Domain:  "mg.fetchandscore.com",
		APIKey:  "secret-key",
		From:    "Fetch and Score <no-reply@fetchandscore.com>",
		BaseURL: srv.URL,
	}

	err := m.Send(context.Background(), Message{
		To:      "brad@example.com",
		Subject: "Your sign-in link",
		Text:    "plain body",
		HTML:    "<p>rich body</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if want := "/v3/mg.fetchandscore.com/messages"; gotPath != want {
		t.Errorf("posted to %q, want %q", gotPath, want)
	}
	if gotUser != "api" || gotKey != "secret-key" {
		t.Errorf("basic auth = %q/%q, want api/secret-key", gotUser, gotKey)
	}
	for field, want := range map[string]string{
		"to":      "brad@example.com",
		"subject": "Your sign-in link",
		"text":    "plain body",
		"html":    "<p>rich body</p>",
	} {
		if got := gotForm.Get(field); got != want {
			t.Errorf("form field %q = %q, want %q", field, got, want)
		}
	}
}

// A message with no HTML part must not send an empty html field, which
// Mailgun would render as a blank email.
func TestMailgun_OmitsAnEmptyHTMLPart(t *testing.T) {
	var hasHTML bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_, hasHTML = r.PostForm["html"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := &Mailgun{Domain: "d", APIKey: "k", From: "f", BaseURL: srv.URL}
	if err := m.Send(context.Background(), Message{To: "a@b.c", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if hasHTML {
		t.Error("an html field was sent for a text-only message")
	}
}

// A rejected send must surface as an error rather than being swallowed, or a
// handler would report a link sent that never went anywhere.
func TestMailgun_ReportsARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid private key"}`))
	}))
	defer srv.Close()

	m := &Mailgun{Domain: "d", APIKey: "wrong", From: "f", BaseURL: srv.URL}
	err := m.Send(context.Background(), Message{To: "a@b.c", Text: "hi"})
	if err == nil {
		t.Fatal("Send succeeded against a 401, want an error")
	}
	if !strings.Contains(err.Error(), "Invalid private key") {
		t.Errorf("error %q does not carry Mailgun's explanation", err)
	}
}

func TestMailgun_DefaultsToTheUSEndpoint(t *testing.T) {
	m := &Mailgun{Domain: "mg.example.com"}
	if want := "https://api.mailgun.net/v3/mg.example.com/messages"; m.endpoint() != want {
		t.Errorf("endpoint() = %q, want %q", m.endpoint(), want)
	}
}

func TestRecorder_CapturesMessages(t *testing.T) {
	var r Recorder

	if _, ok := r.Last(); ok {
		t.Error("Last reported a message before anything was sent")
	}

	for _, body := range []string{"first", "second"} {
		if err := r.Send(context.Background(), Message{To: "a@b.c", Text: body}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	if got := r.Sent(); len(got) != 2 {
		t.Fatalf("recorded %d messages, want 2", len(got))
	}
	last, ok := r.Last()
	if !ok || last.Text != "second" {
		t.Errorf("Last() = %+v, want the second message", last)
	}
}
