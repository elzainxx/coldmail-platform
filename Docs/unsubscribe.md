# The unsubscribe link and its public endpoint

Every message of a send job (module 2 or module 3) carries a per-recipient unsubscribe link. A recipient who follows it is shown one button; pressing it adds their address to the suppression list of the user
whose job it was, and that user's later jobs leave the address out. This document is the reference for the token, the two public routes, the dials and what an operator must set. The decisions behind it are
5, 6 and 7 of [`phase-5-spec.md`](phase-5-spec.md); the refusal a service without it gives is in [`jobs-api.md`](jobs-api.md); the dials are in [`config-reference.md`](config-reference.md).

## What a recipient sees

1. The link in the message: `https://<public address>/u/<token>`. A mail client that supports one-click unsubscribe (RFC 8058) uses the same link in a `List-Unsubscribe` header and posts to it without
   showing a page; every other recipient opens it in a browser.
2. A small page with one button, **Unsubscribe**. Opening the page changes nothing (a link prefetcher, a security scanner or a preview does not unsubscribe anybody); pressing the button does.
3. A plain page that says the request was recorded, that this sender will not include them in future mailings, and that a message already on its way may still arrive.

No page shows an address, a name, a user, a job or a count. A link that is wrong in any way shows one "This link is not valid" page.

## The token

```
token = base64url-without-padding( item id (16 raw bytes) || HMAC-SHA256( secret, "unsub|v1|" + item id )[:24] )        54 characters
```

- The item id is the row id the platform already hands every module (`public.job_items.id` = `platform.job_rows.id`), in the canonical lower-case uuid text inside the MAC. The address is not in the token and
  cannot be read from it: the endpoint resolves the item through the job store.
- The secret is `PLATFORM_UNSUBSCRIBE_SECRET` (at least 32 bytes). Nothing else is signed: there is no expiry and no version, so a link keeps working as long as the secret is kept, and **changing the
  secret makes every link already sent answer "not valid"**.
- The decoding is strict: exactly 54 characters of the URL-safe alphabet, no padding, no white space, and canonical (the last character carries two bits that must be zero, so an altered tail is refused and not read as
  the same token). The MAC is compared in constant time. A short token, a wrong alphabet, a wrong MAC, a token minted under another secret and an altered item id are all refused alike.
- The code is `internal/unsubscribe` (pure: no clock, no network, no database); its tests flip every single bit of a valid token and expect every one refused.

## The routes

Not behind the bearer authentication of `/v1/jobs`: a recipient has no account, and the token is the only credential. The routes are mounted only when both dials are set (see below).

| Route | What it does |
|---|---|
| `GET /u/{token}` | Verifies the token and that the item exists, and shows the confirm page. It writes nothing. |
| `POST /u/{token}` | Verifies the same, adds the item's address to its owner's suppression list (source `unsubscribe`, through `internal/suppress`, which validates the address and stores its normalised form) and shows the thank-you page. |

Any other method on a path under `/u/` is `405` with `Allow: GET, POST`.

**One-click (RFC 8058).** The POST a mail client sends has the content type `application/x-www-form-urlencoded` and the body `List-Unsubscribe=One-Click`; the form on the confirm page POSTs to the same path.
Both are the same request: whatever a POST's body is, it is read up to 1 KiB and ignored, the token is the whole request, and the address and the user are always the item's own (a body that names another
address or user changes nothing). A second POST is the same success and writes no second row.

| Status | When |
|---|---|
| `200` | the confirm page (GET), or the thank-you page (POST: the first one and every repeat) |
| `404` | the token is wrong in any way: too short or too long, a character outside the alphabet, a wrong MAC, minted under another secret, or signed for an item the platform has no row for. **One page, one status, one length** for all of them |
| `405` | a method other than GET and POST (`Allow: GET, POST`) |
| `429` | the client address is over its limit (`Retry-After`, whole seconds) |
| `500` | the store failed; the page says so and nothing more, the cause is in the log as a word |

The MAC is always computed before the store is asked anything, so a malformed token does not answer faster than a well-formed one with a wrong MAC; the store is asked only about a token whose MAC verified.

**Headers on every response**, whatever its status:

```
Cache-Control: no-store
X-Robots-Tag: noindex
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
Content-Security-Policy: default-src 'none'; form-action 'self'; style-src 'unsafe-inline'
```

The pages are static HTML with one inline style: no script, no image, no external resource, nothing to frame.

## The rate limit

A token bucket per client address, applied **before** any HMAC or database work and to every GET and POST that reaches the route, right or wrong. `PLATFORM_UNSUBSCRIBE_RATE_PER_MIN` (default 30, 1 to 600) is the
size of the bucket and its refill per minute: a client address can make that many requests at once and then one every `60 / rate` seconds. Over the limit is `429` with `Retry-After`.

- The address is the connection's remote address; an IPv6 address counts as its `/64`, an IPv4-mapped address as the IPv4 one.
- Memory is bounded: the 10000 most recently seen addresses are remembered; one that was forgotten starts again with a full bucket.
- **Behind a reverse proxy the remote address is the proxy's.** The platform does not read `X-Forwarded-For` (a header that is trusted by default is a header anybody can forge): until the deployment phase configures
  a trusted forwarded-for header, every recipient behind the proxy shares ONE bucket, so the dial should be set for the whole population that reaches the edge, not for one person.

## What an operator must set

| Dial | Meaning |
|---|---|
| `PLATFORM_UNSUBSCRIBE_SECRET` | the signing secret: at least 32 bytes of printable ASCII, never printed or logged, **not rotated** |
| `PLATFORM_PUBLIC_BASE_URL` | the public https address, for example `https://app.example.com`; a link is this, `/u/` and the token; whatever stands at that address must pass `/u/` to this service unchanged |
| `PLATFORM_UNSUBSCRIBE_RATE_PER_MIN` | the limit above |

Both of the first two are needed. With either unset the service still starts, says so once at start-up (`unsubscribe link is not configured`, naming which dial is missing and never a value), does not mount the
`/u/` routes (a request for one is the 404 envelope of any unknown path), and **refuses to confirm a send job**: `409 not_allowed` with `details.reason` `unsubscribe_not_configured`, before module 3 is
asked anything and before anything is held. A verify job is unaffected. The same applies to a store built without a link maker, which is the state of an old wiring: a send without a working link is not allowed. It also holds for a job that was confirmed while the dials were set and whose service later runs without them: the store writes **no** item for it (`EnsureJobItems` answers `not_allowed`, `unsubscribe_not_configured`), so the job waits and no message leaves without its link; set the dials again and the next engine pass writes the items.

## Where the link goes

- `public.job_items.unsubscribe_url` (migration `platform_0017`): NULL, or one https URL with no white space, angle bracket or control character, at most 2000 bytes (`job_items_unsubscribe_url_shape`). The job store
  (`internal/jobs`, the only writer) writes it for every item of a send job, together with the rest of the row; the same link is on the row the renderer sees (`EngineRow.UnsubscribeURL`), so what a template places
  for `{{unsubscribe_url}}` and what a sender puts in the header are one string. A single validated URL and not a map of headers, so no sender can be made to write another header.
- The senders read the column and emit `List-Unsubscribe: <url>` and `List-Unsubscribe-Post: List-Unsubscribe=One-Click` when it is set (chunk S4, each module's own change). A sender that does not read the column
  yet sends the message without the headers; the link in the body is the template's (chunk S2).

## What is logged

The token, the address and the user are never logged. The access log records the path of these routes as `/u/[token]`; a line that must name an item names a 12-character one-way tag of its id. The one
informational line is `unsubscribe recorded` with that tag and whether the row was newly written.

## Not checked, not done

- No real mailbox provider has been asked: how Gmail, Outlook and others treat the headers and the one-click POST is the owner's real-provider smoke test.
- A forwarded-for header is not trusted (see the rate limit); the public edge, DNS and TLS are the deployment phase's.
- A removal flow (taking an address off the suppression list) does not exist; nothing deletes a suppression.
- A job that is running when someone unsubscribes is not changed: the suppression list is applied when a job is confirmed, so the next job leaves the address out and this one may still deliver a message already on its way.
