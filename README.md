# w-popularity-parser-facebook

`facebook` parser for [w_popularity](https://github.com/suenot/w-popularity).

## Status

Best-effort logged-out HTTP scrape + camoufox fallback skeleton.

In production, **Facebook requires camoufox + valid session cookies** to return
anything useful from a public profile URL. A vanilla
`curl -A 'Mozilla/5.0' https://www.facebook.com/<handle>/` reliably yields one
of:

- HTTP 302 to `/login/?next=...`
- HTTP 400 with a tiny "Sorry, something went wrong" stub page
- HTTP 200 with a login form and no real profile content

No `fan_count`, no posts, no follower numbers in any of those.

## Strategy

1. **Primary — logged-out HTML scrape.** GET the profile URL with a realistic
   desktop UA. Scan the response body for embedded JSON counters
   (`fan_count`, `follower_count`, `followers_count`). On the rare profile
   that leaks them, we return a snapshot. On a login wall we surface
   `shared.ErrAuth` with a hint that camoufox + cookies are required.

2. **Fallback — camoufox via CDP (skeleton only).** When `Config.CamoufoxURL`
   is set, the parser will eventually drive a headless camoufox browser via
   Chrome DevTools Protocol to render the page with a real session. The
   `fetchViaCamoufox` function is in place but returns
   `"camoufox path not yet implemented"` — the real CDP wiring is the next PR.

## HTTP status → error mapping

| Status            | Result                                          |
| ----------------- | ----------------------------------------------- |
| 200 + counter     | `ChannelSnapshot{Followers: N}`                 |
| 200 + login form  | `shared.ErrAuth` (camoufox hint)                |
| 200 + no signal   | `shared.ErrAuth` (no-signal note)               |
| 400               | treated as login wall → `shared.ErrAuth`        |
| 404               | `shared.ErrNotFound`                            |
| 429               | `shared.ErrRateLimited`                         |
| 5xx               | `shared.ErrTransient`                           |

`FetchRecentPosts` is intentionally a no-op for the logged-out path; it
returns `(nil, nil)` because nothing reliable is visible without auth.

## Usage

```go
import parser "github.com/suenot/w-popularity-parser-facebook"

p := parser.New(parser.Config{
    CamoufoxURL: os.Getenv("CAMOUFOX_URL"), // e.g. "http://camoufox:3000"
})
snap, err := p.FetchChannel(ctx, "soloviov.evgeniy")
```

## License

MIT
