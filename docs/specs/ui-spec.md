# Technical Specification: Zero-Knowledge CCTV Web Viewer (`zview`) — v2

**Status:** Draft v2.1 · **Depends on:** `contract.md` (the *Contract*). Formats, keys, the sidecar schema and the storage layout are defined there.

## 0. Changes from v1

* **App/data origin split** (§2).
  * The web app is served only from a trusted host; the storage host may run the data API but never serves code (Contract D8).
  * v1's claim that the NAS can be "fully compromised" was false while that NAS served the JavaScript.
* **Chunked `zrec` playback** (§4.3). Media Source Extensions (MSE / `ManagedMediaSource`) replace whole-file decrypt-to-Blob, which needed about 450 MiB transient for a single 300 s clip and would crash iPhone tabs. HTTP Range requests are now meaningful.
* **Crypto moves to a Dedicated Worker** (§4.2). The master key lives as a non-extractable `CryptoKey` in memory, or in IndexedDB when "remember this device" is on. v1 kept the hex string in `sessionStorage`.
* **Family onboarding** (§4.1): paste or QR scan of a checksummed `zk1-…` key, fingerprint verification against recordings, and a keyring for rotation. The passphrase option is removed; it had no KDF.
* **Integrity UX** (§4.4) surfaces tampering, truncation and sidecar/header mismatch.
* **Data server** (§3):
  * token auth derived from the shared key;
  * CORS scoped to the app origin;
  * clip access by ID instead of `/storage/*filePath` (path traversal);
  * lazy per-day index instead of in-memory SQLite;
  * `from`/`to` instant queries (UTC storage, local display);
  * coverage and status endpoints for "not monitored" shading.
* **Timeline** (§4.6) gains vehicle and other trigger colors plus non-color glyphs, per-camera lanes and coverage gaps.
* **Export** (§4.8) is now an explicit plaintext-to-disk action with confirmation and streaming save.

---

## 1. Scope

### 1.1 Goals

* A Synology Surveillance Station-style timeline for **event clips** (Contract D1).
* Runs on desktop and mobile browsers.
* Used by household family members who share one key (Contract D2).
* All decryption happens in the browser. Neither the data server nor storage ever holds the key.

### 1.2 Non-goals (v2)

* Live view.
* Continuous-recording playback.
* Multi-camera synchronized grid playback. The timeline shows all cameras, but one clip plays at a time.
* Per-user accounts or permissions.
* Hiding metadata from storage (Contract D3).

---

## 2. Deployment Topology & Trust

### 2.1 Two origins

| Origin | Serves | Trust | May run on |
|---|---|---|---|
| **App origin** | SPA bundle (HTML/JS/CSS/WASM-free), manifest, service worker | **Trusted** (Contract §2.2) | Relay host (`zview-server --mode app`), or any other host you control. **Never the NAS.** |
| **Data origin** | JSON API + ciphertext | Untrusted for confidentiality and code | NAS (`--mode data`), relay host, VPS |

Why the split matters:

* All data from the data origin is either authenticated ciphertext (Contract G2/G3) or plaintext metadata that is accepted as untrusted.
* A compromised NAS can lie about metadata or withhold clips. It cannot obtain the key or alter what the app does.

### 2.2 Topologies

**Topology A (recommended): split origins**

```
 Family browser ──https──► App origin   (relay host: zview-server --mode app)
        │
        └──https + CORS + Bearer──► Data origin (NAS: zview-server --mode data, storage driver=local)
```

* The NAS reads its own disk, so video bytes don't take an extra network hop.
* Via Tailscale:
  * `tailscale serve` exposes `https://mini.<tailnet>.ts.net` (app)…
  * …and `https://nas.<tailnet>.ts.net` (data).

**Topology B: single trusted host**

```
 Family browser ──https──► relay host: zview-server --mode both  ──SFTP/S3 (read-only creds)──► NAS
```

* Simpler: one origin, no CORS.
* Costs: every video byte is proxied through the relay host's uplink, and viewing becomes unavailable when the relay host is down.

### 2.3 Transport choices (Tailscale vs Cloudflare Tunnel)

* **Tailscale end to end:** the preferred path for the app origin. TLS terminates on your host.
* **Cloudflare Tunnel:** Cloudflare terminates TLS and can see and modify responses.
  * For the **data origin**, this is acceptable. Cloudflare sees ciphertext and already-accepted metadata; put it behind Cloudflare Access.
  * For the **app origin**, it adds **Cloudflare to the trusted base for code delivery** (Contract §2.6). Accept this only if family members won't install Tailscale.
* **Cloudflare Access with split origins:** the cross-origin `fetch` calls send preflight `OPTIONS` requests without cookies. Configure the Access application's CORS handling to pass `OPTIONS` through to the origin, and verify it during Phase 1.

### 2.4 App origin security headers (MUST, served by `--mode app`)

```
Content-Security-Policy:
  default-src 'none';
  script-src 'self';
  style-src 'self' 'unsafe-inline';
  img-src 'self' blob: data:;
  media-src 'self' blob:;
  worker-src 'self';
  connect-src 'self' https://<DATA_ORIGIN>;
  manifest-src 'self';
  base-uri 'none'; form-action 'none'; frame-ancestors 'none'
Cross-Origin-Opener-Policy: same-origin
Referrer-Policy: no-referrer
Permissions-Policy: camera=(self), microphone=(), geolocation=()
X-Content-Type-Options: nosniff
Strict-Transport-Security: max-age=31536000
Cache-Control: no-cache                       # index.html, sw.js
Cache-Control: public, max-age=31536000, immutable   # hashed assets
```

* `style-src 'unsafe-inline'` is tolerated because Svelte transitions inject style rules. Script injection remains blocked.
* Ship CSP in `Report-Only` mode first (Phase 4), then enforce it (Phase 5).

---

## 3. Data Server (`zview-server --mode data`)

### 3.1 Responsibilities

* Index sidecars from storage through `pkg/storage.Reader` (`local`, `sftp` or `s3`, read-only credentials).
* Serve the JSON API and ciphertext bytes with Range support.
* **Never** decrypt, transcode or demux; the server has no key.

### 3.2 Authentication

* **Bearer token:** every `/api/*` request except `/healthz` requires `Authorization: Bearer <base64url(K_api)>`, unpadded per RFC 4648 §5 (Contract §3.2).
* **Stored verifier:** the server holds only `api_token_sha256`, produced on a trusted machine by `zrelay key api-token-hash`. It **base64url-decodes the credential first**, rejects anything that is not exactly 32 bytes, and compares `SHA-256(raw_bytes)` in constant time. The hash preimage is the raw key material, never its text encoding.
* **What the token protects:** it keeps metadata away from other tailnet devices, the internet behind a tunnel, and anyone who lacks the key. Network-level controls (tailnet ACLs, Cloudflare Access) still apply in front of it.
* **Rotation:** rotating the master key rotates the token. During a transition the server MAY accept up to two hashes.
* **Failures:** 401 responses use a uniform body, and failures are rate-limited per client IP (e.g. 10/min).

### 3.3 CORS (Topology A)

| Setting | Value |
|---|---|
| `Access-Control-Allow-Origin` | Exact match against `allowed_app_origins`; no wildcard |
| `Access-Control-Allow-Headers` | `Authorization, Range` |
| `Access-Control-Expose-Headers` | `Content-Range, Content-Length, ETag, Accept-Ranges` |
| `Access-Control-Allow-Methods` | `GET, OPTIONS` |
| `Access-Control-Max-Age` | `600` |
| `Vary` | `Origin` |

### 3.4 API

Errors use `application/problem+json` (RFC 9457). All times are RFC 3339 UTC.

| Method & path | Description |
|---|---|
| `GET /healthz` | Liveness, no auth. |
| `GET /api/v1/status` | Pass-through of `status/relay.json`, plus index stats: `indexed_days`, `invalid_sidecars`, `index_fresh_at`. |
| `GET /api/v1/cameras` | Cameras from `relay.json` merged with cameras seen in indexed sidecars: `[{id, name, brand, tags, last_clip_at, online}]`. |
| `GET /api/v1/clips?from=&to=&camera=…&trigger=…` | Clip summaries **overlapping** `[from, to)`. `camera` and `trigger` are repeatable filters. Max range 8 days. Details below. |
| `GET /api/v1/clips/{clip_id}` | The full sidecar verbatim, plus `video_href`. |
| `GET /api/v1/clips/{clip_id}/video` | Ciphertext. Supports `Range` (206), `ETag` (from storage), `Accept-Ranges: bytes`. Details below. |
| `GET /api/v1/clips/{clip_id}/video-link` | Only for `s3` with `presign: true`. Returns `{url, expires_at}` pointing at a presigned GET. |
| `GET /api/v1/coverage?camera=&from=&to=` | Merged `ready` intervals and `dropped` entries from hourly coverage objects. Hours with no object count as not ready, except the current hour, which is derived from `relay.json`. |

**`GET /api/v1/clips` details**

* **Overlap rule:** `start < to && end > from`. An incomplete clip with a null end uses `start + index.assumed_max_clip_seconds` (default 300). The server cannot see the relay's `max_clip_seconds`, so this is a server-side configuration value rather than a hardcoded constant.
* **Summary contents:** omits `seek_index`; includes `video_href`.
* **Sort order:** by `start_time`.

**`GET /api/v1/clips/{clip_id}/video` details**

* `Content-Type: application/octet-stream` and `Cache-Control: private, max-age=86400, immutable`. Objects are immutable once committed.

**`GET /api/v1/clips/{clip_id}/video-link` details**

* The bucket needs CORS for the app origin.
* The link is returned as JSON rather than a redirect, because browsers drop `Authorization` on cross-origin redirects.

**Clip lookup**

1. The ULID timestamp in `clip_id` equals `start_unix_ms` (Contract §4.1), which gives the UTC day directly.
2. Load that day's index, then resolve the object key.
3. The video key is always `dirname(sidecar_key) + "/" + sidecar.video.object`.
4. Any sidecar whose `video.object` fails the Contract §7.2 regex is rejected.

No client-supplied path ever reaches storage.

### 3.5 Index

* **Structure:** `map[utcDay]*DayIndex` in memory. Each `DayIndex` holds validated sidecar summaries (~1 KiB per clip), sorted by `start_time` per camera.
* **Camera enumeration:** clips live under `clips/<camera_id>/…`, so indexing a day means listing one prefix per camera — but `storage.Reader` has no delimiter listing and S3 has no directories. `status/relay.json` is authoritative for the camera set (Contract §7.5.1), with `index.cameras_fallback` used only when it is missing or stale. The server MUST NOT attempt to enumerate cameras by listing all of `clips/`.
* **Binding:** the server binds `storage.ReadProvider`, not `Provider`, so a read-only deployment has no write methods available to it at all.
* **Hot days** (today and yesterday, UTC): refreshed by `List` on the day prefix at most every `hot_refresh_seconds` (default 30), triggered by requests rather than a timer.
* **Cold days:** loaded lazily on first request and kept in an LRU (`cold_cache_days`, default 60). They are invalidated only if retention deletes the day.
* **Validation:**
  * sidecar schema major must be `2`;
  * field constraints per Contract §6.2;
  * the sidecar's `clip_id` must match its filename;
  * the video object must exist in the same listing.
  * Invalid sidecars are counted and logged, never served.
* A local driver reading NAS disk MAY use fsnotify for faster refresh (optional).

### 3.6 Configuration

```yaml
version: 2
server:
  mode: "data"                          # app | data | both
  listen: "127.0.0.1:8080"              # expose via tailscale serve / cloudflared
  allowed_app_origins: ["https://mini.tailnet-name.ts.net"]
  api_token_sha256: ["FILE:/volume1/zview/api_token.sha256"]
  app_dir: ""                           # mode app|both: path to built SPA
  data_origin: ""                       # mode app: injected into CSP connect-src and app config
storage:
  driver: "local"                       # local | sftp | s3 (read-only credentials)
  root: "cctv"
  local: { base_dir: "/volume1" }       # + root "cctv" → /volume1/cctv/…
  s3: { presign: false, presign_ttl_seconds: 300 }
index:
  hot_refresh_seconds: 30
  cold_cache_days: 60
  assumed_max_clip_seconds: 300         # bounds an incomplete clip's end time (§3.4)
  cameras_fallback: []                  # used only when status/relay.json is missing or stale (§3.5)
```

`storage.root` is a key prefix and `local.base_dir` is the mount point; the mount MUST NOT repeat the root (Contract §7.1). With the values above, sidecars resolve under `/volume1/cctv/clips/…`, so `base_dir` here is `/volume1`, not `/volume1/cctv`.

In `app` mode, the server exposes `/config.json` with `{data_origin}`. The SPA reads it at startup, so one build serves any deployment.

---

## 4. Web App

### 4.1 Key onboarding & lifecycle

**Adding a key** (supported on every device)

1. **Paste or type** the `zk1-…` string. The checksum is validated as the user types, and a bad checksum shows "typo in key".
2. **Scan QR:** in-app scanner via `getUserMedia` plus a small QR decoder library, using `BarcodeDetector` where available. The scanner is built in because an installed iOS Home Screen app has storage separate from Safari. Keys MUST NOT be passed in URLs, including fragments.

**After import**

* The UI shows the fingerprint (`key_id`).
* The app fetches recent clips. If none carry this `key_id`, it warns "This key doesn't match recent recordings" and shows both fingerprints.

**Remember this device** (a toggle offered at import)

* **On:** the worker stores the **non-extractable** HKDF base `CryptoKey` in IndexedDB. Safari may evict site storage after long disuse; installed Home Screen apps are more durable.
* **Off:** keys live only in worker memory for the tab's lifetime.

**Keyring**

* Multiple keys may be stored, one per rotation. The header's `key_id` selects which one to use.

**Locking and forgetting**

* **Lock** discards derived keys and open clip DEKs from worker memory. Remembered base keys remain.
* **Forget key on this device** deletes the key from IndexedDB, then locks.
* Auto-lock after `idle_lock_minutes` (default off, configurable) when the tab is hidden.

### 4.2 Crypto & data worker

A single Dedicated Worker owns all key material **and all data-origin requests**. The main thread never holds the master key (after import), `K_api` or DEKs.

Message protocol (request/response over `postMessage`; plaintext buffers are **transferred**, not copied):

| Message | Result |
|---|---|
| `key.import {encoded, remember}` | `{keyId}` or `{error: "checksum" \| "format"}` |
| `key.list` / `key.forget {keyId}` / `key.lock` | keyring state |
| `api.get {path, query}` | Parsed and schema-validated JSON. Sidecar strings are length-capped. |
| `clip.open {clipId}` | See details below. |
| `clip.read {clipId, chunk}` | `{plaintext: ArrayBuffer (transferred), final}`, or a typed error |
| `clip.close {clipId}` | Aborts in-flight fetches and drops the DEK |

**`clip.open` details**

1. Fetch the sidecar and the header bytes (`Range: bytes=0-168` always covers it, since the maximum header is 105 + 64 bytes).
2. Validate the header per Contract §4.4 and unwrap the DEK with `unwrapKey`.
3. Run the G3 comparison against the sidecar.
4. Return `{header, verification, numChunks, codec, initSegmentBytes, seekIndex, status}`.

`numChunks` MUST be derived from the object's true length — the total in the `Content-Range` header of step 1 — and never from the sidecar's `video.size_bytes`, which is unauthenticated. For the same reason `chunk_size` comes from the descriptor, not from `video.chunk_size` (Contract §4.4).

**Fetching**

* Requests carry `Authorization`.
* Chunk ranges are computed per Contract §6.4.
* Concurrency is limited to 2 in-flight chunk fetches per clip.
* An `AbortController` cancels fetches on seek or close.

### 4.3 Playback engine

**Capability order**

1. `ManagedMediaSource` (Safari 17.1+ including iOS). Set `video.disableRemotePlayback = true`; it's required unless an AirPlay alternative source is provided.
2. `MediaSource`.
3. Blob fallback (§4.3.3).

Before choosing, `isTypeSupported('video/mp4; codecs="<sidecar.video.codec>"')` MUST pass. Otherwise show "This browser can't play <codec>" and offer export.

#### 4.3.1 Sequential play

1. `clip.open`, then decrypt chunks from 0 onward.
2. Append the plaintext byte stream to a single `SourceBuffer` in `segments` mode. Appends may split anywhere, because the parser is continuous.
3. Keep fetching while `buffered.end − currentTime < ahead_seconds`.
4. On `ManagedMediaSource`, honor the `startstreaming` / `endstreaming` events.

#### 4.3.2 Seek to media time *t*

1. If *t* is already buffered, set `currentTime`.
2. Otherwise:
   1. abort in-flight fetches;
   2. call `sourceBuffer.abort()` to reset the parser;
   3. find the fragment via `seek_index`;
   4. decrypt its chunk;
   5. append bytes from the intra-chunk offset;
   6. continue sequentially.
3. The init segment is appended once per `SourceBuffer` and re-appended only after a codec change, which never happens within a clip.

**Buffer window**

* Remove ranges outside `[currentTime − behind_seconds, currentTime + ahead_seconds]` after each append.
* Evict immediately on `QuotaExceededError`, then retry once.

| Profile | `ahead_seconds` | `behind_seconds` | Target peak plaintext in memory |
|---|---|---|---|
| Mobile (touch + narrow viewport, or `deviceMemory ≤ 4`) | 30 | 15 | ≤ 32 MiB at 3 Mbps |
| Desktop | 90 | 30 | ≤ 64 MiB at 3 Mbps |

**Auto-advance:** when a clip ends, continue to the next clip of the same camera (same `series_id` first) if "autoplay next" is on. A `max_duration` split that crosses UTC midnight puts consecutive segments of one series in adjacent day directories (Contract §7.1), so the lookup MUST search both the clip's day and the next.

#### 4.3.3 Blob fallback (no MSE)

* Allowed only if `plaintext_size_bytes ≤ blob_max` (mobile 48 MiB, desktop 256 MiB). Larger clips show "Too large to play on this browser" and offer export.
* Decrypt all chunks into an array of `Uint8Array` parts and build `new Blob(parts, {type: "video/mp4"})`, avoiding a concatenated copy.
* Keep an LRU of Blob URLs (mobile 1, desktop 3) and call `URL.revokeObjectURL` on eviction.

### 4.4 Integrity & status UX

| State | Trigger | UI |
|---|---|---|
| Verifying | `clip.open` in progress | Spinner on player |
| ✅ Verified | Header valid, G3 match, chunks authenticate as played | Subtle check badge; tooltip "Encrypted recording verified" |
| ⚠️ Metadata mismatch | G3 comparison fails | Warning banner: header values shown as authoritative (camera, start time); timeline marker flagged |
| ⛔ Tampered or corrupted | `ErrAuthHeader`, `ErrAuth(i)`, `ErrTrailingData` | Stop at last verified frame; "This recording failed verification at ~mm:ss"; no export |
| ◐ Incomplete | `status: incomplete` or `ErrTruncated` | Plays verified portion; "Recording was interrupted" |
| 🔑 Unknown key | `ErrUnknownKey` | "Recorded with key a1b2-…; add that key to play" |

### 4.5 Untrusted data handling

* All data-origin JSON is validated in the worker against schemas mirroring Contract §6–7. Unknown sidecar majors are rejected.
* Strings are rendered as text only. `{@html}` and `innerHTML` with data are forbidden, with a lint rule to enforce it.
* Numeric fields are bounds-checked (e.g. `seek_index` offsets < `plaintext_size_bytes`, monotonic). Clips that fail are shown as "invalid" and are not playable.

### 4.6 Timeline

**Rendering**

* Canvas 2D, with devicePixelRatio scaling and redraw on `requestAnimationFrame` only when state changes.

**Layout**

* One lane per selected camera (event-only, so lanes stay compact).
* A day ruler in the **viewer's local timezone** using `Intl.DateTimeFormat`, with an optional forced display zone (e.g. `Asia/Jakarta`) for relatives abroad.

**Data**

* Opening a day computes the local-day `[from, to)` instants.
* The app calls `/clips` and `/coverage` for that range, plus ±1 hour of padding.

**Markers**

| Element | Appearance |
|---|---|
| Clip span | Neutral bar |
| Trigger spans (`first_at_ms..last_at_ms`) | Person orange with glyph `P`, vehicle green `V`, pet purple `A`, motion blue `M`, other gray `•` |

Glyphs appear at zoom levels where they fit, so the display never relies on color alone.

**Other indicators**

* **Coverage gaps:** gray hatched regions labeled "not monitored" on hover.
* **Now line:** shown when viewing today.

**Interaction**

* Wheel/pinch zoom through presets 24 h → 6 h → 1 h → 10 min → 1 min, continuous between them.
* Drag and swipe to pan.
* Click or tap a clip to open it and seek to its first trigger offset. Click empty space to snap to the nearest clip within 24 px.
* Double-click to zoom in.

**Performance target:** 60 fps pan/zoom with 500 clips/day × 4 cameras on a mid-range phone.

### 4.7 Layout

* **Desktop:** header (camera filter, date picker, trigger filter, key/lock menu, relay status dot), player plus event list sidebar, timeline and transport controls.
* **Mobile:**
  * the player stays pinned at the top;
  * the timeline sits below with larger touch targets;
  * the event list is a bottom sheet;
  * the header collapses into a menu.
* **Event list:**
  * newest first and virtualized;
  * each row shows local time, camera, trigger glyphs, duration and an integrity badge once known.
* **Keyboard:**
  * `Space` play/pause, `←`/`→` ±10 s, `J`/`K` previous/next clip;
  * `[`/`]` zoom, `T` jump to now.
* **Relay status:**
  * a red dot appears when `relay.json.updated_at` is older than 3 minutes, or a camera is `down`;
  * the tooltip shows `since`.

### 4.8 Export (plaintext to device)

* A confirmation dialog MUST state: "This saves an **unencrypted** copy on this device."
* **Chromium:** `showSaveFilePicker` with a streaming write, chunk by chunk, so the full plaintext never sits in memory.
* **Elsewhere:** a Blob download within the `blob_max` cap. On iOS, prefer `navigator.share({files})`.
* Export is disabled for clips in the ⛔ state. Incomplete clips export the verified portion only, with `-incomplete` in the filename.
* The filename is `<camera_id>_<local YYYYMMDD-HHMMSS>.mp4`.

### 4.9 PWA & service worker

* The app is installable (manifest, icons) and aimed at family members' phones.
* The service worker precaches **only** the hashed app shell. It MUST NOT intercept requests to the data origin or `/api`, and MUST NOT use Cache Storage for any data.
* Updates show a "New version available — reload" prompt. Waiting is never skipped silently while a clip is playing.

---

## 5. Browser Support Baseline

| Browser | Playback path | Notes |
|---|---|---|
| Safari macOS 17+, iOS/iPadOS 17.1+ | `ManagedMediaSource` | H.264 and HEVC (`hvc1`). Verify memory on the oldest supported iPhone. |
| Chrome / Edge (current −2) | `MediaSource` | HEVC only where hardware decode is available. |
| Firefox (current −2) | `MediaSource` | Treat as H.264-only. |
| Older iOS (< 17.1) | Blob fallback | Size-capped; export for large clips. |

Web Crypto requirements (HKDF, AES-GCM, `unwrapKey`, non-extractable keys in IndexedDB) are met by all of the above over HTTPS.

---

## 6. Technology Stack

| Layer | Choice | Notes |
|---|---|---|
| Server | Go (current stable), `net/http` (1.22+ pattern routing) | Shares `pkg/storage`, `pkg/sidecar`, `pkg/zrec` (header parsing for validation tests only). |
| Index | In-memory maps + LRU | SQLite removed; unnecessary at household scale. |
| Frontend | Svelte 5 + Vite (static SPA; SvelteKit with `adapter-static` acceptable) | No SSR; app origin serves static files. |
| Styling | Tailwind CSS | Dark UI; build-time CSS files only. |
| Crypto | Web Crypto in a Dedicated Worker | HKDF, AES-GCM, `unwrapKey`. |
| Media | `ManagedMediaSource` / `MediaSource`, Blob fallback | |
| Schema validation | e.g. `valibot` or `zod` in the worker | Mirrors Contract §6–7. |
| QR | Small decoder library; `BarcodeDetector` where present | |
| Tests | Vitest (Node Web Crypto) for `web/src/lib/zrec`; Playwright (Chromium + WebKit) for e2e | Cross-language vectors from `testdata/zrec/v1/`. |

---

## 7. Phased Implementation Guide (for LLM code generation)

Each phase is done only when its acceptance criteria pass.

### Phase 0 — TypeScript `zrec` reader

* Key text parsing, HKDF derivations, header parsing, `unwrapKey`, chunk decryption with the exact error taxonomy, and seek arithmetic.
* **Acceptance:**
  * All `testdata/zrec/v1/` vectors pass in Vitest.
  * They also pass in Playwright under Chromium and WebKit.
  * Derived `key_id` and `SHA-256(K_api)` match the vectors.

### Phase 1 — Data server

* Storage reader wiring, day index, validation, the API in §3.4, auth, CORS and problem+json errors. Also `--mode app` with the security headers from §2.4 and `/config.json`.
* **Acceptance:**
  * Fixtures produced by zrelay Phase 2/4 index correctly.
  * Overlap queries across midnight UTC and WIB return the right clips.
  * Path-traversal and malformed-sidecar fixtures are rejected.
  * Range requests return correct 206 bytes.
  * Cross-origin fetch works from the app origin with Tailscale, and with Cloudflare Access if used.

### Phase 2 — Worker + playback engine (standalone demo page)

* **Acceptance:**
  * A real 300 s zrelay clip plays and seeks via MSE in Chrome and Safari, and via `ManagedMediaSource` on an iPhone.
  * Peak JS heap plus media buffer stays within the §4.3.1 targets, measured and recorded.
  * Tamper fixtures show the correct §4.4 states.
  * An incomplete (recovered) fixture plays its verified portion.

### Phase 3 — Timeline component

* **Acceptance:**
  * With a synthetic dataset of 4 cameras × 500 clips/day plus coverage gaps, 60 fps pan/zoom on a mid-range phone (profiled).
  * Local-day boundaries are correct for WIB and for a DST zone.
  * Click-to-seek lands on the trigger offset.

### Phase 4 — Dashboard assembly

* Onboarding (paste, QR, remember, fingerprint check), camera/date/trigger filters, event list, integrity badges, export, relay status, PWA, and CSP in Report-Only mode.
* **Acceptance:**
  * A Playwright e2e covers first run through key import, day view, playing a clip, seeking and exporting.
  * No CSP reports are produced during the e2e.
  * The main thread never receives the key (asserted by an instrumented test).

### Phase 5 — Hardening

* Enforce CSP, apply rate limits, run a Lighthouse PWA pass, and write documentation for family onboarding (a one-page guide with QR steps).
* **Acceptance:**
  * An enforced CSP passes the e2e.
  * The service worker is verified never to cache data-origin responses.

---

## 8. Future Work

* Sealed metadata mode (Contract §2.5), which moves indexing into the browser.
* Per-person keys with revocation (asymmetric recipients).
* Multi-camera synchronized grid playback.
* In-browser thumbnails generated after decryption (never uploaded).
* Push notifications for relay-down or person events (they would reveal metadata to the push service).
