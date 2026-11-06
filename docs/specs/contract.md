# Shared Contract: Keys, `zrec` Clip Format, Sidecar & Storage Layout

**Status:** Draft v2.2 · **Applies to:** `zrelay` ≥ 2.0 (writer), `zview` ≥ 2.0 (reader)
**Normative language:** MUST / SHOULD / MAY per RFC 2119.
**Amendments (Phase 1a):** §7.2 and §7.3 step 2 — small atomic writes stage through `.tmp`, never `.part`, so the real sidecar cannot overwrite the step-0 staging sidecar. §7.4 — a staging sidecar with no `.part` video is no longer discarded unconditionally: if the committed video exists and the real sidecar does not, it is promoted.
**Amendments (Round 1 / v2.2):** §6.2 `pre_roll_ms` — it is `0` for continuation segments (`segment_index > 0`); the "≥ configured value" note applies to the first segment only. §7.1 — `smb` (`//host/share/base_dir`) is a driver anchor and takes part in the root-repeat rule. §7.3 step 1 and §7.4 — `smb` stages `.part` and renames like `local`, and its recovery follows the `local` path (read the descriptor from the `.part`, then rename).

## 0. Purpose

`zrelay` and `zview` depend on the same byte format, key derivation, metadata schema and storage layout. In v1 each spec defined these separately, and the copies drifted apart. The env var name, the sidecar struct versus the JSON, the trigger names and the auth-tag description all disagreed.

This document is now the **single source of truth**. The two component specs reference it and MUST NOT redefine anything in it.

---

## 1. Decisions Log

| ID | Decision | Rationale |
|---|---|---|
| D1 | Event clips only; no continuous recording. | Owner choice. |
| D2 | One shared symmetric master key for the household. | Family viewers share one key. Per-person revocation is not required. |
| D3 | Sidecar metadata stays **plaintext**. | Accepted: anyone with storage access can see when events happened, on which camera, which triggers fired, and clip sizes. Video *content* stays confidential. |
| D4 | In-scope adversaries: the storage operator or anyone who breaches the storage, and family members with NAS access who do not hold the key. | Owner threat model. |
| D5 | Out of scope: compromise of the relay host, and deletion or withholding of footage. | Owner threat model. |
| D6 | Chunked streaming AEAD replaces whole-file AES-GCM. | Memory stays bounded on both ends. Enables streaming upload, useful Range requests, MSE playback and truncation detection. |
| D7 | A random DEK per clip, wrapped with a key derived from the master key. | Makes counter nonces safe. Rotation can rewrap DEKs without re-encrypting video. The key is identifiable by `key_id`. |
| D8 | Web app code is **never** served by the storage host. | Otherwise a D4 adversary can serve modified JavaScript that captures the key (see zview §2). |
| D9 | All timestamps and storage paths use UTC. UIs display in local time. | 21:59 WIB is 14:59Z. Local-date folders break at midnight and DST. |
| D10 | No audio in v2. | Tapo cameras typically send G.711 over RTSP, which browsers will not play inside MP4. The no-transcoding rule stands. |

---

## 2. Threat Model

### 2.1 Assets

1. Recorded video content (primary).
2. The master key.
3. Event metadata. Integrity only; confidentiality is waived per D3.

### 2.2 Trusted components

* Cameras and the home LAN.
* The relay host running `zrelay`.
* Family viewer devices, and the browser on them.
* The **app origin**: the host that serves the zview web app bundle (zview §2).

### 2.3 Adversaries in scope

| Adversary | Capabilities assumed |
|---|---|
| NAS operator or anyone who breaches it | Read, modify, add, reorder and delete any stored object. Run or modify `zview-server` when it is hosted there. Observe upload timing and sizes. |
| Family member with NAS access, without the key | Browse and copy files through the NAS UI, SMB or SFTP. |
| Network observer on the transport path | Observe, drop and delay traffic. Tailscale and TLS prevent reading or modifying it. |

### 2.4 Guarantees

* **G1 — Video confidentiality.** No in-scope adversary can recover plaintext video without the master key.
* **G2 — Video integrity.** The viewer detects the following before any affected plaintext is played:
  * modification of any byte;
  * reordering, removal or duplication of chunks;
  * truncation or appended data;
  * substitution of one clip's key block into another clip.
* **G3 — Metadata binding.** The authenticated header carries `clip_id`, `camera_id` and `start_unix_ms`. The viewer checks them against the sidecar and surfaces any mismatch. Other sidecar fields (end time, triggers, tags) are **not** authenticated.
* **G4 — Code integrity.** Viewer code is delivered only from the trusted app origin.
* **G5 — No plaintext at rest.** `zrelay` never writes plaintext video to disk. The viewer holds plaintext in memory only, except for an explicit user export. This is best effort, because browsers may page Blob or media data to disk.

### 2.5 Accepted leakage (explicit)

Anyone with storage access can see:

* when each event occurred;
* camera IDs, names and tags;
* trigger types and their offsets within the clip;
* durations, resolution and codec;
* ciphertext sizes (roughly bitrate × duration);
* upload timing;
* relay and camera online/offline status.

These together reveal household activity patterns. This is accepted per D3. A future sealed-metadata mode is reserved as sidecar schema major 3.

### 2.6 Non-goals

* Any attacker controlling the relay host, which sees live plaintext frames.
* Deletion, withholding or rollback of clips by the storage operator.
* Traffic and size analysis.
* A malicious holder of the shared key.
* Malware on viewer devices.
* Forward secrecy.
* **Cloudflare**, if the app origin is exposed through Cloudflare Tunnel. Cloudflare terminates TLS and therefore joins the trusted base for code delivery (zview §2.3).

---

## 3. Keys

### 3.1 Master key

The master key is 32 bytes from a CSPRNG, generated by `zrelay key generate`.

**Text encoding.** The canonical form is:

```
zk1-<64 hex chars>-<4 hex chars checksum>
checksum = hex(SHA-256(UTF-8("zk1") || key_bytes)[0:2])
```

* Parsers MUST be case-insensitive and MUST strip whitespace.
* Parsers MUST reject input with a bad checksum, reporting it as a typo rather than a wrong key.
* QR codes MUST encode the **uppercase** form, so QR alphanumeric mode applies and the code stays small.

### 3.2 Derived keys (HKDF-SHA256)

All derivations use `HKDF(hash=SHA-256, secret=master_key, salt=UTF-8("zrec/v1"), info=<label>, L)`.

| Name | `info` label | Length | Use |
|---|---|---|---|
| `key_id` | `key-id` | 8 bytes | Public identifier stored in headers and sidecars, shown as a fingerprint (`a1b2-c3d4-e5f6-0718`). |
| `K_wrap` | `wrap` | 32 bytes | AES-256-GCM key that wraps per-clip DEKs. |
| `K_api` | `api-token` | 32 bytes | Bearer token for `zview-server`. The server stores only `SHA-256(K_api)`. |

Because HKDF behaves as a PRF, disclosing `key_id` or `K_api` reveals nothing about the master key or `K_wrap`.

**`K_api` transport and verification.** The hash preimage is the **raw 32 bytes**, never the text encoding:

* The bearer credential is `base64url(K_api)` per RFC 4648 §5, **unpadded** (no `=`).
* A server MUST decode the credential first, MUST reject anything that does not decode to exactly 32 bytes, and MUST then compare `SHA-256(raw_bytes)` against its stored verifier in constant time.
* `zrelay key api-token-hash` prints `hex(SHA-256(K_api))` over those same raw bytes.

Reference calls:

* Go ≥ 1.24: `hkdf.Key(sha256.New, master, []byte("zrec/v1"), "wrap", 32)`
* Web Crypto: `deriveBits` / `deriveKey` with `{name:"HKDF", hash:"SHA-256", salt, info}`

### 3.3 Per-clip data encryption key (DEK)

* Each clip gets 32 fresh CSPRNG bytes. A DEK MUST NOT be reused.
* The writer SHOULD zero DEK buffers after the clip is finalized (best effort in Go).

### 3.4 Rotation

1. Generate a new master key and point `zrelay` at it.
2. Viewers keep a **keyring** of old keys, selected by `key_id`.
3. Optionally, `zdecrypt rewrap --from <old> --to <new>` rewrites only the key block (§4.2). Chunk ciphertext is untouched, because chunk AAD covers only the descriptor.

Revoking one family member is not possible with a shared key (D2). The only remedy is rotation plus rewrap.

---

## 4. `zrec` Encrypted Clip Format (format_version 1)

All integers are big-endian. File extension: `.zrec`.

### 4.1 Layout

```
┌──────────────────────── Header (header_len bytes) ────────────────────────┐
│ Descriptor D (37 + n bytes)                     │ Key block K (68 bytes)  │
└───────────────────────────────────────────────────────────────────────────┘
┌──────── chunk 0 ────────┬──────── chunk 1 ────────┬───┬──── chunk N-1 ────┐
│ chunk_size + 16 bytes   │ chunk_size + 16 bytes   │ … │ 17..chunk_size+16 │
└─────────────────────────┴─────────────────────────┴───┴───────────────────┘
```

**Descriptor D**

| Offset | Size | Field | Rules |
|---|---|---|---|
| 0 | 4 | `magic` | ASCII `ZREC` |
| 4 | 1 | `format_version` | `0x01` |
| 5 | 1 | `suite` | `0x01` = AES-256-GCM chunks, DEK wrapped with HKDF-derived AES-256-GCM |
| 6 | 2 | `header_len` | u16, MUST equal `105 + n` |
| 8 | 4 | `chunk_size` | u32, power of two in [65 536, 16 777 216]; default 1 048 576 |
| 12 | 16 | `clip_id` | Binary ULID. Its 48-bit timestamp MUST equal `start_unix_ms`. |
| 28 | 8 | `start_unix_ms` | i64, wall-clock time of the first frame (UTC ms) |
| 36 | 1 | `camera_id_len` (n) | 1..64 |
| 37 | n | `camera_id` | UTF-8, matching `^[a-z0-9][a-z0-9-]{0,63}$` |

**Key block K** (starts at offset `37 + n`)

| Offset | Size | Field |
|---|---|---|
| +0 | 8 | `key_id` |
| +8 | 12 | `wrap_nonce` (CSPRNG) |
| +20 | 48 | `wrapped_dek` = 32-byte ciphertext + 16-byte GCM tag |

### 4.2 Cryptographic construction

```
H_D          = SHA-256(D)
wrapped_dek  = AES-256-GCM-Seal(K_wrap, wrap_nonce, DEK, AAD = H_D || key_id)
nonce_i      = uint88_be(i) || final_flag            // 11 + 1 = 12 bytes; final_flag ∈ {0x00, 0x01}
chunk_i      = AES-256-GCM-Seal(DEK, nonce_i, plaintext_i, AAD = H_D)
```

What this binding achieves:

* Chunk AAD covers the descriptor, so `clip_id`, `camera_id` and `start_unix_ms` are authenticated with every chunk (G3).
* Wrap AAD also covers the descriptor, so a key block copied from another clip fails to unwrap (G2).
* Rewrapping (§3.4) replaces K only, which leaves `H_D` and therefore all chunk ciphertext unchanged.

Browsers SHOULD use `crypto.subtle.unwrapKey("raw", wrapped_dek, K_wrap, {name:"AES-GCM", iv, additionalData}, "AES-GCM", false, ["decrypt"])` so that DEK bytes never enter the JavaScript heap.

### 4.3 Writer rules

1. `clip_id`, `start_unix_ms` and `camera_id` are **supplied by the caller**, because the object key (§7.1) is built from them before the writer exists. The writer generates the DEK and `wrap_nonce`, and MUST reject a `clip_id` whose 48-bit ULID timestamp does not equal `start_unix_ms`. It emits the full header before any chunk.
2. **Hold-back:** seal a non-final chunk only once *more than* `chunk_size` plaintext bytes are buffered. On `Close`, seal the remaining 1..`chunk_size` bytes with `final_flag = 0x01`.
3. Every non-final chunk holds exactly `chunk_size` plaintext bytes. This is what makes the §6.4 seek arithmetic (`floor(o / chunk_size)`) valid, so it is not merely an efficiency rule.
4. An empty plaintext is an error. A clip always contains at least the fMP4 init segment. A writer that reaches `Close` with no buffered plaintext MUST fail and MUST NOT commit the object.
5. Peak writer memory is ≤ 2 × `chunk_size` + 17 bytes: one hold-back byte beyond a full chunk, plus the 16-byte GCM tag on the sealed chunk. The 17 bytes are intrinsic to the hold-back rule (rule 2) and to the AEAD — they are not slack, and an implementation cannot be under this bound while still obeying rule 2.
6. The writer MUST accept an injectable randomness source (`io.Reader`) so test vectors are reproducible.
7. The writer MUST provide `Abort`, which zeroes the DEK, drops buffers and abandons the object without committing. Egress backpressure can evict a clip mid-stream (zrelay §4.8), so abandonment is a normal path, not only an error path.
8. The writer's sink MUST NOT be assumed to be durable storage. Writers emit into a bounded non-blocking buffer owned by the caller; see zrelay §4.7–4.8.

### 4.4 Reader rules

Definitions:

```
S = chunk_size + 16
body_len = file_len − header_len
N = ceil(body_len / S)
chunk i spans [header_len + i·S, min(header_len + (i+1)·S, file_len))
```

`file_len` MUST be the **actual** length of the stored object — the HTTP `Content-Range` total, or `Stat.Size`. Readers MUST NOT derive `N` from the sidecar's `video.size_bytes`, which is unauthenticated.

More generally, the **header is authoritative**. Where `chunk_size` or `key_id` appear in both the descriptor and the sidecar, readers MUST use the descriptor's values. The sidecar copies exist only so a viewer can select a key before fetching any bytes (§6.2).

Header validation MUST run first, in this order:

* `magic`, `format_version` and `suite` are supported (otherwise `ErrFormat`);
* `header_len` is consistent with `camera_id_len` (otherwise `ErrFormat`);
* `camera_id` is valid (otherwise `ErrFormat`);
* the 48-bit ULID timestamp of `clip_id` equals `start_unix_ms` (otherwise `ErrFormat`);
* `key_id` is present in the keyring (otherwise `ErrUnknownKey`);
* the DEK unwraps (otherwise `ErrAuthHeader`).

The keyring lookup precedes unwrapping, so a corrupted `key_id` reports `ErrUnknownKey` rather than `ErrAuthHeader`. §4.5 pins this ordering with a vector.

Chunks are then classified as follows:

| Position | Try | Outcome |
|---|---|---|
| `body_len == 0`, or last chunk shorter than 17 bytes | — | `ErrTruncated` |
| `i < N−1` | open with flag 0 | OK |
| `i < N−1`, flag 0 fails | open with flag 1 | Success means `ErrTrailingData`; failure means `ErrAuth(i)` |
| `i == N−1` | open with flag 1 | OK |
| `i == N−1`, flag 1 fails | open with flag 0 | Success means `ErrTruncated`; failure means `ErrAuth(i)`* |

\* When the caller sets the **allow-incomplete** option, an authentication failure on the **last** chunk MUST be reported as `ErrTruncated` instead. Recovered uploads can end mid-chunk.

Allow-incomplete is a caller-supplied boolean, not something the reader discovers: a `zrec` reader parses the clip format only and MUST NOT depend on the sidecar schema. Callers set it from `sidecar.status == "incomplete"`, or from `zdecrypt --allow-incomplete`.

Further requirements:

* Chunks MAY be decrypted in any order (random access for seeking).
* Plaintext of chunk *i* MUST NOT be released until its tag verifies. One-shot GCM per chunk enforces this naturally.

### 4.5 Test vectors (REQUIRED)

`testdata/zrec/v1/` MUST contain deterministic vectors generated by the Go writer. Both the Go reader and the TypeScript reader MUST pass them in CI.

* **Inputs:** fixed master key `00..1f`; fixed DEK, `wrap_nonce`, `clip_id`, `start_unix_ms` and `camera_id`; `chunk_size = 65536`.
* **Derived-value checks:** `key_id`, `K_wrap`, `K_api`, `SHA-256(K_api)`.

#### Manifest

`testdata/zrec/v1/vectors.json` is the machine-readable index both languages load:

```json
{
  "format_version": 1,
  "suite": 1,
  "master_key_hex": "000102…1f",
  "chunk_size": 65536,
  "camera_id": "front-door",
  "clip_id": "01M2NBMVS0K8EKRQKGGAJCDT0B",
  "start_unix_ms": 1789570740000,
  "derived": {
    "key_id_hex": "…", "k_wrap_hex": "…",
    "k_api_hex": "…", "k_api_sha256_hex": "…"
  },
  "positive": [
    { "name": "one-byte", "file": "pos-one-byte.zrec",
      "plaintext_len": 1, "plaintext_sha256_hex": "…" }
  ],
  "negative": [
    { "name": "bitflip-chunk", "file": "neg-bitflip-chunk.zrec",
      "expect": "ErrAuth", "chunk_index": 1 }
  ]
}
```

`expect` is one of `ErrFormat`, `ErrUnknownKey`, `ErrAuthHeader`, `ErrAuth`, `ErrTruncated`, `ErrTrailingData`. `chunk_index` is present only when `expect` is `ErrAuth`.

#### Positive cases

| Plaintext length | Why |
|---|---|
| 1 byte | Minimum legal clip; a single final chunk of 17 ciphertext bytes. |
| exactly `chunk_size` | The hold-back boundary. The writer MUST NOT seal at exactly `chunk_size`, so this yields `N == 1` with `final_flag = 1`, not two chunks. |
| exactly 2 × `chunk_size` | Whole-multiple case; `N == 2`, last chunk full-size and final. |
| 2.5 × `chunk_size` | Partial final chunk. |

#### Negative cases

| Vector | Expected error |
|---|---|
| bit flip in `camera_id` (inside D) | `ErrAuthHeader` |
| bit flip in `key_id` | `ErrUnknownKey` |
| bit flip in `wrapped_dek` | `ErrAuthHeader` |
| bit flip in chunk 1 of 3 | `ErrAuth(1)` |
| chunks 0 and 1 swapped | `ErrAuth(0)` |
| dropped final chunk | `ErrTruncated` |
| an extra whole chunk appended after the final chunk | `ErrTrailingData` |
| key block transplanted from another clip | `ErrAuthHeader` |
| wrong master key | `ErrUnknownKey` |
| 100 trailing bytes after the final chunk | `ErrAuth(N−1)` |
| header only, no chunks | `ErrTruncated` |

The last two are the non-obvious ones. Trailing bytes that do **not** form whole `S`-byte chunks fall inside the final chunk's computed span, so both flag attempts fail the tag check and the result is `ErrAuth`, not `ErrTrailingData`. `ErrTrailingData` is reachable only when the appended data lands on chunk boundaries.

---

## 5. Plaintext Content Profile (fMP4)

* The file is an init segment (`ftyp` + `moov`) followed by media fragments (`moof` + `mdat`).
* **Exactly one GOP per fragment.** Every fragment begins with a sync sample (IDR). `tfdt` starts at 0 for the clip. Video timescale is 90 000.
* **H.264:** `avc1` sample entry, parameter sets in `avcC`.
* **H.265:** `hvc1` sample entry with parameter sets in `hvcC` (Safari requires `hvc1`, not `hev1`).
* No audio track (D10).
* A change in SPS/PPS/VPS mid-stream MUST end the clip and start a new segment (§6.4, `end_reason: codec_change`).
* The RFC 6381 codec string (e.g. `avc1.640032`) MUST be recorded in the sidecar so viewers can call `MediaSource.isTypeSupported`.

---

## 6. Sidecar Metadata (`zrec.sidecar/2`)

### 6.1 Example

2026-09-16 21:59 WIB is 14:59Z. The IDs below are illustrative.

```json
{
  "schema": "zrec.sidecar/2",
  "status": "complete",
  "clip_id": "01M2NBMVS0K8EKRQKGGAJCDT0B",
  "series_id": "01M2NBMVS0K8EKRQKGGAJCDT0B",
  "segment_index": 0,
  "camera": {
    "id": "front-door",
    "name": "Front Door",
    "brand": "tapo",
    "tags": ["exterior", "entrance"]
  },
  "start_time": "2026-09-16T14:59:00.000Z",
  "end_time": "2026-09-16T14:59:42.500Z",
  "duration_ms": 42500,
  "pre_roll_ms": 15800,
  "end_reason": "post_roll_elapsed",
  "triggers": [
    { "type": "motion", "first_at_ms": 15800, "last_at_ms": 32400,
      "raw_topics": ["tns1:RuleEngine/CellMotionDetector/Motion"] },
    { "type": "person", "first_at_ms": 16900, "last_at_ms": 29100,
      "raw_topics": ["tns1:RuleEngine/PeopleDetector/People"] }
  ],
  "video": {
    "object": "145900000_01M2NBMVS0K8EKRQKGGAJCDT0B.zrec",
    "format_version": 1,
    "key_id": "a1b2c3d4e5f60718",
    "chunk_size": 1048576,
    "size_bytes": 16777371,
    "plaintext_size_bytes": 16777000,
    "container": "fmp4",
    "codec": "avc1.640032",
    "width": 2560,
    "height": 1440,
    "init_segment_bytes": 1187,
    "seek_index": [[0, 1187], [2000, 791344], [4000, 1580212]]
  },
  "recorder": { "relay_id": "home-mini", "version": "2.0.0", "time_source": "rtcp" }
}
```

### 6.2 Fields

| Field | Type | Req. | Notes |
|---|---|---|---|
| `schema` | string | ✔ | `zrec.sidecar/<major>`. Readers MUST reject unknown majors. |
| `status` | enum | ✔ | `complete` or `incomplete` (recovered after a crash, §7.4). |
| `clip_id` | ULID | ✔ | MUST equal the header value. |
| `series_id` / `segment_index` | ULID / int | ✔ | Segments split by `max_duration` or `codec_change` share a `series_id`. |
| `camera.id` | string | ✔ | MUST equal the header `camera_id`. |
| `camera.name`, `brand`, `tags` | string / string / string[] | ✔ | Untrusted display strings. Max 128 characters each; tags max 16. |
| `start_time` | RFC 3339 UTC, ms | ✔ | MUST equal the header `start_unix_ms`. |
| `end_time`, `duration_ms` | RFC 3339 / int | ✔ if complete | MAY be `null` when incomplete. |
| `pre_roll_ms` | int | ✔ | Actual pre-roll; ≥ configured value because of IDR alignment. This applies to the first segment only: it is `0` for continuation segments (`segment_index > 0`), which open with no pre-roll (zrelay §4.5). |
| `end_reason` | enum | ✔ | `post_roll_elapsed`, `max_duration`, `stream_lost`, `codec_change`, `backlog_overflow`, `shutdown`, `recovered` |
| `triggers[]` | array | ✔ | See the rows below. |
| `triggers[].type` | enum | ✔ | `motion`, `person`, `vehicle`, `pet`, `other` |
| `triggers[].first_at_ms`, `last_at_ms` | int | ✔ | Offsets from clip start. |
| `triggers[].raw_topics` | string[] | — | Vendor topics that matched; for diagnostics only. |
| `video.object` | string | ✔ | Bare filename, a **sibling** of the sidecar. MUST match §7.2. Never a path. |
| `video.key_id` | hex(16) | ✔ | Lets viewers pick a key before fetching the header. A **hint only** — the descriptor is authoritative (§4.4). The same applies to `video.chunk_size`. |
| `video.size_bytes` | int | ✔ | Ciphertext file size. |
| `video.plaintext_size_bytes` | int | ✔ if complete | |
| `video.codec` | string | ✔ | RFC 6381. |
| `video.init_segment_bytes` | int | ✔ | Plaintext length of `ftyp` + `moov`. |
| `video.seek_index` | `[[t_ms, plaintext_offset], …]` | ✔ if complete | One entry per fragment. `t_ms` is the fragment's media time; the offset points at its `moof`. |
| `recorder.time_source` | enum | ✔ | `rtcp` (RTCP sender report NTP mapping) or `arrival` (host receive time). |

### 6.3 Compatibility rules

* Readers MUST ignore unknown fields.
* Writers MUST NOT remove or retype a required field within a major version.
* Any breaking change bumps the major version (e.g. sealed metadata becomes `zrec.sidecar/3`).
* A field marked ✔ is required **including for `status: "incomplete"`**. Crash recovery satisfies this from the staging sidecar (§7.3), so consumers never need a separate validation path for recovered clips.

### 6.4 Seeking arithmetic

To seek to media time *t*:

1. Pick the last `seek_index` entry with `t_ms ≤ t`, giving plaintext offset *o*.
2. The chunk containing *o* is `floor(o / chunk_size)`, starting at intra-chunk offset `o mod chunk_size`.
3. The ciphertext byte range for chunks *a..b* is `[header_len + a·S, header_len + (b+1)·S)`.

---

## 7. Storage Layout

### 7.1 Tree

```
<root>/
  clips/<camera_id>/<YYYY>/<MM>/<DD>/<HHMMSSmmm>_<clip_id>.zrec
  clips/<camera_id>/<YYYY>/<MM>/<DD>/<HHMMSSmmm>_<clip_id>.json
  status/relay.json
  status/coverage/<camera_id>/<YYYY>/<MM>/<DD>/<HH>.json
```

* All date components and `HHMMSSmmm` are **UTC**, derived from `start_time`.
* The `mmm` suffix plus the ULID makes collisions impossible.

**Key space vs. mount point.** Every path above is an object **key**, relative to the storage root. The effective location is `<mount> / <root> / <key>`, where `<mount>` is the driver's anchor (`local.base_dir`; `smb`: `//host/share/base_dir`; `sftp.base_dir` or the S3 `bucket` once those drivers exist) and `<root>` is the `storage.root` key prefix, applied identically by every driver.

`<mount>` MUST NOT already include `<root>`. Configuring `base_dir: "/volume1/cctv"` together with `root: "cctv"` resolves to `/volume1/cctv/cctv/…` and is a configuration error. For `smb` the trailing element of the anchor is `base_dir`'s last element, or the `share` name when `base_dir` is empty. The uniform rule is what lets one `root` move a deployment between `local`, `smb` and (once added) `sftp` or `s3` unchanged, and gives `storage.CleanKey` a single place to enforce its rules.

### 7.2 Naming

* Final objects MUST match `^[0-9]{9}_[0-9A-HJKMNP-TV-Z]{26}\.(zrec|json)$`.
* Staging objects are named `.<final-name>.part`.
* Atomic small-object writes (sidecars, status and coverage objects) that a driver stages through a temporary use `.<final-name>.tmp`, never `.part`. A `.tmp` is hidden by the rule below, is never a recovery candidate (§7.4), and is not reaped; a crash mid-write leaves only a tiny orphan. The staging sidecar `.<name>.json.part` (§7.3 step 0) is an ordinary object written with the same atomic small write and MUST NOT share a name with any driver temporary, which is why `.tmp` and `.part` are distinct.
* Readers and indexers MUST ignore any name beginning with `.`.

**Visibility split.** Listing hides staging objects; explicit access does not:

* `List` MUST omit any entry whose **base name** begins with `.`.
* `Stat`, `OpenRange` and `Delete` MUST NOT filter, because crash recovery (§7.4) reads and promotes staging objects by explicit key.
* Key sanitisation MUST permit a leading `.` while still rejecting `..`, absolute paths, backslashes and control characters.

### 7.3 Commit protocol

A clip is **visible if and only if its sidecar exists.** The steps are:

0. **Staging sidecar.** As soon as the muxer has produced the init segment — and *before* the first `zrec` chunk is handed to the upload stream — write `.<HHMMSSmmm>_<clip_id>.json.part` containing every field already known: `clip_id`, `series_id`, `segment_index`, `camera`, `start_time`, `pre_roll_ms`, and `video.{object, format_version, key_id, chunk_size, container, codec, width, height, init_segment_bytes}`. It is a dot-name, so §7.2 keeps it invisible to readers and indexers.
1. Stream the `.zrec`. Local, SMB and SFTP drivers write `.<name>.part` and then rename it (using POSIX rename where the server supports it). The S3 driver uses multipart upload; the object appears only on `CompleteMultipartUpload`.
2. Once the video is committed, write the real sidecar atomically (`.tmp` + rename, or `PutObject`). The temporary is deliberately not `.part`: the driver's `.part` name for `<name>.json` is exactly the step-0 staging sidecar, and writing through it would truncate the only copy of the fields a crash cannot otherwise reconstruct.
3. Delete the staging sidecar.
4. Deletion (retention) runs in the reverse order: sidecar first, then video.

**Why staging exists.** Codec, resolution and `init_segment_bytes` live inside the encrypted fMP4, and `series_id`, `segment_index` and `pre_roll_ms` live in relay process state. A crash loses all six, yet §6.2 marks them required. Staging is what lets recovery produce a sidecar valid under the *same* schema as a normal clip, so no consumer needs a "recovered clip" branch.

A crash between steps 2 and 3 leaves an orphan staging sidecar. §7.2 already makes it invisible, and the next recovery sweep reaps it.

**Ordering invariant.** Step 0 strictly precedes the first chunk. Therefore: if a staging sidecar exists, the init segment was built; if it does not, the `.part` video holds at most a `zrec` header and no chunks, which is an empty plaintext and an error under §4.3.4. §7.4 relies on this being total.

### 7.4 Crash recovery (performed by `zrelay` on startup)

**Candidates:** under today's and yesterday's prefixes, `.part` files and S3 multipart uploads that are older than 2 minutes.

**Triage.** Pair each candidate video with its staging sidecar (§7.3 step 0). The pairing is total:

| Staging sidecar | `.part` video | Action |
|---|---|---|
| present | present | Recover (below). |
| absent | present | **Discard the video.** By the §7.3 ordering invariant it holds at most a header and no chunks, so there is nothing playable to recover. |
| present | absent | Decide by what survives, below. **Do not discard on this row alone.** |

**Staging sidecar present, `.part` video absent.** The `.part` is gone because it was either committed or discarded, and the surviving final objects say which:

| Real sidecar | Committed video | Meaning | Action |
|---|---|---|---|
| present | (any) | Crash between §7.3 steps 2 and 3: the clip completed. | Discard the staging sidecar. |
| absent | present | Crash between §7.3 steps 1 and 2: the video is complete but invisible, and the staging sidecar holds the only copy of the fields a crash cannot reconstruct. | **Promote** the staging sidecar by the sidecar rules below (read the committed video's descriptor first; `video.size_bytes` from the committed object), then delete it. Discarding here would orphan a good video. |
| absent | absent | The video was discarded and the discard was interrupted. | Discard the staging sidecar. |

**A `.part` with no readable descriptor.** The staging sidecar exists, so §7.3 step 0 ran, but a crash before the first `write(2)` reached the file (or a fault truncated it) can leave the `.part` empty or shorter than a descriptor. **Discard the pair**: never publish a recovered clip whose descriptor could not be read and validated, because the §7.4 cross-check (clip_id, camera_id, start_unix_ms) is what proves the two files pair, and §4.3.4 makes a header-only or empty plaintext an error. `RecoverCommit` cannot tell this case from a valid one — it commits what it is given — so the caller must attempt the descriptor read first wherever the driver stages.

**Recovery procedure**, ordered by driver because an in-progress S3 multipart upload cannot be read:

* `local`, `smb`, `sftp`: read the plaintext descriptor from the `.part`, then commit it (rename).
* `s3`: `ListParts`, then `CompleteMultipartUpload` with the parts already uploaded, then read the descriptor from the committed object.

Then promote the staging sidecar to the real sidecar, adding:

* `status: "incomplete"` and `end_reason: "recovered"`;
* `end_time`, `duration_ms`, `plaintext_size_bytes` and `seek_index` set to `null`;
* `triggers` set to `[]`;
* `video.size_bytes` from the committed object's size.

Finally, delete the staging sidecar.

Cross-check the descriptor's `clip_id`, `camera_id` and `start_unix_ms` against the staging sidecar and reject the pair on mismatch; the staging sidecar is unauthenticated, so the descriptor wins (§4.4).

**Accepted loss on S3.** The un-uploaded tail part is gone, so up to `part_size_mib` of ciphertext is lost. That will usually cut mid-chunk, which readers report as `ErrTruncated` — the correct outcome, not a defect.

The relay cannot finalize the stream, because the DEK is lost with the process. Viewers play the recovered chunks and label the clip incomplete.

### 7.5 Status objects

**`status/relay.json`** is overwritten every 60 s:

```json
{
  "schema": "zrec.status/1",
  "relay_id": "home-mini",
  "version": "2.0.0",
  "updated_at": "2026-09-16T15:00:00Z",
  "key_id": "a1b2c3d4e5f60718",
  "cameras": [
    { "id": "front-door", "name": "Front Door", "rtsp": "up", "events": "up",
      "since": "2026-09-16T09:12:04Z", "last_clip_at": "2026-09-16T14:59:00Z" }
  ]
}
```

**`status/coverage/<camera>/<YYYY>/<MM>/<DD>/<HH>.json`** is written at the end of each UTC hour, and a partial file is written on shutdown:

```json
{
  "schema": "zrec.coverage/1",
  "camera_id": "front-door",
  "hour_start": "2026-09-16T14:00:00Z",
  "ready": [["2026-09-16T14:00:00Z", "2026-09-16T14:37:10Z"],
            ["2026-09-16T14:38:02Z", "2026-09-16T15:00:00Z"]],
  "dropped": [{ "at": "2026-09-16T14:37:10Z", "reason": "stream_lost", "approx_ms": 52000 }]
}
```

* `ready` lists the intervals when an event would have been captured: the RTSP buffer was primed **and** the event subscription was active.
* Viewers shade the complement as *not monitored*. This is what distinguishes "nothing happened" from "the recorder was down".

#### 7.5.1 `zrec.status/1` fields

| Field | Type | Req. | Notes |
|---|---|---|---|
| `schema` | string | ✔ | `zrec.status/<major>`. Readers MUST reject unknown majors. |
| `relay_id` | string | ✔ | Matches `relay.id`. |
| `version` | string | ✔ | Relay build version. |
| `updated_at` | RFC 3339 UTC | ✔ | Rewritten every 60 s. Viewers treat the relay as down when this is older than 3 minutes. |
| `key_id` | hex(16) | ✔ | The key the relay is currently writing with. |
| `cameras[]` | array | ✔ | One entry per configured camera. **Authoritative for camera enumeration** (zview §3.5); no directory listing can enumerate `clips/` on every driver. |
| `cameras[].id` | string | ✔ | Same pattern as the header `camera_id` (§4.1). |
| `cameras[].name` | string | ✔ | Untrusted display string, max 128 characters. |
| `cameras[].rtsp` | enum | ✔ | `up` \| `down` \| `degraded` |
| `cameras[].events` | enum | ✔ | `up` \| `down` \| `degraded` |
| `cameras[].since` | RFC 3339 UTC | ✔ | When the current state was entered. |
| `cameras[].last_clip_at` | RFC 3339 UTC | — | `null` when the camera has produced no clip. |

`degraded` corresponds to the zrelay §4.1 camera state of the same name: the transport is connected but not usable for recording — for example RTSP is up but no IDR has arrived yet.

#### 7.5.2 `zrec.coverage/1` fields

| Field | Type | Req. | Notes |
|---|---|---|---|
| `schema` | string | ✔ | `zrec.coverage/<major>`. |
| `camera_id` | string | ✔ | Same pattern as the header `camera_id` (§4.1). |
| `hour_start` | RFC 3339 UTC | ✔ | Exactly on the hour. |
| `ready[]` | `[start, end]` pairs | ✔ | RFC 3339 UTC. Non-overlapping, ascending, clamped to the hour. MAY be empty. |
| `dropped[]` | array | ✔ | MAY be empty. |
| `dropped[].at` | RFC 3339 UTC | ✔ | |
| `dropped[].reason` | enum | ✔ | `stream_lost` \| `backlog` \| `shutdown` |
| `dropped[].approx_ms` | int | ✔ | Estimated footage lost. |

An hour with no object counts as **not ready**, except the current hour, which viewers derive from `status/relay.json` (zview §3.4).
