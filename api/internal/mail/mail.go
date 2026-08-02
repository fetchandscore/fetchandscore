// Package mail sends the transactional email the service depends on, which
// today is only the sign-in link.
package mail

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Message is one email.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers a message. The interface exists so tests can assert on what
// would have been sent without reaching the network.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Recorder is a Sender that keeps messages in memory. Used by tests and by the
// development server, where printing the sign-in link to the log is the point.
type Recorder struct {
	mu   sync.Mutex
	sent []Message
	// OnSend, if set, runs after each message is recorded.
	OnSend func(Message)
}

func (r *Recorder) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	r.sent = append(r.sent, m)
	r.mu.Unlock()
	if r.OnSend != nil {
		r.OnSend(m)
	}
	return nil
}

// Sent returns a copy of everything recorded so far.
func (r *Recorder) Sent() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.sent...)
}

// Last returns the most recent message, and whether there was one.
func (r *Recorder) Last() (Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sent) == 0 {
		return Message{}, false
	}
	return r.sent[len(r.sent)-1], true
}

// Mailgun sends through the Mailgun HTTP API.
//
// Their API is a form POST with basic auth, so this needs no SDK — one
// dependency avoided for about thirty lines.
type Mailgun struct {
	Domain string
	APIKey string
	From   string
	// BaseURL defaults to Mailgun's US endpoint. Set it to the EU host, or to
	// a test server.
	BaseURL string
	Client  *http.Client
}

func (m *Mailgun) endpoint() string {
	base := m.BaseURL
	if base == "" {
		base = "https://api.mailgun.net"
	}
	return fmt.Sprintf("%s/v3/%s/messages", strings.TrimSuffix(base, "/"), m.Domain)
}

func (m *Mailgun) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (m *Mailgun) Send(ctx context.Context, msg Message) error {
	form := url.Values{
		"from":    {m.From},
		"to":      {msg.To},
		"subject": {msg.Subject},
		"text":    {msg.Text},
	}
	if msg.HTML != "" {
		form.Set("html", msg.HTML)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building mailgun request: %w", err)
	}
	req.SetBasicAuth("api", m.APIKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client().Do(req)
	if err != nil {
		return fmt.Errorf("sending via mailgun: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read a bounded amount: Mailgun's errors are short, and an unbounded
		// read here would be a denial-of-service handed to us by our own
		// dependency.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("mailgun returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
