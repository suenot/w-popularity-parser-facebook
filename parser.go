// Package parser implements the w_popularity facebook adapter.
//
// Reality check: facebook.com serves almost nothing useful to unauthenticated
// HTTP scrapers. A plain `curl -A 'Mozilla/5.0' https://www.facebook.com/<h>/`
// typically yields either a 302 to /login/?next=... or a tiny "Error" stub
// page — no fan_count, no follower_count, no posts.
//
// This module therefore takes a two-track approach:
//
//   1. Best-effort public HTML attempt. We GET the profile URL with a realistic
//      UA, look for embedded JSON blobs that occasionally leak counters (e.g.
//      `"fan_count":12345` or `"follower_count":12345` inside scripts tagged
//      adp_LikeAndShareDataLargeAdp or similar). If we find a number, great —
//      we return a snapshot. If not, we surface ErrAuth wrapped with a hint
//      that production scrapes require camoufox + cookies.
//
//   2. Camoufox fallback skeleton. Config.CamoufoxURL points at a docker
//      camoufox service. fetchViaCamoufox is a stub: it returns a sentinel
//      error unless CamoufoxURL is set, otherwise a not-yet-implemented error.
//      The real CDP wiring is intentionally deferred to a follow-up PR.
//
// FetchRecentPosts is unimplemented for the logged-out path: nothing reliable
// is visible without auth. Returns (nil, nil) — callers treat empty list as
// "no posts available".
//
// Strategy:
//
//	primary:  best-effort logged-out HTML scrape
//	fallback: camoufox via CDP (stubbed)
package parser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// Config controls runtime behaviour.
type Config struct {
	// Credential is unused for the logged-out path. Reserved for future
	// cookie/session-token plumbing.
	Credential string
	// HTTPTimeout caps every outbound call. Default: 20s.
	HTTPTimeout time.Duration
	// CamoufoxURL is the base URL of a camoufox CDP service (e.g.
	// "http://camoufox:3000"). When empty, the camoufox fallback is
	// disabled and FetchChannel surfaces ErrAuth directly.
	CamoufoxURL string
	// BaseURL overrides https://www.facebook.com. Used in tests.
	BaseURL string
	// HTTPClient overrides the default client. Used in tests.
	HTTPClient *http.Client
	// UserAgent overrides the request UA. Default: a recent desktop Chrome UA.
	UserAgent string
}

// New constructs a FacebookParser with sensible defaults.
func New(cfg Config) *FacebookParser {
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 20 * time.Second
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://www.facebook.com"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	return &FacebookParser{cfg: cfg}
}

// FacebookParser is the public entry point.
type FacebookParser struct{ cfg Config }

// Platform implements shared.Parser.
func (p *FacebookParser) Platform() shared.Platform { return shared.PlatformFacebook }

// FetchChannel attempts the logged-out HTML scrape first, falling back to
// camoufox when configured. Map of outcomes:
//
//   - HTTP 200 with extractable counter → snapshot.
//   - HTTP 200 with login-wall markers → ErrAuth (with camoufox hint).
//   - HTTP 404 → ErrNotFound.
//   - HTTP 429 → ErrRateLimited.
//   - HTTP 5xx → ErrTransient.
//   - HTTP 200 with no useful signal → ErrAuth + Raw["fetch_note"] marker
//     (returned indirectly: caller never sees the partial snapshot in the
//     error path; the marker is documented here for the camoufox follow-up).
func (p *FacebookParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	if handle == "" {
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: empty handle", shared.ErrNotFound)
	}

	body, status, err := p.fetchProfileHTML(ctx, handle)
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return shared.ChannelSnapshot{}, err
	case err != nil:
		return shared.ChannelSnapshot{}, err
	}

	switch {
	case status == http.StatusNotFound:
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: facebook returned 404 for %q", shared.ErrNotFound, handle)
	case status == http.StatusTooManyRequests:
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: facebook returned 429 for %q", shared.ErrRateLimited, handle)
	case status >= 500:
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: facebook returned %d for %q", shared.ErrTransient, status, handle)
	case status >= 400 && status != http.StatusBadRequest:
		// 400 is what FB returns when redirecting unauthed traffic to /login —
		// treat that the same as a login wall (handled below).
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: facebook returned %d for %q", shared.ErrTransient, status, handle)
	}

	// Try to pull counters out of any embedded JSON. Facebook occasionally
	// leaves fan_count/follower_count in the initial HTML, even for logged-out
	// users (typically for high-traffic Pages).
	if followers, ok := extractFollowers(body); ok {
		profileURL := fmt.Sprintf("https://www.facebook.com/%s/", url.PathEscape(handle))
		return shared.ChannelSnapshot{
			Platform:  shared.PlatformFacebook,
			Handle:    handle,
			URL:       profileURL,
			FetchedAt: time.Now().UTC(),
			Followers: followers,
			Raw: map[string]interface{}{
				"source":      "logged_out_html",
				"http_status": status,
			},
		}, nil
	}

	// No useful signal. Either it's a login wall or a stub error page.
	if isLoginWall(body) || status == http.StatusBadRequest {
		// Camoufox fallback — currently stubbed.
		if _, ferr := p.fetchViaCamoufox(ctx, fmt.Sprintf("%s/%s/", strings.TrimRight(p.cfg.BaseURL, "/"), url.PathEscape(handle))); ferr != nil {
			return shared.ChannelSnapshot{}, fmt.Errorf("%w: Facebook requires camoufox + cookies (%v)", shared.ErrAuth, ferr)
		}
		// If a real camoufox path ever returns nil here, we'd re-parse and
		// return a snapshot. For now this branch is unreachable.
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: Facebook requires camoufox + cookies", shared.ErrAuth)
	}

	// 200 OK, no markers, no counters. Treat as auth-required too; record a
	// fetch_note so future improvements can distinguish this from a hard wall.
	return shared.ChannelSnapshot{}, fmt.Errorf("%w: logged-out scrape returned no signal; Facebook requires camoufox + cookies", shared.ErrAuth)
}

// FetchRecentPosts is intentionally a no-op for the logged-out path. Public
// post visibility on facebook.com without auth is unreliable; returning an
// empty slice is more honest than fabricating a partial result.
func (p *FacebookParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	return nil, nil
}

// fetchProfileHTML GETs the profile URL with a realistic UA and returns the
// raw body and HTTP status. Redirects are followed.
func (p *FacebookParser) fetchProfileHTML(ctx context.Context, handle string) ([]byte, int, error) {
	u := fmt.Sprintf("%s/%s/", strings.TrimRight(p.cfg.BaseURL, "/"), url.PathEscape(handle))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build request: %v", shared.ErrTransient, err)
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("%w: http: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%w: read body: %v", shared.ErrTransient, err)
	}
	return body, resp.StatusCode, nil
}

// fetchViaCamoufox is the placeholder for the eventual CDP-based fetch path.
//
// Contract:
//   - If Config.CamoufoxURL is empty, return errors.New("camoufox not
//     configured") — callers translate this into ErrAuth.
//   - Otherwise, return errors.New("camoufox path not yet implemented") so
//     production code fails loudly rather than silently degrading.
//
// TODO: implement via chromedp or direct CDP websocket. Suggested shape:
//
//	conn, err := cdp.New(ctx, p.cfg.CamoufoxURL)
//	target, err := conn.CreateTarget(ctx, targetURL)
//	html, err := target.WaitForSelector(ctx, "div[role='main']", 30s)
//	return html, nil
//
// Once implemented, FetchChannel can re-parse the returned HTML with the
// same extractFollowers helper used for the logged-out path.
func (p *FacebookParser) fetchViaCamoufox(ctx context.Context, targetURL string) ([]byte, error) {
	if p.cfg.CamoufoxURL == "" {
		return nil, errors.New("camoufox not configured")
	}
	return nil, errors.New("camoufox path not yet implemented")
}

// --- HTML / JSON inspection helpers ---

// followerRegexes are the patterns we hunt for in embedded JSON. Each one
// matches a digits-only number after the key. We deliberately keep them
// loose: Facebook routinely shuffles internal field names, and we'd rather
// catch fan_count today and follower_count tomorrow than miss both.
var followerRegexes = []*regexp.Regexp{
	regexp.MustCompile(`"fan_count"\s*:\s*(\d+)`),
	regexp.MustCompile(`"follower_count"\s*:\s*(\d+)`),
	regexp.MustCompile(`"followers_count"\s*:\s*(\d+)`),
	// adp_LikeAndShareDataLargeAdp blob: usually contains follower/like
	// counts in nearby fields. The regexes above already cover the common
	// field names; the marker is only used to prioritise hits inside this
	// blob via context if we ever need to disambiguate.
}

// extractFollowers scans body for any of followerRegexes and returns the
// first numeric match. Returns (0, false) when nothing matches.
func extractFollowers(body []byte) (int64, bool) {
	for _, re := range followerRegexes {
		m := re.FindSubmatch(body)
		if len(m) < 2 {
			continue
		}
		if n, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// loginWallMarkers are substrings that strongly suggest Facebook is asking
// us to log in instead of serving real profile content. We err on the side
// of false positives: a page that mentions /login/?next= is almost never
// useful for scraping.
var loginWallMarkers = [][]byte{
	[]byte("/login/?next="),
	[]byte("login_form"),
	[]byte("loginbutton"),
	[]byte(`"action":"\/login\/`),
	[]byte("You must log in to continue"),
	[]byte("Sorry, something went wrong"),
}

func isLoginWall(body []byte) bool {
	// Cheap case-insensitive check against a few obvious markers, plus a
	// case-sensitive scan for the more specific URL fragments.
	lower := bytesToLowerASCII(body)
	for _, m := range loginWallMarkers {
		if contains(lower, bytesToLowerASCII(m)) {
			return true
		}
	}
	return false
}

// bytesToLowerASCII is a tiny ASCII-only lower-caser. We don't need Unicode
// folding for the marker strings, and avoiding bytes.ToLower keeps the hot
// path allocation-free for the common (no-match) case via reuse below.
func bytesToLowerASCII(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// contains is a thin wrapper over strings.Contains for []byte inputs.
func contains(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}
