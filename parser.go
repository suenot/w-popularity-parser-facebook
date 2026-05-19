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
	"bytes"
	"context"
	"encoding/json"
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
		// Personal profiles don't expose a structured post stream the way
		// Page /posts does. We render the profile so the wrapper at least
		// proves auth works, but we don't try to extract individual posts
		// from the resulting HTML — that's a follow-up.
		if _, _, err := p.fetchViaCamoufox(ctx, p.profileURL(handle)); err != nil {
			return nil, fmt.Errorf("%w: %v", shared.ErrAuth, err)
		}
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

// camoufoxFetchRequest mirrors the JSON body accepted by POST /fetch on the
// camoufox HTTP wrapper (see camoufox/server.py).
type camoufoxFetchRequest struct {
	URL             string `json:"url"`
	Profile         string `json:"profile"`
	WaitForSelector string `json:"wait_for_selector,omitempty"`
	TimeoutMS       int    `json:"timeout_ms,omitempty"`
}

// camoufoxFetchResponse mirrors the response envelope.
type camoufoxFetchResponse struct {
	HTML           string `json:"html"`
	Status         int    `json:"status"`
	FinalURL       string `json:"final_url"`
	Title          string `json:"title"`
	CookiesPresent bool   `json:"cookies_present"`
	Error          string `json:"error,omitempty"`
}

// fetchChannelCamoufox is the camoufox-side equivalent of fetchChannelGraph.
// It POSTs the personal-profile URL to the camoufox wrapper, gets back the
// fully-rendered HTML (post-login), and walks the embedded JSON blobs for a
// follower count.
//
// Personal Facebook profiles do not always expose a public follower count,
// even when authenticated. When no counter is found we return a snapshot
// with Followers=0 plus a Raw["fetch_note"] diagnostic — *not* an error —
// because the parent scheduler should record "we successfully reached the
// page; counter not surfaced" as a real data point.
func (p *FacebookParser) fetchChannelCamoufox(ctx context.Context, handle string) (shared.ChannelSnapshot, error) {
	target := p.profileURL(handle)
	html, finalURL, err := p.fetchViaCamoufox(ctx, target)
	if err != nil {
		return shared.ChannelSnapshot{}, fmt.Errorf("%w: %v", shared.ErrAuth, err)
	}

	snap := parseProfileHTML(string(html), handle, target)
	if finalURL != "" {
		snap.URL = finalURL
	}
	return snap, nil
}

// fetchViaCamoufox POSTs the target URL to the camoufox wrapper, returns the
// rendered HTML body and the final URL after redirects.
//
// Network/transport failures and 5xx from the wrapper map to ErrTransient so
// the scheduler retries them. 4xx from the wrapper (e.g. invalid profile
// name) propagate as plain errors.
func (p *FacebookParser) fetchViaCamoufox(ctx context.Context, targetURL string) ([]byte, string, error) {
	if p.cfg.CamoufoxURL == "" {
		return nil, "", errors.New("camoufox not configured")
	}

	reqBody, err := json.Marshal(camoufoxFetchRequest{
		URL:     targetURL,
		Profile: "facebook",
		// Wait for the friends-link to render — the friend / follower
		// counter widget loads after the page shell. "body" returns too
		// early and leaves Followers=0 even on healthy fetches.
		WaitForSelector: `a[href*="/friends"]`,
		TimeoutMS:       30_000,
	})
	if err != nil {
		return nil, "", fmt.Errorf("%w: marshal camoufox request: %v", shared.ErrTransient, err)
	}

	endpoint := strings.TrimRight(p.cfg.CamoufoxURL, "/") + "/fetch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, "", fmt.Errorf("%w: build camoufox request: %v", shared.ErrTransient, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("%w: camoufox: %v", shared.ErrTransient, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", fmt.Errorf("%w: read camoufox body: %v", shared.ErrTransient, err)
	}

	var parsed camoufoxFetchResponse
	if jerr := json.Unmarshal(raw, &parsed); jerr != nil {
		return nil, "", fmt.Errorf("%w: parse camoufox response (http %d): %v", shared.ErrTransient, resp.StatusCode, jerr)
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusBadGateway {
		msg := parsed.Error
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return nil, "", fmt.Errorf("%w: camoufox: %s", shared.ErrTransient, msg)
	}
	if resp.StatusCode >= 400 {
		msg := parsed.Error
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return nil, "", fmt.Errorf("camoufox: %s", msg)
	}
	if parsed.HTML == "" {
		return nil, "", fmt.Errorf("%w: camoufox returned empty HTML", shared.ErrTransient)
	}
	return []byte(parsed.HTML), parsed.FinalURL, nil
}

// --- HTML extraction for the camoufox path ---------------------------------

// reFollowerKey matches any JSON key that looks like a follower count:
//
//	followers_count, follower_count, followersCount, followerCount,
//	num_followers, profile_follower_count, …
//
// We deliberately keep this case-insensitive and end-anchored so we don't
// match unrelated suffixes like "...follower_count_format" or "follow_count"
// from some unrelated edge.
var reFollowerKey = regexp.MustCompile(`(?i)follow(?:er)?s?_count$|(?i)follow(?:er)?sCount$|(?i)followerCount$`)

// reFanKey covers the legacy "Likes" counter still seen on some Pages.
var reFanKey = regexp.MustCompile(`(?i)^fan_count$|(?i)^fanCount$`)

// (no top-level regex needed — we extract balanced JSON via scanBalancedJSON)

// parseProfileHTML attempts to extract a follower count and display name
// from a fully-rendered Facebook profile page. Strategy: locate every
// `<script>...</script>` payload, JSON-decode any chunk that looks JSON-ish,
// and deep-walk for known keys. If nothing matches we return a soft success
// (Followers=0 + Raw["fetch_note"]).
func parseProfileHTML(body, handle, profileURL string) shared.ChannelSnapshot {
	now := time.Now().UTC()
	snap := shared.ChannelSnapshot{
		Platform:  shared.PlatformFacebook,
		Handle:    handle,
		URL:       profileURL,
		FetchedAt: now,
		Raw:       map[string]interface{}{"source": "camoufox_html"},
	}

	// Strategy 1: visible-text counters that Facebook renders into the DOM.
	// Profile top-card prints `<strong>N</strong>&nbsp;— друзья` (RU) or
	// `<strong>N</strong> friends` / `<strong>N</strong> followers`. This
	// surface is the most stable — JSON blobs change shape weekly.
	if applyVisibleCounters(&snap, body) {
		// keep going below so the JSON walker can still enrich Raw with name etc.
	}

	// Strategy 2: deep-walk every JSON blob in <script>. Picks up
	// fan_count/followers_count for business Pages and any leftover signals.
	scripts := regexp.MustCompile(`(?is)<script[^>]*>(.*?)</script>`).FindAllStringSubmatch(body, -1)
	for _, m := range scripts {
		if len(m) < 2 {
			continue
		}
		walkScriptForCounters(&snap, m[1])
	}

	// Best-effort display name from <title>. Useful when no JSON blobs matched.
	if name, ok := snap.Raw["name"].(string); !ok || name == "" {
		if t := extractTitle(body); t != "" {
			// FB titles look like "Display Name | Facebook" — strip suffix.
			t = strings.TrimSuffix(t, " | Facebook")
			// Strip notification-count prefix like "(3) " that FB prepends
			// when the logged-in account has unread items.
			t = regexp.MustCompile(`^\(\d+\)\s+`).ReplaceAllString(t, "")
			snap.Raw["name"] = strings.TrimSpace(t)
		}
	}

	// Final fan_count fallback (legacy Likes counter for Pages). Runs only
	// once at the top level so it can't beat followers_count due to map
	// iteration order inside walkJSONForCounters.
	if snap.Followers == 0 {
		if fc, ok := snap.Raw["fan_count"].(int64); ok && fc > 0 {
			snap.Followers = fc
			snap.Raw["followers_key"] = "fan_count"
		}
	}

	if snap.Followers == 0 {
		snap.Raw["fetch_note"] = "no follower count surfaced (personal profile may not expose one publicly)"
	}
	return snap
}

// applyVisibleCounters scrapes the rendered top-card counters. Patterns
// covered (locale-aware):
//   - <strong>289</strong>&nbsp;— друзья              (RU personal profile)
//   - <strong>289</strong>&nbsp;friends               (EN personal profile)
//   - <strong>1.2K</strong>&nbsp;followers            (EN with follower count enabled)
//   - 1,234 followers / 1,234 подписчиков             (alt formats)
//
// Friends and followers are both audience-ish; we prefer followers when both
// are present (since that's the broader "public reach" signal), else fall
// back to friends. Both numbers are persisted to Raw separately.
// Note: Facebook's <strong> tag carries dozens of obfuscated CSS classes
// (e.g. `<strong class="html-strong xdj266r x14z9mp …">289</strong>`), so
// we permit any attributes inside the opening tag. The separator between
// the number and the label can be a literal em-dash, en-dash, hyphen, or
// nothing at all, optionally surrounded by &nbsp; and whitespace.
var reFBVisibleCounter = regexp.MustCompile(`(?i)<strong[^>]*>([\d,.kKmM\xa0 ]+)</strong>(?:&nbsp;|\s)*(?:—|–|-)?\s*(друз|friend|follower|подписч)`)

func applyVisibleCounters(snap *shared.ChannelSnapshot, body string) bool {
	matches := reFBVisibleCounter.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return false
	}
	var friends, followers int64
	for _, m := range matches {
		n := parseCompactNumber(m[1])
		if n == 0 {
			continue
		}
		switch strings.ToLower(m[2]) {
		case "друз", "friend":
			if n > friends {
				friends = n
			}
		case "follower", "подписч":
			if n > followers {
				followers = n
			}
		}
	}
	if friends == 0 && followers == 0 {
		return false
	}
	if followers > 0 {
		snap.Followers = followers
	} else {
		snap.Followers = friends
	}
	if friends > 0 {
		snap.Raw["friend_count"] = friends
	}
	if followers > 0 {
		snap.Raw["follower_count"] = followers
	}
	snap.Raw["extracted_via"] = "visible_counters"
	return true
}

// parseCompactNumber handles "1,234", "1.2K", "5M", "1 234" (NBSP/space).
func parseCompactNumber(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mul := 1.0
	last := s[len(s)-1]
	switch last {
	case 'k', 'K':
		mul = 1_000
		s = s[:len(s)-1]
	case 'm', 'M':
		mul = 1_000_000
		s = s[:len(s)-1]
	}
	// strip thousands separators (comma, NBSP, regular space).
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ',', ' ', ' ':
			return -1
		}
		return r
	}, s)
	f, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0
	}
	return int64(f * mul)
}

// walkScriptForCounters extracts JSON chunks from a single <script> body
// (balanced {…} / […]) and decodes each. The deep-walker mutates snap.
//
// We try every {…} or […] that *can* be balanced. Facebook scripts embed
// many small JSON blobs separated by JS punctuation; we don't try to be
// clever, we just attempt every potential start position and let the JSON
// decoder reject invalid ones.
func walkScriptForCounters(snap *shared.ChannelSnapshot, scriptBody string) {
	for _, blob := range scanBalancedJSON(scriptBody) {
		var obj interface{}
		dec := json.NewDecoder(strings.NewReader(blob))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			continue
		}
		walkJSONForCounters(snap, obj)
	}
}

// scanBalancedJSON walks s and returns every balanced {…} or […] starting at
// a top-level `{` / `[`. Strings (with escapes) are tracked so braces inside
// quoted strings don't terminate the scan. Worst-case O(n²) on adversarial
// input but in practice limited by the size of FB's embedded blobs.
func scanBalancedJSON(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '{' && c != '[' {
			i++
			continue
		}
		end := matchBalanced(s, i)
		if end > i {
			blob := s[i : end+1]
			// Heuristic: only keep blobs that contain a key we care about.
			// Saves the JSON decoder a lot of work on tiny `{}` chunks.
			low := strings.ToLower(blob)
			if strings.Contains(low, "follow") || strings.Contains(low, "fan_count") || strings.Contains(low, "fancount") || strings.Contains(low, "\"name\"") {
				out = append(out, blob)
			}
			i = end + 1
		} else {
			i++
		}
	}
	return out
}

// matchBalanced returns the index of the bracket that closes the one at
// start, or -1 if unbalanced / runs off the end.
func matchBalanced(s string, start int) int {
	open := s[start]
	var close byte
	switch open {
	case '{':
		close = '}'
	case '[':
		close = ']'
	default:
		return -1
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if c == '\\' {
				esc = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// walkJSONForCounters descends maps and slices looking for follower/fan
// counter keys and a few interesting strings (name).
func walkJSONForCounters(snap *shared.ChannelSnapshot, node interface{}) {
	switch v := node.(type) {
	case map[string]interface{}:
		for k, val := range v {
			// Strings of interest.
			if (k == "name" || k == "display_name" || k == "title") {
				if s, ok := val.(string); ok && s != "" {
					if _, exists := snap.Raw["name"]; !exists {
						snap.Raw["name"] = s
					}
				}
			}
			// Followers.
			if reFollowerKey.MatchString(k) {
				if n, ok := numericField(val); ok && snap.Followers == 0 {
					snap.Followers = n
					snap.Raw["followers_key"] = k
				}
			}
			// Fan count (legacy Likes); only used if no follower count.
			if reFanKey.MatchString(k) {
				if n, ok := numericField(val); ok {
					if _, exists := snap.Raw["fan_count"]; !exists {
						snap.Raw["fan_count"] = n
					}
				}
			}
			// Recurse.
			walkJSONForCounters(snap, val)
		}
	case []interface{}:
		for _, item := range v {
			walkJSONForCounters(snap, item)
		}
	}
	// (fan_count → Followers fallback moved to parseProfileHTML — running
	// it inside recursion let fan_count beat followers_count when Go's
	// random map iteration happened to visit fan_count first.)
}

// extractTitle pulls the contents of <title>...</title>. Falls back to "".
func extractTitle(body string) string {
	re := regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// numericField coerces a JSON-ish value to int64. Mirrors the LinkedIn
// helper. Handles json.Number (because we configured UseNumber on the
// decoder), float64, int, string with optional commas.
func numericField(v interface{}) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(x), ",", "")
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}
