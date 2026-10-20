package discord

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeDiscord stands in for Discord's REST API. It answers every request
// a session sends with the reply registered for its method and path, and
// records each one, so no test reaches the network.
type fakeDiscord struct {
	mu       sync.Mutex
	replies  map[string]func(body string) fakeReply
	requests []fakeRequest
	next     int
	// seen is signalled after each request.
	seen chan struct{}
}

type fakeReply struct {
	status int
	body   string
}

type fakeRequest struct {
	Method, Path string
	Body         string
}

func newFakeDiscord() *fakeDiscord {
	return &fakeDiscord{replies: map[string]func(string) fakeReply{}, seen: make(chan struct{}, 1)}
}

// on sets the reply to method and path, which is relative to the API
// root, such as "/applications/1/commands". A path ending in "*" matches
// every path it begins, after the exact paths.
func (f *fakeDiscord) on(method, path string, status int, body string) {
	f.reply(method, path, func(string) fakeReply { return fakeReply{status, body} })
}

// reply sets a function that answers method and path from the request's
// body.
func (f *fakeDiscord) reply(method, path string, fn func(body string) fakeReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method+" "+path] = fn
}

// messages answers method and path with a new message id each time,
// "m1", "m2", and so on, shared across every path set this way.
func (f *fakeDiscord) messages(method, path string) {
	f.reply(method, path, func(string) fakeReply {
		f.next++
		return fakeReply{200, fmt.Sprintf(`{"id": "m%d", "channel_id": "c1"}`, f.next)}
	})
}

// waitFor waits until a request matches method and path, and returns it.
func (f *fakeDiscord) waitFor(t *testing.T, method, path string) fakeRequest {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		for _, r := range f.sent() {
			if r.Method == method && r.Path == path {
				return r
			}
		}
		select {
		case <-f.seen:
		case <-deadline:
			t.Fatalf("no %s %s among %v", method, path, f.sent())
		}
	}
}

// session returns a discordgo session whose requests go to f.
func (f *fakeDiscord) session(t *testing.T) *discordgo.Session {
	t.Helper()
	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: f}
	s.MaxRestRetries = 0
	return s
}

// sent returns the requests f has received.
func (f *fakeDiscord) sent() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeDiscord) RoundTrip(r *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v"+discordgo.APIVersion)
	var body []byte
	if r.Body != nil {
		var err error
		if body, err = io.ReadAll(r.Body); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{r.Method, path, string(body)})
	fn, ok := f.replies[r.Method+" "+path]
	for key, g := range f.replies {
		if pre, wild := strings.CutSuffix(key, "*"); !ok && wild && strings.HasPrefix(r.Method+" "+path, pre) {
			fn, ok = g, true
		}
	}
	reply := fakeReply{http.StatusNotFound, `{"message": "Unknown", "code": 10003}`}
	if ok {
		reply = fn(string(body))
	}
	f.mu.Unlock()
	select {
	case f.seen <- struct{}{}:
	default:
	}
	return &http.Response{
		StatusCode: reply.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(reply.body)),
		Request:    r,
	}, nil
}
