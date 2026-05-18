# w-popularity-parser-facebook

`facebook` parser for [w_popularity](https://github.com/suenot/w-popularity).

## Status

Graph API v19 (primary) + camoufox via CDP (skeleton).

The logged-out HTML approach was abandoned. A vanilla
`curl -A 'Mozilla/5.0' https://www.facebook.com/<handle>/` reliably yields
either a 302 to `/login/?next=…`, a 400 stub page, or a 200 with a login form —
no `fan_count`, no `follower_count`, no posts. We do not even try it.

## Strategy

1. **Primary — Graph API v19.** When `Config.AccessToken` is set we hit
   `https://graph.facebook.com/v19.0/<handle>?fields=id,name,followers_count,fan_count,about,link,verification_status`.
   This requires a **Page Access Token** with `pages_read_engagement` scope.
   User profiles do not expose counters to third-party tokens — Pages only.

2. **Fallback — camoufox via CDP (skeleton only).** When `Config.CamoufoxURL`
   is set we will eventually drive a headless camoufox over Chrome DevTools
   Protocol with a real session cookie. The branching is wired but the fetch
   stub returns `"camoufox path not implemented"` so callers fail loudly until
   the implementation lands.

3. **Neither configured.** `FetchChannel` / `FetchRecentPosts` return
   `shared.ErrAuth` with the hint
   `"set FACEBOOK_ACCESS_TOKEN or CAMOUFOX_URL"`.

## Mapping

`FetchChannel` (`/{handle}?fields=…`):

| Graph field           | Snapshot field             |
| --------------------- | -------------------------- |
| `followers_count`     | `Followers`                |
| `fan_count`           | `Followers` (fallback)     |
| `id`                  | `Raw["page_id"]`           |
| `about`               | `Raw["about"]`             |
| `link`                | `Raw["link"]`, `URL`       |
| `verification_status` | `Raw["verification_status"]` |
| `name`                | `Raw["name"]`              |

`PostsCount` is **not** filled — counting requires paginating the `/posts`
edge. Callers that need it should call `FetchRecentPosts` and look at the
length (with an explicit `since` bound).

`FetchRecentPosts` (`/{handle}/posts?fields=…&limit=50`):

| Graph field                          | Snapshot field |
| ------------------------------------ | -------------- |
| `id`                                 | `PostID`       |
| `created_time`                       | `PublishedAt`  |
| `permalink_url`                      | `URL`          |
| `message`                            | `Raw["message"]` |
| `reactions.summary.total_count`      | `Likes`        |
| `comments.summary.total_count`       | `Comments`     |
| `shares.count`                       | `Shares`       |

`PostKind` is always `post` from this edge (videos/reels live on different
edges; add them when needed).

## Error mapping

| Condition                            | Result                |
| ------------------------------------ | --------------------- |
| Token + 200 + valid payload          | snapshot              |
| Token + graph code 190               | `shared.ErrAuth`      |
| Token + graph code 100               | `shared.ErrNotFound`  |
| Token + graph code 4 / 17 / 32 / 613 | `shared.ErrRateLimited` |
| HTTP 429                             | `shared.ErrRateLimited` |
| HTTP 5xx                             | `shared.ErrTransient` |
| HTTP 401 / 403                       | `shared.ErrAuth`      |
| No token, no camoufox                | `shared.ErrAuth` + hint |
| Camoufox configured (today)          | `shared.ErrAuth` ("not implemented") |

## Obtaining a Page Access Token

1. Sign in at <https://developers.facebook.com/> and create (or pick) an app.
2. Open **My Apps → your app → Tools → Graph API Explorer**.
3. Choose your app from the **Meta App** dropdown.
4. Click **Generate Access Token**, select the Page you administer, and grant
   `pages_read_engagement` (plus `pages_show_list` if you need to discover
   pages first).
5. Copy the token — that is a short-lived User Access Token. To get the
   long-lived **Page Access Token**:

```bash
# Step 1: exchange short-lived user → long-lived user (60 days)
curl -G https://graph.facebook.com/v19.0/oauth/access_token \
  -d grant_type=fb_exchange_token \
  -d client_id=$FB_APP_ID \
  -d client_secret=$FB_APP_SECRET \
  -d fb_exchange_token=$SHORT_USER_TOKEN

# Step 2: list pages with long-lived per-Page tokens
curl -G https://graph.facebook.com/v19.0/me/accounts \
  -d access_token=$LONG_USER_TOKEN
```

The `access_token` field on each Page in the response is the long-lived
**Page Access Token** — that is what you set in `FACEBOOK_ACCESS_TOKEN`.

For full details see
[Access Tokens documentation](https://developers.facebook.com/docs/facebook-login/guides/access-tokens).

## Usage

```go
import parser "github.com/suenot/w-popularity-parser-facebook"

p := parser.New(parser.Config{
    AccessToken: os.Getenv("FACEBOOK_ACCESS_TOKEN"),
    CamoufoxURL: os.Getenv("CAMOUFOX_URL"), // optional, e.g. "ws://camoufox:3000"
})
snap, err := p.FetchChannel(ctx, "soloviov.evgeniy")
```

## License

MIT
