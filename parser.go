// Package parser implements the w_popularity Facebook adapter.
//
// Reality check: the open web (logged-out) cannot scrape Facebook reliably.
// A vanilla `curl -A 'Mozilla/5.0' https://www.facebook.com/<h>/` either
// 302s to /login/?next=… or returns a tiny "Error" stub page. We do not even
// try the logged-out path anymore. Instead we offer two real routes:
//
//  1. Graph API v19 (primary, when Config.AccessToken is set). Requires a
//     Page Access Token; user profiles do not expose counters to third-party
//     tokens. See https://developers.facebook.com/docs/graph-api/reference/page/
//     for the field list.
//
//  2. camoufox via CDP (skeleton). When Config.CamoufoxURL is set we will
//     eventually drive a headless camoufox over Chrome DevTools Protocol with
//     a real session cookie. The wiring layer is present but the fetch is
//     currently a stub — it errors with "camoufox path not implemented" so
//     production callers fail loudly until somebody finishes the work.
//
// If neither AccessToken nor CamoufoxURL is configured, FetchChannel /
// FetchRecentPosts return shared.ErrAuth with a hint pointing the operator
// at the two env vars.
package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	shared "github.com/suenot/w-popularity-shared"
)

// defaultGraphAPI is Facebook's Graph API root. Override via Config.GraphAPIURL
// in tests (httptest) — production always uses the public endpoint.
const defaultGraphAPI = "https://graph.facebook.com"

// defaultAPIVersion is the Graph API version we target. Bump deliberately:
// every minor revision breaks field availability in some way.
const defaultAPIVersion = "v19.0"

// Config controls runtime behaviour.
type Config struct {
	// AccessToken is a Facebook Graph API Page Access Token. When empty, the
	// parser falls through to the camoufox path. Source: env FACEBOOK_ACCESS_TOKEN.
	AccessToken string
	// HTTPClient lets callers inject a custom transport (tests, instrumentation,
	// proxies). Default: &http.Client{Timeout: HTTPTimeout}.
	HTTPClient *http.Client
	// HTTPTimeout caps every outbound Graph API call. Default: 15s.
	HTTPTimeout time.Duration
	// UserAgent is sent on every Graph API request. Default: a w_popularity
	// identifier — Graph does not particularly care, but it helps log triage
	// on the FB side.
	UserAgent string
	// CamoufoxURL is the CDP endpoint (Playwright WebSocket URL, e.g.
	// "ws://camoufox:3000"). When set and AccessToken is empty, FetchChannel
	// attempts the camoufox path (currently stubbed). Source: env CAMOUFOX_URL.
	CamoufoxURL string
	// GraphAPIURL overrides the Graph API root. Test hook only — leave empty
	// in production.
	GraphAPIURL string
	// APIVersion overrides the API version segment (e.g. "v19.0"). Default
	// "v19.0".
	APIVersion string
}

// New constructs a FacebookParser with sensible defaults.
func New(cfg Config) *FacebookParser {
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 15 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "w-popularity-parser-facebook/1.0"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	if cfg.GraphAPIURL == "" {
		cfg.GraphAPIURL = defaultGraphAPI
	}
	if cfg.APIVersion == "" {
		cfg.APIVersion = defaultAPIVersion
	}
	return &FacebookParser{cfg: cfg}
}

// FacebookParser is the public entry point.
type FacebookParser struct{ cfg Config }

// Platform implements shared.Parser.
func (p *FacebookParser) Platform() shared.Platform { return shared.PlatformFacebook }

// Compile-time interface check.
var _ shared.Parser = (*FacebookParser)(nil)

// FetchChannel routes to Graph API when AccessToken is set, falls through to
// camoufox when CamoufoxURL is set, otherwise returns ErrAuth with a hint.
func (p *FacebookParser) FetchChannel(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	if handle == "" {
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: empty handle", shared.ErrNotFound)
	}
	if p.cfg.AccessToken != "" {
		return p.fetchChannelGraph(ctx, handle)
	}
	if p.cfg.CamoufoxURL != "" {
		return p.fetchChannelCamoufox(ctx, handle)
	}
	return shared.ChannelSnapshot{}, fmt.Errorf("%w: set FACEBOOK_ACCESS_TOKEN or CAMOUFOX_URL", shared.ErrAuth)
}

// FetchRecentPosts uses the Graph /{page-id}/posts endpoint when AccessToken
// is set. Camoufox fallback is a stub.
func (p *FacebookParser) FetchRecentPosts(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	if handle == "" {
		return nil, fmt.Errorf("%w: empty handle", shared.ErrNotFound)
	}
	if p.cfg.AccessToken != "" {
		return p.fetchPostsGraph(ctx, handle, since)
	}
	if p.cfg.CamoufoxURL != "" {
		_, err := p.fetchViaCamoufox(ctx, p.profileURL(handle))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", shared.ErrAuth, err)
		}
		// If a real camoufox path ever returns nil, we'd parse HTML here.
		// For now this branch is unreachable.
		return nil, nil
	}
	return nil, fmt.Errorf("%w: set FACEBOOK_ACCESS_TOKEN or CAMOUFOX_URL", shared.ErrAuth)
}

// --- Graph API ---------------------------------------------------------------

// graphPage is a subset of the Page node returned by /{page-id}.
//
// Field rationale:
//   - followers_count: preferred follower number for a Page. Newer than fan_count.
//   - fan_count: legacy "Likes" counter. Still populated for some Pages.
//   - id: numeric page id, needed for /{id}/posts.
//   - link, about, verification_status: surface in Raw for downstream tooling.
//
// Field availability varies by token scope and Page settings. We tolerate
// missing fields and emit zero values rather than erroring.
type graphPage struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	FollowersCount     int64  `json:"followers_count"`
	FanCount           int64  `json:"fan_count"`
	About              string `json:"about"`
	Link               string `json:"link"`
	VerificationStatus string `json:"verification_status"`
	Error              *graphError `json:"error,omitempty"`
}

// graphPostsResp is the envelope for /{page-id}/posts.
type graphPostsResp struct {
	Data  []graphPost `json:"data"`
	Error *graphError `json:"error,omitempty"`
}

// graphTime wraps time.Time to handle Facebook's idiosyncratic ISO-8601
// variant: `2026-05-01T12:00:00+0000` (no colon in the timezone offset).
// Go's RFC3339 parser refuses that form, so we try a couple of layouts.
type graphTime struct{ time.Time }

func (g *graphTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	layouts := []string{
		"2006-01-02T15:04:05-0700", // Graph's default
		time.RFC3339,
		time.RFC3339Nano,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			g.Time = t.UTC()
			return nil
		}
	}
	return fmt.Errorf("graphTime: cannot parse %q", s)
}

// graphPost is one item from /{page-id}/posts. Reactions / comments / shares
// arrive in nested envelopes that we squash into flat counters.
type graphPost struct {
	ID           string    `json:"id"`
	CreatedTime  graphTime `json:"created_time"`
	Message      string    `json:"message"`
	PermalinkURL string    `json:"permalink_url"`
	Shares       struct {
		Count int64 `json:"count"`
	} `json:"shares"`
	Reactions struct {
		Summary struct {
			TotalCount int64 `json:"total_count"`
		} `json:"summary"`
	} `json:"reactions"`
	Comments struct {
		Summary struct {
			TotalCount int64 `json:"total_count"`
		} `json:"summary"`
	} `json:"comments"`
}

// graphError is the standard error envelope returned by Graph API on 4xx/5xx.
// Both top-level fields are present in practice; we expose Code/Subcode so
// the caller can disambiguate "invalid token" from "unknown field" etc.
type graphError struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Code      int    `json:"code"`
	Subcode   int    `json:"error_subcode"`
	FBTraceID string `json:"fbtrace_id"`
}

// fetchChannelGraph calls /{handle} with the documented field set and maps
// the result onto shared.ChannelSnapshot.
func (p *FacebookParser) fetchChannelGraph(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	q := url.Values{}
	q.Set("fields", "id,name,followers_count,fan_count,about,link,verification_status")
	q.Set("access_token", p.cfg.AccessToken)

	u := p.graphURL(handle, q)
	var page graphPage
	status, err := p.getJSON(ctx, u, &page)
	if err != nil {
		return shared.ChannelSnapshot{}, err
	}
	if page.Error != nil {
		return shared.ChannelSnapshot{}, mapGraphError(status, page.Error)
	}

	followers := page.FollowersCount
	if followers == 0 {
		followers = page.FanCount
	}

	raw := map[string]interface{}{"source": "graph_api"}
	if page.ID != "" {
		raw["page_id"] = page.ID
	}
	if page.About != "" {
		raw["about"] = page.About
	}
	if page.Link != "" {
		raw["link"] = page.Link
	}
	if page.VerificationStatus != "" {
		raw["verification_status"] = page.VerificationStatus
	}
	if page.Name != "" {
		raw["name"] = page.Name
	}

	profileURL := page.Link
	if profileURL == "" {
		profileURL = p.profileURL(handle)
	}

	return shared.ChannelSnapshot{
		Platform:  shared.PlatformFacebook,
		Handle:    handle,
		URL:       profileURL,
		FetchedAt: time.Now().UTC(),
		Followers: followers,
		// PostsCount is not exposed by /{page-id}; counting requires
		// paginating /{page-id}/posts which we deliberately skip here.
		PostsCount: 0,
		Raw:        raw,
	}, nil
}

// fetchPostsGraph calls /{handle}/posts with edge summaries enabled.
//
// We use the handle directly — Graph resolves handle-or-id transparently for
// the /posts edge. For the rare handle that contains URL-unsafe characters we
// path-escape it.
func (p *FacebookParser) fetchPostsGraph(ctx context.Context, handle string, since time.Time) ([]shared.PostSnapshot, error) {
	q := url.Values{}
	q.Set("fields", "id,created_time,message,permalink_url,shares,reactions.summary(total_count),comments.summary(total_count)")
	q.Set("limit", "50")
	if !since.IsZero() {
		// since must be a unix timestamp per Graph docs.
		q.Set("since", strconv.FormatInt(since.UTC().Unix(), 10))
	}
	q.Set("access_token", p.cfg.AccessToken)

	u := p.graphURL(handle+"/posts", q)
	var env graphPostsResp
	status, err := p.getJSON(ctx, u, &env)
	if err != nil {
		return nil, err
	}
	if env.Error != nil {
		return nil, mapGraphError(status, env.Error)
	}

	now := time.Now().UTC()
	out := make([]shared.PostSnapshot, 0, len(env.Data))
	for _, post := range env.Data {
		published := post.CreatedTime.Time
		if !since.IsZero() && !published.IsZero() && published.Before(since) {
			continue
		}
		permalink := post.PermalinkURL
		if permalink == "" {
			permalink = fmt.Sprintf("https://www.facebook.com/%s", post.ID)
		}
		raw := map[string]interface{}{"source": "graph_api"}
		if post.Message != "" {
			raw["message"] = post.Message
		}
		out = append(out, shared.PostSnapshot{
			Platform:      shared.PlatformFacebook,
			ChannelHandle: handle,
			PostID:        post.ID,
			URL:           permalink,
			Kind:          shared.PostKindPost,
			PublishedAt:   published,
			FetchedAt:     now,
			Likes:         post.Reactions.Summary.TotalCount,
			Comments:      post.Comments.Summary.TotalCount,
			Shares:        post.Shares.Count,
			Raw:           raw,
		})
	}
	return out, nil
}

// graphURL builds {base}/{version}/{path}?{query}.
func (p *FacebookParser) graphURL(path string, q url.Values) string {
	base := strings.TrimRight(p.cfg.GraphAPIURL, "/")
	// path may contain a slash (e.g. "handle/posts"); only the leading
	// handle segment needs escaping. Splitting on the first / preserves the
	// edge name.
	parts := strings.SplitN(path, "/", 2)
	parts[0] = url.PathEscape(parts[0])
	escaped := strings.Join(parts, "/")
	return fmt.Sprintf("%s/%s/%s?%s", base, p.cfg.APIVersion, escaped, q.Encode())
}

// profileURL returns the canonical facebook.com URL for a handle. Used as a
// fallback when Graph does not return `link`.
func (p *FacebookParser) profileURL(handle string) string {
	return fmt.Sprintf("https://www.facebook.com/%s/", url.PathEscape(handle))
}

// getJSON GETs u and decodes the body into dst. It returns the HTTP status so
// callers can distinguish 4xx-with-graphError from network failures.
//
// 5xx responses short-circuit to ErrTransient; we do not try to decode them.
// 429 short-circuits to ErrRateLimited.
func (p *FacebookParser) getJSON(ctx context.Context, u string, dst interface{}) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: build request: %v", shared.ErrTransient, err)
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, err
		}
		return 0, fmt.Errorf("%w: http: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, fmt.Errorf("%w: graph returned 429", shared.ErrRateLimited)
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, fmt.Errorf("%w: graph returned %d", shared.ErrTransient, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%w: read body: %v", shared.ErrTransient, err)
	}
	// Graph returns the error envelope inside a 200 sometimes for batch calls,
	// and inside a 4xx for the canonical case. Either way, decoding the body
	// onto dst surfaces dst.Error which our callers inspect.
	if err := json.Unmarshal(body, dst); err != nil {
		return resp.StatusCode, fmt.Errorf("%w: parse json: %v", shared.ErrTransient, err)
	}
	return resp.StatusCode, nil
}

// mapGraphError translates Graph's error envelope to one of our sentinels.
//
// Notable codes (https://developers.facebook.com/docs/graph-api/guides/error-handling):
//   - 100 + subcode 33 / unknown field → object not found.
//   - 190                              → invalid/expired token.
//   - 4 / 17 / 32 / 613                → throttling.
func mapGraphError(status int, e *graphError) error {
	switch {
	case e.Code == 190:
		return fmt.Errorf("facebook: %w: graph code 190: %s", shared.ErrAuth, e.Message)
	case e.Code == 100:
		// 100 covers both "unknown field" and "object does not exist" cases.
		// In practice production callers care that the handle is unresolved,
		// so we map both to ErrNotFound.
		return fmt.Errorf("facebook: %w: graph code 100: %s", shared.ErrNotFound, e.Message)
	case e.Code == 4, e.Code == 17, e.Code == 32, e.Code == 613:
		return fmt.Errorf("facebook: %w: graph code %d: %s", shared.ErrRateLimited, e.Code, e.Message)
	}
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("facebook: %w: %s", shared.ErrNotFound, e.Message)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("facebook: %w: %s", shared.ErrRateLimited, e.Message)
	case status >= 500:
		return fmt.Errorf("facebook: %w: graph %d: %s", shared.ErrTransient, status, e.Message)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("facebook: %w: graph %d: %s", shared.ErrAuth, status, e.Message)
	}
	return fmt.Errorf("facebook: graph %d (code %d/%d): %s", status, e.Code, e.Subcode, e.Message)
}

// --- camoufox ---------------------------------------------------------------

// fetchChannelCamoufox is the camoufox-side equivalent of fetchChannelGraph.
// It calls fetchViaCamoufox to obtain the HTML and would parse counters out
// of it once the real implementation lands.
func (p *FacebookParser) fetchChannelCamoufox(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	_, err := p.fetchViaCamoufox(ctx, p.profileURL(handle))
	if err != nil {
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: %v", shared.ErrAuth, err)
	}
	// Unreachable until fetchViaCamoufox is implemented. When it is, parse the
	// returned HTML into a ChannelSnapshot here.
	return shared.ChannelSnapshot{}, fmt.Errorf("%w: camoufox returned no HTML", shared.ErrAuth)
}

// fetchViaCamoufox is the connection layer for the eventual CDP-based fetch.
//
// Contract:
//   - If Config.CamoufoxURL is empty, return errors.New("camoufox not
//     configured") so callers can map to ErrAuth.
//   - Otherwise, return errors.New("camoufox path not implemented") — the
//     branching is wired (FetchChannel routes here) but the Playwright/CDP
//     client is intentionally deferred to a follow-up.
//
// TODO: implement using github.com/playwright-community/playwright-go or a
// minimal chromedp connection. Suggested shape:
//
//	pw, err := playwright.Run()
//	browser, err := pw.Chromium.ConnectOverCDP(p.cfg.CamoufoxURL)
//	page, err := browser.NewPage()
//	_, err = page.Goto(targetURL, …)
//	html, err := page.Content()
//	return []byte(html), nil
//
// Once it returns real HTML, fetchChannelCamoufox can parse it the same way
// fetchChannelGraph parses the API response (extract fan_count /
// follower_count from the embedded JSON blobs).
func (p *FacebookParser) fetchViaCamoufox(ctx context.Context, targetURL string) ([]byte, error) {
	if p.cfg.CamoufoxURL == "" {
		return nil, errors.New("camoufox not configured")
	}
	_ = ctx
	_ = targetURL
	return nil, errors.New("camoufox path not implemented")
}
