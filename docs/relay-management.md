# Relay Management API (NIP-86) and the admin page

Haven implements [NIP-86](https://github.com/nostr-protocol/nips/blob/master/86.md), the relay management API, so
the owner can ban a pubkey, drop a stored event, block an address or rename a relay without a shell on the box. It
ships with a web page at `/admin` that drives the API from a nostr browser extension.

Only the relay owner (`OWNER_NPUB`) can use it. There is no way to delegate access.

## Endpoints

NIP-86 lives on the same URL as the relay's websocket, so Haven has four of them — one per relay, and a call applies
to the relay it was sent to:

| URL | Relay |
|---|---|
| `https://your.relay` | outbox |
| `https://your.relay/private` | private |
| `https://your.relay/chat` | chat |
| `https://your.relay/inbox` | inbox |

A request is a `POST` with `Content-Type: application/nostr+json+rpc` and a JSON body:

```json
{ "method": "banpubkey", "params": ["<32-byte-hex-pubkey>", "spam"] }
```

The response is `{"result": ...}` or `{"error": "..."}`.

## Authorization

Every call carries a [NIP-98](https://github.com/nostr-protocol/nips/blob/master/98.md) event, base64 encoded, in an
`Authorization: Nostr <base64>` header. The event must be:

- kind `27235`, correctly signed by `OWNER_NPUB`
- tagged `u` with the relay's URL — the one you are POSTing to
- tagged `method` with `POST`
- tagged `payload` with the hex sha256 of the exact request body (NIP-86 makes this mandatory)
- stamped `created_at` within 60 seconds of the relay's clock, in either direction

Anything else is a `401`. If every call fails with a `created_at` complaint, your clock is wrong — the admin page
detects this and says so, but a command line client will not.

### Calling it by hand

```sh
BODY='{"method":"listbannedpubkeys","params":[]}'
PAYLOAD=$(printf '%s' "$BODY" | sha256sum | cut -d' ' -f1)
AUTH=$(nak event -k 27235 -c "" \
  -t "u=https://your.relay" -t "method=POST" -t "payload=$PAYLOAD" \
  --sec <owner-nsec> | base64 -w0)

curl -H "Content-Type: application/nostr+json+rpc" \
     -H "Authorization: Nostr $AUTH" \
     --data-binary "$BODY" https://your.relay
```

The body has to be byte-identical between the hash and the request, or the `payload` tag will not match.

## Methods

`supportedmethods` lists them all. Some are relay-wide and some apply only to the relay you sent them to:

| Method | Scope | Notes |
|---|---|---|
| `supportedmethods` | — | |
| `banpubkey`, `unbanpubkey`, `listbannedpubkeys` | all relays | stops a pubkey writing anything |
| `allowpubkey`, `unallowpubkey`, `listallowedpubkeys` | all relays | grants the same access as whitelisting |
| `banevent`, `allowevent`, `listbannedevents`, `listallowedevents` | one relay | |
| `changerelayname`, `changerelaydescription`, `changerelayicon` | one relay | |
| `allowkind`, `disallowkind`, `listallowedkinds`, `listdisallowedkinds` | one relay | |
| `blockip`, `unblockip`, `listblockedips` | all relays | |
| `listblobs`, `blobstats` | all relays | not part of NIP-86; see [Media](#media) |
| `deleteblob`, `deleteblobs` | all relays | deletes blossom media, permanently |
| `blockblob`, `unblockblob`, `listblockedblobs` | all relays | refuses a hash on upload and stops serving it |
| `listorphanblobs`, `deleteorphanblobs` | all relays | reconciles the blob index against the disk |
| `listevents`, `getevent` | one relay | not part of NIP-86; see [Notes](#notes) |
| `deleteevent`, `deleteevents` | one relay | removes stored events; **not** a ban |
| `dashboard` | all relays | not part of NIP-86; see [Dashboard](#dashboard) |
| `stats` | one relay | not part of NIP-86; see below |

Not implemented: the roles API (NIP-86 gives roles no permission semantics and Haven has no role concept),
`listeventsneedingmoderation` (Haven has no moderation queue), and `grantadmin`/`revokeadmin` (never part of the
spec).

## Where the state lives

Everything the API changes is written to `management.json` — the path is `MANAGEMENT_STATE_FILE`, and the file is
created on first use. Do not edit it while the relay is running.

If the file cannot be read or parsed at startup, Haven logs the reason, keeps serving, and makes the management API
**read-only** until it is fixed. It will not overwrite a file it could not understand.

Set `MANAGEMENT_API_ENABLED=false` to make the endpoint and the admin page unreachable altogether.

## Two sources of bans, one of which is read-only

Haven has two ban lists and they are unioned: the one this API writes, and the kind `10084` list the owner publishes
(see [access control](access-control.md#banning-users)). `listbannedpubkeys` reports a `source` on every entry
(`"api"`, `"list"`, `"file"` or `"owner"`) so you can tell them apart.

**The API cannot lift a ban that came from your kind 10084 list.** Haven holds no private key — only `OWNER_NPUB` —
so it cannot sign a replacement list. `unbanpubkey` on such a pubkey clears the relay's own record and then tells you
the ban is still standing; publish an updated list from a nostr client to remove it.

The same applies to `unallowpubkey` against a pubkey in `WHITELISTED_NPUBS_FILE`: Haven will not rewrite a file you
maintain by hand.

The API refuses to ban the owner, to un-allow the owner, and to block a loopback or private address — behind a
reverse proxy that does not set `X-Forwarded-For`, every client looks like `127.0.0.1`, so blocking it would take the
relay offline for everybody.

## Banning events

`banevent` deletes the stored copy from that relay's database and records the id, so re-publishing it is refused and
the import paths skip it. It does **not** publish a NIP-09 delete request: Haven has no key to sign one with. Other
relays keep their copies, and nothing is blasted onwards.

`allowevent` is the inverse of `banevent`, not a restore. It lifts the block on re-publishing; the deleted copy does
not come back, and it does not resurrect anything deleted with a NIP-09 request.

## Kind rules

Kind rules **narrow** what a relay accepts and never widen it, because they run alongside every other policy:

- an empty allow list means no allow-list restriction — only the disallow list applies
- a non-empty allow list is exhaustive, intersected with everything else the relay enforces
- the disallow list wins when a kind is on both, and allowing a kind removes it from the disallow list

So `allowkind 1` on the chat relay accepts nothing: the chat relay only ever takes chat-related kinds, and the
intersection is empty. The inbox relay is the same for gift-wrapped DMs.

The owner's kind 5 delete requests are always accepted, so disallowing kind 5 cannot cost you the ability to delete.

## Blocking addresses

Blocks apply to websocket connections. A blocked address can still read the NIP-11 document, the landing page and
blossom blobs.

Blocking is only as trustworthy as your reverse proxy's `X-Forwarded-For` handling. A proxy that *appends* to the
header lets a client prepend a forged address and win — configure yours to replace it (see the reverse proxy section
in the [README](../README.md#6-set-up-a-reverse-proxy-optional)). This already affects Haven's rate limiters, so it
is worth getting right regardless.

## `stats`

Not part of NIP-86 — Haven defines the shape. It reports the relay, version, uptime, per-database event counts and
the size of each list, counted per source rather than summed. Event counts are a full scan of every database, so
they are cached for a minute; `events_counted_at` says how stale they are.

## Media

Blossom is served by the outbox relay alone, but there is one blob directory, one
index and one blocklist behind all four relays, so these methods are relay-wide and
answer on any of the four URLs. The admin page calls them on the outbox endpoint,
because that is where the media itself lives.

`listblobs` answers with everything the media browser needs in one call — the blobs,
a storage summary and the blocked hashes — because NIP-86 has no batching and each
call is one signature. It takes an optional `[offset, limit]`; the default and the
maximum page is 10,000 blobs, and `truncated` with `next_offset` says when there are
more. Reading a blob itself needs no authorization at all, so thumbnails in the admin
page cost nothing.

Two different things can shorten that list and they are reported separately.
`truncated` means the page hit the cap. `complete: false` means the relay could not
read its own blob index to the end, and some media is missing from the answer
altogether — the count comes from a separate exact count, so this is measured rather
than guessed.

One entry is one blob, not one upload. The index records an entry per uploader per
hash while the file on disk is shared, so several uploaders of the same bytes collapse
into a single row with an `owners` array. Deleting takes the file away from all of
them, which is why they are not listed separately.

### Deleting and blocking

`deleteblob` and `deleteblobs` remove every index entry for a hash and the file
itself. There is no trash, the URLs stop working immediately, and anything on nostr
embedding them breaks. Deleting something that is already half gone is not an error —
an index entry with no file, and a file with no index entry, are exactly the states
reconciliation exists to clean up. `deleteblobs` takes at most 500 hashes per call,
because the request body is capped at 64 KiB.

Deleting alone does not stop the same bytes being uploaded again a second later. Both
delete methods take an optional `block` flag, and `blockblob` does it on its own. A
blocked hash is refused on upload and no longer served:

- an upload naming the hash in its authorization event is refused with a `403` and a
  clear reason, including from `HEAD /upload`, so a well behaved client finds out
  before sending anything
- an upload that does not name it is stopped when the relay records it, which happens
  before a single byte reaches the disk
- a blob already stored answers `410` instead of its content

What blocking cannot do is recall what has already been fetched. Blobs are served with
a week of immutable caching, so any browser or CDN holding a copy keeps serving it.
Blocking stops distribution going forward; it is not a takedown.

### Reconciliation

`listorphanblobs` compares the index against the blob directory and names two
different problems:

- **files on disk with no index entry.** The relay serves a blob whether or not the
  index knows about it, so these are live public URLs that nothing in the media
  browser lists.
- **index entries with no file.** Their URLs already return 404.

Anything newer than an hour is held back and counted separately rather than listed. An
upload writes its index entry before it writes its file, so an upload in flight is
indistinguishable from one whose file was lost, and a cleanup that trusted the scan
would delete a blob that was about to land. The lists are therefore a truthful dry run
of exactly what `deleteorphanblobs` would remove.

`deleteorphanblobs` takes a mode: `index`, `files` or `both`. It rescans rather than
trusting the cached report, and it **refuses** to delete files when the index could
not be read in full — a file that looks unindexed may only look that way because the
entry pointing at it was unreadable.

> [!IMPORTANT]
> Haven's backups contain the blob **index**, not the blob **files**. Restoring a
> backup onto an empty blossom directory leaves every entry pointing at a missing
> file; restoring the files without `db/blossom` leaves every file unindexed. The
> admin page refuses to offer a cleanup when more than half the files are unindexed,
> because that shape is a lost database rather than a leak, and cleaning it would
> delete almost the whole library.

Walking the index and the directory is not free, so the answer is cached for a minute;
anything that changes a blob clears that cache immediately, so a delete is never
hidden behind it. `counted_at` says how old the numbers are.

## Notes

`listevents` reads one relay's own store, newest first. It takes a single **object**
parameter rather than positional ones, because it has eight of them:

```json
{ "method": "listevents", "params": [{
    "kinds":   [1, 30023],
    "authors": ["<64 hex>"],
    "ids":     ["<64 hex>"],
    "since":   1724630400,
    "until":   1799999999,
    "search":  "some words",
    "limit":   200,
    "cursor":  "hv1.eyJ1Ijo…"
}] }
```

Every field is optional, and an option it does not recognise is an **error** rather
than something quietly ignored — a mistyped `kind` where `kinds` was meant would
otherwise return every event on the relay with a delete button beside each one.

The answer distinguishes three different reasons it might be short, because they are
three different things:

| field | meaning |
|---|---|
| `truncated` | the page filled up; `next_cursor` resumes it |
| `complete: false` | the filter's range was not walked to its end because the scan budget ran out; `scanned` against `scan_budget` says by how much, and `next_cursor` resumes the **scan** rather than restarting it |
| `warning` | something was skipped outright and cannot be resumed |

`total` is every event on that relay, from the same cache `stats` uses, with
`counted_at` saying how old the number is.

**`profiles` is part of the answer.** It maps each author on the page to their kind 0
content, resolved from what this relay already stores. The admin page's CSP is
`connect-src 'self'`, so it cannot fetch a profile from anywhere, and a second
management call would cost a second signature — so names ride along with the page
that needed them. An author with no locally stored kind 0 is simply absent, and the
page falls back to a shortened npub. Nothing is fetched from anywhere else to fill
the gap, which would leak your interest in a pubkey to whatever relay was asked.

**`search` is a substring scan, not NIP-50.** Neither event store implements NIP-50 —
worse, both answer a filter carrying a search string by closing the channel with
nothing in it, so passing one through would report an empty result as a real one.
Haven scans for the substring itself, over `content`, bounded by `scan_budget`
(20,000 events). `scanned` and `complete` are how you can tell it looked at
everything.

`getevent` returns one whole event untruncated, plus whether it is banned, whether
this relay holds a NIP-09 request covering it, and whether deleting it would keep it
out.

### Deleting is not banning

`deleteevent` and `deleteevents` (up to 500 ids per call) remove the stored copies
from that relay's database. That is all they do:

- Nothing is published. Haven holds no private key, so it cannot sign a NIP-09
  deletion on your behalf, and other relays keep their copies.
- The author — or anyone else holding the event — can publish it to this relay again,
  and an import or a JSONL restore will put it straight back.

Each id comes back with its own outcome, and `permanent` says whether anything
actually stops it returning: true only when the id is on this relay's banned list, or
when the relay already holds a delete request covering it. When `permanent` is less
than `removed`, the answer carries a `warning` saying so in as many words. **Use
`banevent` when you want it to stay gone.**

## Dashboard

`dashboard` takes no parameters and answers the whole analytics page in one call,
because every call is a signature. It returns:

- `ranges` — `24h`, `7d` and `30d` **together**, already bucketed (hourly for the
  first two, daily for the third), so switching between them on the page costs
  nothing further. A bucket with no record is `null`, never `0`: an hour before
  analytics was switched on, or an hour the relay was down, is not an hour with no
  traffic, and drawing it as zero would put a floor under a blackout.
- `live` — per relay: current connections, the totals since this process started, and
  the last full pass over that database (`corpus`).
- `events` / `counted_at` — the same cached per-database counts `stats` reports, so
  the two never disagree.
- `blobs` — the blossom inventory, but only if it was already cached. A dashboard
  refresh must not be the thing that triggers a walk of fifty thousand files, so this
  is `null` rather than stale.
- `analytics` — whether counting is on, whether it is being **persisted**, when
  history begins, and the retention windows.

Counters come from hooks on each relay: one at the front of khatru's reject chain
counting what was offered, one at the back counting what survived every policy, and
the difference is the rejections. They are folded into hourly buckets every fifteen
seconds and written to `ANALYTICS_STATE_FILE` every `ANALYTICS_FLUSH_MINUTES` and on
every hour boundary. If that file cannot be read at startup, Haven keeps counting in
memory, keeps serving the dashboard, and **stops writing** — `analytics.persisted`
goes false and the page says so, because history is the one thing here that cannot be
recreated.

`uptime_seconds` is counted per bucket for exactly one reason: it is what lets you
tell an idle hour apart from an hour Haven was not running.

The kind mix, top authors and stored byte totals need a full walk of every database,
which the backends run without ever checking for cancellation — so they are computed
by a background goroutine every `ANALYTICS_AGGREGATE_MINUTES` and never on the
request. Until the first pass finishes the answer says so rather than reporting zero.

> [!NOTE]
> Stored size is the summed size of the events themselves, not the size of the
> database directory. Badger preallocates sparse value logs, so a directory walk
> reports gigabytes for a relay holding a handful of events.

## The admin page

`https://your.relay/admin`, signing with a [NIP-07](https://github.com/nostr-protocol/nips/blob/master/07.md)
browser extension such as nos2x or Alby. A NIP-46 bunker that injects `window.nostr` works too. Your key never
reaches the page or the relay.

The page has four views. **Dashboard** graphs the relay: traffic, connections, accepted against rejected, the kind
mix, top authors and storage. It is read-only, it covers all four relays, and one signature loads every time range
at once so switching between 24h, 7d and 30d afterwards is free.

**Notes** browses the events one relay is storing — filter and search what is loaded for nothing, ask the relay for
more at one signature a page, open any event for its tags and raw JSON, and delete. Media served by this relay's own
blossom is shown inline; anything hosted elsewhere stays behind a click, because fetching it tells that host your IP
address.

**Moderation** holds the panels: those down the left apply to the relay selected at the top, those down the right
apply to all four. Each panel prints the endpoint it calls.

**Media** browses everything on the blossom server — filter, search and sort it, select in bulk, delete, block a
hash, and reconcile the index against the disk. It has no relay switcher, because blossom is served by the outbox
relay alone. The whole library arrives in one call, so everything after that costs no further signatures, and
thumbnails are plain unauthenticated blob reads.

NIP-86 has no batching, so every action costs one signature — panels load on demand rather than all at once, and it
is worth using your signer's "always allow for this site" option if it has one.

> [!IMPORTANT]
> The page needs a **secure context** to sign anything: `crypto.subtle` does not exist otherwise, and most
> extensions will not load. Reach it over `https://`, or from `http://localhost` — for example
> `ssh -L 3355:localhost:3355 you@your-server`, then open `http://localhost:3355/admin`. Onion addresses count as
> secure and work as they are.

The page loads Tailwind and Inter from a CDN, as the relay's landing page already does. If those are unreachable —
the normal case for a Tor-only relay — it falls back to plain styles and stays usable.

---

[README](../README.md)
