package parser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// fakeProfileWithCounter is what we'd get in the (rare) case Facebook ships
// counters inline. The fan_count value is embedded inside a fake script tag
// the way FB renders bootstrap state.
const fakeProfileWithCounter = `<!DOCTYPE html>
<html><head><title>Some Page | Facebook</title></head>
<body>
<div id="root"></div>
<script type="application/json" data-content-len="123" data-sjs>
{"require":[["adp_LikeAndShareDataLargeAdp",[],[{"page":{"name":"Some Page","fan_count":12345,"follower_count":12300}}]]]}
</script>
</body></html>`

// fakeLoginWall is the gist of what unauthenticated FB serves: a tiny error
// page that mentions /login/?next= somewhere.
const fakeLoginWall = `<!DOCTYPE html>
<html><body>
<div id="header"><a href="//www.facebook.com/">FB</a></div>
<form id="login_form" action="/login/?next=https%3A%2F%2Fwww.facebook.com%2Ftest%2F">
  <input name="email"><input name="pass" type="password">
  <button id="loginbutton">Log In</button>
</form>
<p>You must log in to continue.</p>
</body></html>`

// newServer returns an httptest.Server that responds with the given status
// and body to every request, regardless of path.
func newServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestPlatform(t *testing.T) {
	if p := New(Config{}); p.Platform() != shared.PlatformFacebook {
		t.Fatalf("platform mismatch: %s", p.Platform())
	}
}

// TestFetchChannel_HappyPath: simulate FB leaking fan_count in HTML.
func TestFetchChannel_HappyPath(t *testing.T) {
	srv := newServer(t, http.StatusOK, fakeProfileWithCounter)
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	snap, err := p.FetchChannel(context.Background(), "test")
	if err != nil {
		t.Fatalf("FetchChannel: %v", err)
	}
	if snap.Platform != shared.PlatformFacebook {
		t.Errorf("platform = %s", snap.Platform)
	}
	if snap.Handle != "test" {
		t.Errorf("handle = %s", snap.Handle)
	}
	if snap.Followers != 12345 {
		t.Errorf("followers = %d; want 12345", snap.Followers)
	}
	if snap.URL != "https://www.facebook.com/test/" {
		t.Errorf("url = %s", snap.URL)
	}
	if snap.FetchedAt.IsZero() {
		t.Errorf("fetched_at not set")
	}
	if got, _ := snap.Raw["source"].(string); got != "logged_out_html" {
		t.Errorf("raw[source] = %v; want logged_out_html", snap.Raw["source"])
	}
}

// TestFetchChannel_LoginWall_NoCamoufox: the common production case for an
// unconfigured worker — should surface ErrAuth and mention camoufox.
func TestFetchChannel_LoginWall_NoCamoufox(t *testing.T) {
	srv := newServer(t, http.StatusOK, fakeLoginWall)
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, CamoufoxURL: ""})
	_, err := p.FetchChannel(context.Background(), "test")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if !strings.Contains(err.Error(), "camoufox") {
		t.Errorf("error should mention camoufox; got %q", err.Error())
	}
}

// TestFetchChannel_LoginWall_WithCamoufoxStub: camoufox URL is set but the
// fetch path is still stubbed. We expect ErrAuth with the stub error wrapped
// into the message so it's visible in logs.
func TestFetchChannel_LoginWall_WithCamoufoxStub(t *testing.T) {
	srv := newServer(t, http.StatusOK, fakeLoginWall)
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL, CamoufoxURL: "http://camoufox:3000"})
	_, err := p.FetchChannel(context.Background(), "test")
	if !errors.Is(err, shared.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Errorf("error should mention the stub; got %q", err.Error())
	}
}

func TestFetchChannel_NotFound(t *testing.T) {
	srv := newServer(t, http.StatusNotFound, "<html><body>404</body></html>")
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	_, err := p.FetchChannel(context.Background(), "nope")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFetchChannel_RateLimited(t *testing.T) {
	srv := newServer(t, http.StatusTooManyRequests, "")
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	_, err := p.FetchChannel(context.Background(), "spammer")
	if !errors.Is(err, shared.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
}

func TestFetchChannel_ServerError(t *testing.T) {
	srv := newServer(t, http.StatusInternalServerError, "")
	defer srv.Close()

	p := New(Config{BaseURL: srv.URL})
	_, err := p.FetchChannel(context.Background(), "anyone")
	if !errors.Is(err, shared.ErrTransient) {
		t.Fatalf("want ErrTransient, got %v", err)
	}
}

func TestFetchChannel_EmptyHandle(t *testing.T) {
	p := New(Config{BaseURL: "http://127.0.0.1:1"})
	_, err := p.FetchChannel(context.Background(), "")
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("want ErrNotFound for empty handle, got %v", err)
	}
}

func TestFetchRecentPosts_AlwaysEmpty(t *testing.T) {
	p := New(Config{})
	posts, err := p.FetchRecentPosts(context.Background(), "test", time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("FetchRecentPosts err = %v", err)
	}
	if len(posts) != 0 {
		t.Errorf("want 0 posts (logged-out path), got %d", len(posts))
	}
}

// TestExtractFollowers covers the regex helpers directly so we don't have
// to spin up a server for every variant of FB's evolving field names.
func TestExtractFollowers(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
		ok   bool
	}{
		{"fan_count", `{"page":{"fan_count":987}}`, 987, true},
		{"follower_count", `{"x":{"follower_count":42}}`, 42, true},
		{"followers_count", `{"data":{"followers_count":555}}`, 555, true},
		{"spaced", `{"fan_count" :  12 }`, 12, true},
		{"zero treated as miss", `{"fan_count":0}`, 0, false},
		{"none", `<html>no signal</html>`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractFollowers([]byte(tc.body))
			if ok != tc.ok || got != tc.want {
				t.Errorf("extractFollowers(%q) = (%d, %v); want (%d, %v)", tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestIsLoginWall(t *testing.T) {
	if !isLoginWall([]byte(fakeLoginWall)) {
		t.Errorf("isLoginWall(fakeLoginWall) = false; want true")
	}
	if isLoginWall([]byte(fakeProfileWithCounter)) {
		t.Errorf("isLoginWall(profile) = true; want false")
	}
}
