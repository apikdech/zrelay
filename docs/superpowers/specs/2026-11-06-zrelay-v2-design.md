# zrelay v2: Rewrite Design

**Status:** Approved in brainstorming (2026-11-06); written spec pending owner review.
**Normative base:** `docs/specs/contract.md`, `docs/specs/relay-spec.md`, `docs/specs/ui-spec.md`. This document amends them. Where they conflict, this document wins (§11 lists every amendment).
**Normative language:** MUST / SHOULD / MAY per RFC 2119.

---

## 1. Intent

### 1.1 What the owner asked for

* Rewrite zrelay from its specifications into the new `zrelay-v2` repository. Everything is written from scratch. The v1 code (`../zrelay`) is not ported, and nothing in this design depends on it working.
* Production storage is **S3**. A `local` driver exists **for tests only**.
* zrelay **replaces iptv-seer** (`../iptv-seer`). The owner shuts iptv-seer down before development starts, and losing footage meanwhile is accepted. iptv-seer is a **reference** for camera behaviour that has been proven on the Tapo C220. It is not a dependency.
* Two iptv-seer features carry over: **live view in zview** and an **RTSP restream**.
* **Zero-disk is kept.** When storage is unreachable, the relay queues clips **in memory** behind a **circuit breaker with exponential backoff**. Clips are dropped only when the in-memory backlog is exhausted.
* One spec and one plan cover all three subsystems: the recorder, zview-server and the SPA.

### 1.2 Success criteria

1. Event clips from the C220 land in MinIO on the Synology as `zrec` ciphertext with valid sidecars. `zdecrypt` verifies them and they play in zview.
2. A NAS or Tailscale outage shorter than the backlog window loses no clips. A longer outage drops clips, and every drop is visible in coverage and metrics.
3. A `kill -9` mid-clip yields an `incomplete` clip after restart.
4. Family members open zview over Tailscale, import the `zk1-…` key, browse the timeline, play and seek clips, and watch any camera live.
5. VLC and IPTV players on the tailnet play `rtsp://…:8554/<camera>`.
6. The zrelay container runs with a read-only root filesystem and no volumes.

### 1.3 Assumptions (stated, not asked)

* The Contract's byte format, key derivation, sidecar schema and storage layout are unchanged. The only Contract change is editorial: references to `smb` and `sftp` are dropped (§11).
* One Mac mini runs both containers. MinIO runs on the Synology and is reachable over Tailscale.
* Cameras are Tapo (the C220 today). Other ONVIF/RTSP cameras remain supported through config rules.

---

## 2. Decisions

These are added to Contract §1. The numbering continues the Contract's.

| ID | Decision | Rationale |
|---|---|---|
| D11 | Storage drivers are `s3` (production) and `local` (tests only). `smb` and `sftp` are removed. | Owner choice. Production target is MinIO on the Synology. |
| D12 | Deployment is Docker (OrbStack) on the Mac mini: two containers, `zrelay` and `zview-server`. The macOS LaunchAgent and `KEYCHAIN:` secrets are removed. | Matches the owner's working iptv-seer setup. A container with a read-only root filesystem and no volumes enforces zero-disk mechanically. |
| D13 | Secrets use `ENV:` and `FILE:` references only. The master key defaults to `FILE:/run/secrets/zrelay_master` (a Docker secret). | `KEYCHAIN:` is unreachable from a Linux container. |
| D14 | Zero-disk stays strict. The egress backlog is memory-only. A circuit breaker with exponential backoff gates uploads. Clips are dropped when the backlog is exhausted. | Owner choice: an encrypted on-disk spool was offered and declined. `egress_backlog_mb` is the lever for longer outage tolerance. |
| D15 | Retention is a MinIO ILM (lifecycle) rule, not relay code. The relay's credentials cannot delete archive objects. | No code to maintain, and it runs while zrelay is down. A compromised relay host cannot wipe the archive through its own credentials. |
| D16 | zrelay serves **live view** (WebRTC, WHEP signaling) and an **RTSP restream**. Both carry plaintext from the trusted relay host straight to tailnet clients, and never touch storage. | Owner requirement (replaces iptv-seer). The relay host already sees plaintext (Contract §2.2), so this adds no new trusted party. |
| D17 | Live audio passes through as G.711 when the camera sends it. Clips stay video-only (D10 unchanged). | WebRTC supports PCMA/PCMU natively. MP4 in browsers does not. |
| D18 | zview is single-origin: `zview-server --mode both` serves the SPA, `/api/v1/*` and `/live/*` under one `tailscale serve` HTTPS host. CORS, `data_origin`, `/config.json` and presigned links are removed. | Both containers are on the trusted Mac mini, and MinIO is tailnet-only. Topology A (split origins) moves to future work. |
| D19 | The fMP4 muxer uses `bluenviron/mediacommon/v2` (`pkg/formats/fmp4`), not `Eyevinn/mp4ff`. | Same family as gortsplib v5, already proven in iptv-seer. |

### 2.1 Threat-model deltas (Contract §2)

* **New plaintext egress paths:** live WebRTC (DTLS-SRTP) and the RTSP restream. Both come from the trusted relay host. Live view requires the `K_api` bearer. The restream uses its own username and password, so it gives **weaker** access control than the key: anyone on the tailnet with the restream credentials can watch live. This is accepted. Tailnet ACLs are the outer boundary, as in iptv-seer.
* **Retention by ILM** keeps D5 (deletion out of scope). The relay's S3 policy allows `DeleteObject` only on staging keys (§6.5).
* **G5 is unchanged.** zrelay writes no files at all.

---

## 3. Architecture

```
 Tapo cameras ──RTSP(TCP) + ONVIF──► zrelay container (Mac mini, trusted, zero-disk)
                                      ├─ Camera core (×N): 1 RTSP session + 1 ONVIF subscription
                                      │    └─ Hub: in-process fan-out of access units to
                                      ├─ Live: WHEP / WebRTC (pion)        ┐ plaintext, tailnet only,
                                      ├─ Restream: RTSP server :8554      ┘ never stored
                                      ├─ Recorder: pre-roll ring → clip FSM → fMP4 → zrec writer
                                      ├─ Egress: memory backlog rings → circuit breaker → S3 multipart
                                      ├─ Status/coverage publisher
                                      └─ HTTP: /healthz /metrics (127.0.0.1), /live/* (compose network)
                                              │ S3 over Tailscale (write creds, no archive delete)
                                              ▼
                                      MinIO on Synology  (ILM expires clips/ and status/coverage/)
                                              ▲ read-only creds
 Browser ──https (tailscale serve)──► zview-server container (--mode both)
                                      ├─ SPA bundle (app origin, trusted)
                                      ├─ Data API: day index, sidecars, ciphertext Range, status, coverage
                                      └─ /live/* reverse proxy → zrelay WHEP signaling
 Browser ◄──ICE/UDP (fixed port, Tailscale IP only)── zrelay live media
 VLC/IPTV ──RTSP/TCP :8554 (Tailscale IP only)──► zrelay restream
```

### 3.1 Repository layout

```
cmd/zrelay/  cmd/zdecrypt/  cmd/zview-server/
pkg/zrec/          format writer/reader, key derivation, zk1 text encoding (Contract §3–4)
pkg/sidecar/       sidecar/status/coverage types, validation, layout (Contract §6–7)
pkg/storage/       interfaces, CleanKey, RangeReaderAt, storagetest/ (conformance suite)
pkg/storage/local/ test driver
pkg/storage/s3/    production driver (aws-sdk-go-v2, path-style for MinIO)
pkg/secretref/     ENV:/FILE: resolution, --env-file loading
internal/relay/
  config/ metrics/ camera/ events/ live/ restream/ ring/ clip/ mux/ egress/ status/
internal/zview/    index, API, auth, app serving, live proxy
web/               Svelte 5 + Vite SPA; web/src/lib/zrec (TS reader)
testdata/zrec/v1/  cross-language vectors (Contract §4.5)
deploy/            compose.yaml, Dockerfile.zrelay, Dockerfile.zview, deploy.sh, runbook.md
docs/specs/        normative specs; docs/superpowers/ design and plans
```

---

## 4. Camera core (`internal/relay/camera`, `internal/relay/events`)

### 4.1 RTSP session and Hub

* One gortsplib **v5** client per camera over **TCP** (camera Wi-Fi drops UDP; iptv-seer). H.264 is required. H.265 is accepted if the camera offers it.
* The session reads codec parameters from the SDP and from in-band parameter sets. A change bumps a **parameter-set generation** counter carried on each access unit.
* It reassembles RTP into **access units**: `{PTS, DTS, IDR, NALUs, ParamGen, Wall time}`. The wall clock comes from the RTCP sender report NTP mapping (`time_source: rtcp`), falling back to arrival time (`arrival`).
* G.711 audio RTP (8 kHz mono, as iptv-seer accepts) is always set up when the camera offers it. Live view and the restream share it. Audio never reaches the recorder.
* **Hub:** subscribers register with a bounded channel and a class (`recorder` or `viewer`).
  * The camera read loop never blocks. A full subscriber channel marks the subscriber **lagged**: its queue is cleared, and it resumes at the next IDR.
  * A lagged `recorder` subscriber increments `zrelay_recorder_lag_total{camera}`, and the open clip finalizes as `stream_lost`. Losing the middle of a GOP would corrupt the fragment.
  * Queue sizes are config (`recorder_queue_frames`, `viewer_queue_frames`) and counted in the memory budget (§8.1).
* Errors and logs never contain credentials (scrub URL userinfo and password, as iptv-seer's `redact` does).

### 4.2 Supervisor and camera states

* Per-camera states: `connecting → priming → ready → degraded → connecting`, as relay-spec §4.1. `ready` = at least one IDR buffered **and** the event subscription is active. Transitions feed coverage.
* Reconnect uses full-jitter exponential backoff from 1 s to 60 s, reset after 5 minutes of stability. One camera's failure never affects another.

### 4.3 ONVIF events

Replaces the Tapo-specific parts of relay-spec §4.4 with behaviour proven in iptv-seer (`internal/motion/onvif.go`):

* **Endpoint:** the default `events.port` is **2020** and the service URL is `http://<host>:<port>/onvif/service`. `GetCapabilities` discovery is used only when `events.service_path` is unset **and** the default fails.
* **Subscription:** `CreatePullPointSubscription` with `InitialTerminationTime=PT120S`. **Renew every 60 s.** A failed Renew is the health signal: `Unsubscribe` best-effort, then resubscribe with backoff (5 s to 60 s).
* **Pull:** `PullMessages(Timeout=PT1S, MessageLimit=20)` about every 1 s. Tapo drops the connection at once when nothing is queued, so a **transport error means "no events"**. It is counted in `zrelay_onvif_pull_retries_total{camera}` and is not a failure. A non-200 HTTP status **is** a failure.
* The **subscription address host is replaced** with the configured camera host, keeping its port and path. Requests carry credentials, so they go only to the configured host. HTTP redirects are refused.
* `Connection: close` is sent on every request. The client applies a 5 s request timeout and a 1 MiB response cap.
* **WS-UsernameToken digest** uses the camera clock offset from `GetSystemDateAndTime`, fetched on connect and every 30 minutes (relay-spec §4.4). iptv-seer used host time. v2 keeps the offset because it costs little and guards against camera skew.
* When a subscription ends for any reason, every trigger kind is forced inactive, so an outage never claims a never-ending event.

**Built-in Tapo rules** apply when `brand: tapo` and `rules` is omitted:

| Topic suffix | Item | Kind |
|---|---|---|
| `RuleEngine/CellMotionDetector/Motion` | `IsMotion` | motion |
| `RuleEngine/TPSmartEventDetector/TPSmartEvent` | `IsPeople` | person |
| `RuleEngine/TPSmartEventDetector/TPSmartEvent` | `IsPet` | pet |
| `RuleEngine/TPSmartEventDetector/TPSmartEvent` | `IsVehicle` | vehicle |
| `RuleEngine/TPSmartEventDetector/TPSmartEvent` | any other item, value `true` | other |

The `IsVehicle` and catch-all rows MUST be confirmed with `zrelay onvif discover` on the C220 during Phase 1. The table is updated from what the camera actually emits. Rule matching, `Initialized` handling, the trigger tracker (`trigger_timeout_seconds`) and `zrelay onvif discover` are otherwise as relay-spec §4.4.

---

## 5. Live view and restream

### 5.1 Live view (`internal/relay/live`)

* **WHEP** (RFC 9725) using pion/webrtc v4:
  * `POST /live/<camera_id>` with `application/sdp` returns `201` with the answer SDP and a `Location` for the session resource.
  * `DELETE` on that resource ends the session.
  * Trickle ICE is not used; the answer contains all candidates.
* **Media:** H.264 is forwarded as RTP without re-encoding. G.711 is forwarded when present and negotiated. A new viewer starts at the **next IDR** from the Hub. Startup latency is therefore up to one GOP, which is acceptable and avoids replaying a burst of old frames.
* **Auth:** `Authorization: Bearer base64url(K_api)`, verified exactly as Contract §3.2: decode, require 32 bytes, constant-time compare of `SHA-256` against `live.api_token_sha256` (a list of up to two hashes, for rotation).
* **Network:**
  * Signaling listens on `live.listen` (default `0.0.0.0:9465` inside the container). It is published only on the compose network, so zview-server reaches it as `http://zrelay:9465`.
  * ICE uses a single UDP mux port (`live.ice_udp_port`, default 8189) with the host's Tailscale IP as the only advertised candidate (`live.advertise_ip`). Compose publishes it on the Tailscale IP only (the iptv-seer pattern).
* **Limits:** `live.max_viewers` in total (default 4). A viewer whose queue lags is resynced at the next IDR, as in §4.1. ICE disconnect or 30 s without RTCP closes the session.

### 5.2 RTSP restream (`internal/relay/restream`)

* A gortsplib v5 server on `restream.listen` (default `:8554`) publishes `rtsp://<user>:<pass>@<host>:8554/<camera_id>` with the same video track, and audio when present.
* **TCP only.** RTSP-over-HTTP and RTSP-over-WebSocket tunnels are refused.
* Credentials: `restream.username` and `restream.password` (secret refs). Digest or basic auth, whichever gortsplib provides.
* When the camera session reconnects, restream clients are dropped and must reconnect (as iptv-seer).
* Compose publishes the port on the Tailscale IP only.

---

## 6. Recording pipeline and egress

### 6.1 Pre-roll ring, clip FSM, muxer, writer

* **Ring:** as relay-spec §4.3 (whole GOPs, retention rule, hard cap `ring_bytes`, pooled buffers).
* **Clip FSM:** exactly relay-spec §4.5, including the `max_duration` split with a shared `series_id`, `codec_change` (detected via `ParamGen`), the `stream_lost` abort when no fragment was ever written, `backlog_overflow → Suppressed`, `shutdown`, and trigger offsets clamped to `[0, duration_ms]`. It takes an injectable clock and is tested table-driven against a fake `Source`.
* **Muxer:** relay-spec §4.6, implemented with mediacommon `fmp4` (`ftyp+moov` init, one `moof+mdat` per GOP, video only, `tfdt` from 0, timescale 90 000, `avc1`/`hvc1`). It records `init_segment_bytes`, `seek_index` and `plaintext_size_bytes`.
* **zrec writer:** Contract §4.3. It emits into the clip's backlog ring (§6.3) and never talks to storage.

### 6.2 S3 driver (`pkg/storage/s3`)

* `aws-sdk-go-v2` with `endpoint`, `region`, `bucket`, `use_path_style: true` (MinIO), and access and secret keys as secret refs.
* **`Create` → multipart upload.** Parts are `part_size_mib` (default 8, minimum 5; the final part may be smaller).
  * `Write` buffers into the current part. A full part is uploaded synchronously inside `Write`, which may block (backpressure, relay-spec §4.9).
  * `DurableOffset` = the byte end of the last **completed** part.
  * `Resume` discards the in-flight part and returns the end of the last completed part. That value equals the last `DurableOffset`, which satisfies relay-spec §4.8.
  * `Commit` = upload the final part, then `CompleteMultipartUpload`.
  * `Abort` = `AbortMultipartUpload`.
* **`PutSmall`** = `PutObject` (atomic on S3). **`Delete`** = `DeleteObject`.
* **`List`** = `ListObjectsV2` with byte prefix, filtered to whole path elements, with dot-names omitted (relay-spec §4.9). **`OpenRange`** = ranged `GetObject` returning `Total` from `Content-Range` and the `ETag`.
* **Recoverer:**
  * `ListIncomplete` = `ListMultipartUploads` under the prefix, filtered by `Initiated` age.
  * `RecoverCommit` = `ListParts`, then `CompleteMultipartUpload` with the parts that exist. When **no** part exists the upload is aborted and `ErrNothingToRecover` is returned.
* **`HealthCheck`** = `HeadBucket`.
* `ETag` comes from S3 and changes when content changes.

### 6.3 Egress, backlog and circuit breaker (`internal/relay/egress`)

* **Upload streams and resume contract:** as relay-spec §4.8. Each clip has a seekable ciphertext ring that retains bytes until `DurableOffset` passes them, drained by one owning worker. At most `upload_workers` drain concurrently. Only the owner calls `Abort`.
* **Retries per request:** up to 3 attempts with full-jitter backoff (200 ms base, 2 s cap), classified as follows:
  * **Transient:** network errors, 5xx, 429, `RequestTimeout`, `SlowDown`.
  * **Permanent:** 403, `NoSuchBucket`, `InvalidAccessKeyId`, `SignatureDoesNotMatch`.
* **Circuit breaker** (one per storage endpoint, shared by every drain worker and by the status publisher):

| State | Behaviour | Transition |
|---|---|---|
| `closed` | Requests flow. | `breaker.failure_threshold` (default 5) consecutive transient failures, or **any** permanent failure → `open`. |
| `open` | Workers park. Bytes stay in their rings, so the backlog grows. Status writes are skipped. | After the current backoff delay → `half_open`. |
| `half_open` | Exactly one `HealthCheck` probe. Workers stay parked. | Success → `closed`, backoff reset, and workers call `Resume` and continue. Failure → `open`, delay doubled. |

  * Backoff is full jitter, starting at `breaker.base_delay` (default 1 s) and capped at `breaker.max_delay` (default 5 min).
  * A permanent failure is logged at error level once per transition. The breaker still probes, because a fixed credential or a recreated bucket should recover without a restart.
  * Exposed as `zrelay_storage_breaker_state` (0 closed, 1 half-open, 2 open), `zrelay_storage_breaker_transitions_total{to}`, `/healthz` (unhealthy while open longer than 5 minutes), and `storage` in `status/relay.json` (§6.6).
* **Backlog accountant:** as relay-spec §4.8 (evict the oldest finalized-but-uncommitted clip, then force-finalize the largest in-progress clip → `Suppressed`). Every drop is recorded in coverage as `backlog` and in `zrelay_clips_dropped_total{camera,reason}`.
* **Outage tolerance:** this is roughly `egress_backlog_mb ÷ (combined recording bitrate)`. Examples:
  * 512 MiB at 3.2 Mbps ≈ 22 minutes of one camera continuously recording.
  * 512 MiB at 9.6 Mbps ≈ 7 minutes of three.

  Event clips are bursty, so real tolerance is usually far longer. The runbook documents this calculation.

### 6.4 Commit protocol on S3

As Contract §7.3, concretely:

0. `PutObject .<HHMMSSmmm>_<clip_id>.json.part` (staging sidecar), before the first chunk enters the upload stream.
1. Stream the multipart upload. The object appears only on `CompleteMultipartUpload`.
2. `PutObject <name>.json` (the real sidecar).
3. `DeleteObject` the staging sidecar.

If the breaker is open at step 0, the staging write waits in the clip's ring like any other bytes. The staging sidecar is queued as the first item of the clip's upload stream, so ordering holds.

### 6.5 Crash recovery and S3 permissions

* **Startup recovery:** Contract §7.4's S3 path, on today's and yesterday's prefixes, for candidates older than 2 minutes:
  * Pair each multipart upload with its staging sidecar.
  * **Both present:** `RecoverCommit` → read and validate the descriptor → cross-check `clip_id`, `camera_id` and `start_unix_ms` → `PutObject` the real sidecar (`status: incomplete`, `end_reason: recovered`, nulls as specified, `video.size_bytes` from the committed object) → delete the staging sidecar.
  * **Upload with no staging sidecar:** abort the upload.
  * **Staging sidecar with no upload:** decide by Contract §7.4's second table (promote, or discard).
  * **Descriptor unreadable or mismatched after completion:** see "Unreadable recovered video" below.

  Recovery runs only when the breaker is closed. If storage is down at startup, it retries on every breaker close until it succeeds once.
* **Unreadable recovered video.** An in-progress multipart upload cannot be read, so the descriptor is checked only after `CompleteMultipartUpload`. If it is then unreadable or mismatched, the relay **leaves the committed video in place** and deletes only the staging sidecar. With no sidecar the video is invisible (Contract §7.3: visible iff the sidecar exists), and ILM expires it (§6.7). This is what lets the relay run without delete permission on archive keys.
* **Relay S3 policy** (MinIO policy JSON in the runbook):
  * `s3:PutObject`, `s3:GetObject`, `s3:ListBucket`, `s3:ListBucketMultipartUploads`, `s3:ListMultipartUploadParts` and `s3:AbortMultipartUpload` on the bucket under `<root>/`.
  * `s3:DeleteObject` **only** on dot-named staging objects, `<root>/clips/*/.*`. No delete on any other key (D15).
* **Read-only policy (zview-server):** `s3:GetObject` and `s3:ListBucket` only.

### 6.6 Status and coverage

As Contract §7.5 and relay-spec §4.10, via `PutSmall`. While the breaker is open, writes are skipped and coverage accumulates in memory; the next successful tick writes the current state. `status/relay.json` gains an additive field, permitted by Contract §6.3's ignore-unknown rule:

```json
"storage": { "state": "closed", "since": "2026-11-06T09:00:00Z", "backlog_bytes": 0 }
```

Its only consumer is zview's status display. A relay that cannot reach storage cannot publish this anyway, so viewers still rely on the 3-minute `updated_at` staleness rule.

### 6.7 Retention (MinIO ILM)

There is no retention code in zrelay. The runbook documents:

```
mc ilm rule add <alias>/<bucket> --prefix "<root>/clips/"           --expire-days <N>
mc ilm rule add <alias>/<bucket> --prefix "<root>/status/coverage/" --expire-days <N>
```

Bucket versioning stays **off**. With it on, expiry leaves noncurrent versions behind and storage is never reclaimed.

* A clip's video and sidecar expire independently, seconds apart. zview already treats a sidecar whose video is missing from the day listing as invalid (ui-spec §3.5) and does not serve it. A cached day index may briefly return such a clip, and the video request then answers `404`, which the SPA shows as "video unavailable".
* **Incomplete multipart uploads:** if the runbook adds an abort-incomplete-upload rule, it MUST be at least **7 days**. Recovery scans only today and yesterday, so a shorter rule could abort an upload before recovery sees it.
* zview-server's cold-day LRU is invalidated by a cheap re-list when a day falls past `index.retention_days` (config, matching N). Otherwise stale entries are harmless and age out.

---

## 7. zview-server and SPA

### 7.1 zview-server (`internal/zview`)

ui-spec §2.4 and §3, with these changes:

* **Single origin, `--mode both` only.** The `app` and `data` modes are removed (YAGNI) and return with Topology A if ever needed. CORS, `allowed_app_origins`, `data_origin` and `/config.json` are removed. The CSP uses `connect-src 'self'`.
* **Storage:** the `s3` driver bound as `storage.ReadProvider` with read-only credentials. `local` exists for tests. The `video-link` endpoint and presigning are removed.
* **API:** ui-spec §3.4 minus `video-link`. `/api/v1/status` includes the relay's `storage` object when present.
* **Index:** ui-spec §3.5 unchanged, plus the retention invalidation in §6.7.
* **Auth:** ui-spec §3.2 unchanged (Bearer, stored `SHA-256(K_api)`, up to two hashes, uniform 401, per-IP rate limit).
* **Live proxy:** `POST /live/<camera_id>` and `DELETE /live/<camera_id>/<session>` are checked against the same bearer verifier, then reverse-proxied to `live.upstream` (default `http://zrelay:9465`) with the `Authorization` header passed through. Bodies are capped at 64 KiB, and only `application/sdp` is accepted. `Location` headers are rewritten to the proxy's path.
* **Security headers:** ui-spec §2.4 with `connect-src 'self'`. Report-Only until Phase 13. `/healthz` needs no auth.

### 7.2 SPA (`web/`): Svelte 5 + Vite + Tailwind

ui-spec §4–5 unchanged, except:

* The data origin **is** the app origin. The worker calls relative `/api/v1/*` paths.
* **New worker message:** `live.offer {cameraId, sdp} → {sdp, sessionPath}` and `live.close {sessionPath}`. The worker performs the WHEP `POST`/`DELETE` with `K_api`. `RTCPeerConnection` runs on the main thread (it is unavailable in workers), so the main thread sees only SDP and media, never `K_api`.
* **New Live view:** a responsive grid of camera tiles, each a `<video muted playsinline>` attached to a WHEP session.
  * Sessions close when the tab is hidden (`visibilitychange`) and reopen when it is visible.
  * Each tile shows a live badge, a reconnecting state with backoff, a tap-to-unmute control (audio only when the track exists), and a "viewer limit reached" state on `503`.
  * The CSP needs no change for WebRTC.
* **Navigation:** two top-level views, **Timeline** (ui-spec §4.6–4.7) and **Live**.
* The service worker MUST NOT intercept `/api` or `/live`.

---

## 8. Configuration

### 8.1 `zrelay` (`relay-spec §5.2` amended)

```yaml
version: 2

relay:
  id: "home-mini"
  max_memory_mb: 768
  egress_backlog_mb: 512
  upload_workers: 2
  shutdown_grace_seconds: 30
  http_listen: "127.0.0.1:9464"       # /healthz /metrics

encryption:
  master_key: "FILE:/run/secrets/zrelay_master"
  chunk_size_kib: 1024

storage:
  driver: "s3"                        # s3 | local (tests)
  root: "zrelay"
  s3:
    endpoint: "ENV:S3_ENDPOINT"       # e.g. https://nas.<tailnet>.ts.net:9000
    region: "us-east-1"
    bucket: "cctv"
    use_path_style: true
    access_key: "ENV:S3_ACCESS_KEY"
    secret_key: "FILE:/run/secrets/s3_secret_key"
    part_size_mib: 8                  # >= 5
  local:
    base_dir: "/tmp/zrelay-test"
  breaker:
    failure_threshold: 5
    base_delay: "1s"
    max_delay: "5m"

live:
  listen: "0.0.0.0:9465"
  advertise_ip: "ENV:TAILSCALE_IP"
  ice_udp_port: 8189
  max_viewers: 4
  api_token_sha256: ["FILE:/run/secrets/api_token_sha256"]

restream:
  listen: "0.0.0.0:8554"
  username: "ENV:RESTREAM_USER"
  password: "ENV:RESTREAM_PASS"

defaults:
  pre_roll_seconds: 15
  post_roll_seconds: 10
  max_clip_seconds: 300
  max_bitrate_kbps: 4000
  max_gop_seconds: 4
  trigger_timeout_seconds: 60
  recorder_queue_frames: 256
  viewer_queue_frames: 64

cameras:
  - id: "tapo-c220"
    name: "Tapo C220"
    brand: "tapo"                     # tapo + no rules → built-in rules (§4.3)
    tags: ["interior"]
    host: "ENV:CAMERA_IP"
    credentials:
      username: "ENV:CAMERA_USERNAME"
      password: "ENV:CAMERA_PASSWORD"
    rtsp: { port: 554, path: "/stream1" }
    events: { source: "onvif", port: 2020 }
```

**Validation (relay-spec §5.3 amended):**
* `storage.driver` ∈ {`s3`, `local`}.
* `part_size_mib` ≥ 5.
* `s3.endpoint` is an absolute `http(s)` URL.
* `breaker.base_delay` ≤ `max_delay`.
* `live.max_viewers` ≥ 0 (0 disables live).
* Strict parsing rejects `smb`, `sftp`, `KEYCHAIN:` refs, `retention` and `mlock`.

The Contract §7.1 root-repeat check applies to `local.base_dir` only. On `s3` the anchor is the bucket, a namespace rather than a path, so `root` cannot repeat it and no check is made.

**Memory budget (relay-spec §3.2 amended):**

```
ring_bytes    = B × (pre_roll_seconds + G) × 1.25
active_bytes  = 2 × chunk_size + B × G + part_size_mib × MiB
queue_bytes   = (recorder_queue_frames + viewer_queue_frames × live.max_viewers) × B / fps_est   # fps_est = 15
camera_bytes  = ring_bytes + active_bytes + queue_bytes
live_bytes    = live.max_viewers × 2 MiB                                   # pion buffers, estimate
total         = Σ camera_bytes + live_bytes + egress_backlog_mb + 32 MiB
```

`config check` fails if `total > 0.8 × max_memory_mb`. The per-viewer queue term is a coarse estimate. Phase 2 measures real usage and adjusts the constant.

### 8.2 `zview-server` (`ui-spec §3.6` amended)

```yaml
version: 2
server:
  listen: "0.0.0.0:8080"              # behind tailscale serve
  api_token_sha256: ["FILE:/run/secrets/api_token_sha256"]
  app_dir: "/app"                     # built SPA, baked into the image
storage:
  driver: "s3"                        # s3 | local (tests)
  root: "zrelay"
  s3:
    endpoint: "ENV:S3_ENDPOINT"
    region: "us-east-1"
    bucket: "cctv"
    use_path_style: true
    access_key: "ENV:S3_RO_ACCESS_KEY"
    secret_key: "FILE:/run/secrets/s3_ro_secret_key"
index:
  hot_refresh_seconds: 30
  cold_cache_days: 60
  assumed_max_clip_seconds: 300
  retention_days: 30
  cameras_fallback: []
live:
  upstream: "http://zrelay:9465"
```

### 8.3 Deployment (`deploy/`)

* **`Dockerfile.zrelay`:** multi-stage `golang:1.27-alpine` → `scratch`, `CGO_ENABLED=0`, user `65534`.
* **`Dockerfile.zview`:** a node build stage (pnpm, Vite) → Go build → `scratch` with the SPA at `/app`.
* **`compose.yaml`:**
  * `zrelay`: `read_only: true`, **no volumes and no tmpfs**, `secrets:` mounted at `/run/secrets`, `env_file: .env`. Ports `${TAILSCALE_IP}:8554:8554/tcp` and `${TAILSCALE_IP}:8189:8189/udp`. 9465 is not published, and 9464 is published on `127.0.0.1` only.
  * `zview-server`: `read_only: true`, port `127.0.0.1:8080:8080` for `tailscale serve`.
  * `logging` uses the default driver. Logs go to stdout only and never contain secrets.
  * `ulimits: core: 0`.
* **`deploy.sh`:** build and `docker compose up -d`, as iptv-seer.
* **`runbook.md`:** MinIO users and policies (§6.5), the bucket, ILM rules (§6.7), `tailscale serve`, key generation and `api-token-hash`, the outage-tolerance calculation, OrbStack Local Network permission, and family onboarding (QR).

---

## 9. Testing strategy

Strict TDD (RED → GREEN → REFACTOR) for all production code.

| Layer | Tests |
|---|---|
| `pkg/zrec` | Vectors (Contract §4.5) generated and committed. Property tests over 1 B .. 5 × `chunk_size`. Every negative vector gives its exact error. Rewrap round-trip. |
| `pkg/sidecar` | Schema validation including `incomplete`. Layout regexes. Coverage invariants. |
| `pkg/storage` | `storagetest.Run(t, factory)` runs **unchanged** against `local` and against **MinIO in Docker** (testcontainers-go). It covers `List` visibility and element-wise prefix matching, `CleanKey`, `PutSmall` overwrite with `ETag` change, multipart `Resume` through a **fault-injecting TCP proxy** (committed object byte-identical), and the `Recoverer` paths. |
| `camera` | A gortsplib test server replays synthetic H.264 (fixtures generated by ffmpeg in Docker, small, committed). Covers access-unit assembly, `ParamGen` bumps, Hub lag/resync, and reconnect. |
| `events` | A fake ONVIF HTTP server: transport drops on pull, a failed Renew → resubscribe, SOAP faults, subscription address rewrite, redirect refusal, `Initialized`, the trigger timeout and Tapo default rules. |
| `clip` | Table-driven FSM with a fake clock and `Source` (relay-spec §9 Phase 3 list). |
| `egress` | Breaker transitions with an injected clock. Permanent vs transient classification. Accountant eviction order. Commit ordering (a sidecar never precedes its video). |
| `live` / `restream` | A WHEP offer/answer with a pion client in-process, receiving RTP from a synthetic Hub. Auth rejects bad tokens. A gortsplib client reads the restream, and tunnelled transports are refused. |
| Integration | **Chaos:** 3 synthetic 4 Mbps cameras with constant triggers, MinIO stopped for 10 minutes. RSS stays under `max_memory_mb`, the breaker opens then closes, drops appear in coverage, and the drain resumes. **Crash:** `kill -9` mid-clip produces an `incomplete` sidecar after restart. |
| `internal/zview` | Fixtures index correctly. Overlap queries across UTC/WIB midnight. Traversal and malformed sidecars rejected. Range returns 206. The live proxy enforces auth and rewrites `Location`. |
| `web` | Vitest: the TS zrec reader passes all vectors. Worker and schema unit tests. Playwright (Chromium + WebKit): vectors in-browser, then the e2e (onboarding → timeline → play → seek → export; Live tile connects to a test WHEP endpoint). The main thread never receives the key (instrumented). |
| Live checks | End of each relay phase against the real C220, plus a 72-hour soak in Phase 7. |

---

## 10. Phases

Ordered so the camera core and live view come first. They exercise the real C220 immediately, and later phases are tested against a working camera path.

| # | Phase | Done when |
|---|---|---|
| 0 | **Skeleton:** Go module, `pkg/secretref` (ENV/FILE, `--env-file`), `internal/relay/config` (strict YAML, validation, budget), `zrelay config check`, Dockerfiles, compose, `deploy.sh`, CI (`go test -race`, `go vet`, `staticcheck`) | `config check` prints the budget table (the key fingerprint line arrives in Phase 2); both images build; CI green |
| 1 | **Camera core:** RTSP session, access units, Hub, supervisor and states; ONVIF client, rules, trigger tracker; `zrelay onvif discover` | A 1-hour live session on the C220 with flat goroutines and RSS; `discover` output captured and the Tapo rule table confirmed |
| 2 | **Keys + live + restream:** `pkg/zrec` key half (zk1 text encoding, HKDF `key_id`/`K_wrap`/`K_api`, Contract §3), `zrelay key generate/fingerprint/api-token-hash`, the shared bearer verifier, WHEP (pion), ICE mux, the `:8554` restream | Key derivations match hand-computed HKDF values (vectors complete in Phase 3); VLC plays the restream; a bare HTML test page plays WHEP from a phone on the tailnet; per-viewer memory measured and the budget constant updated |
| 3 | **Contract packages:** `pkg/zrec` format half (writer, reader, rewrap, vectors), `pkg/sidecar`, `zdecrypt` (+ `rewrap`, exit codes) | All vectors (including the derived-key checks) and property tests pass; zdecrypt exit codes match relay-spec §6 |
| 4 | **Storage:** interfaces, `CleanKey`, `RangeReaderAt`, `local`, `s3` (multipart, resume, recoverer, health), conformance suite on MinIO, fault proxy | The suite passes unchanged on both drivers; the fault proxy test is byte-identical |
| 5 | **Recording pipeline:** ring, mediacommon fMP4 muxer, clip FSM, zrec writer into a per-clip ring, uploading synchronously to `local` (no breaker yet) | `zdecrypt \| ffprobe` (Docker) shows valid fMP4 with every fragment on an IDR; `seek_index` points at `moof`; pre-roll ≥ configured |
| 6 | **Egress:** drain workers, retries, circuit breaker, backlog accountant, S3 commit protocol, startup recovery, status/coverage publisher | Chaos and crash tests pass; real clips from the C220 appear in Synology MinIO and verify with `zdecrypt` |
| 7 | **Ops:** `/healthz`, `/metrics` (relay-spec §4.12 + breaker + recorder lag + live viewers), `runbook.md` (policies, ILM, tailscale serve) | Every metric present; a 72-hour soak with stable RSS and goroutines on a read-only container |
| 8 | **TS zrec reader** (`web/src/lib/zrec`) and SPA scaffold (Svelte 5, Vite, Tailwind, Vitest, Playwright) | Vectors pass in Vitest and in Playwright (Chromium + WebKit); derived `key_id` and `SHA-256(K_api)` match |
| 9 | **zview-server:** index, API, auth, rate limit, app serving, headers (Report-Only), live proxy | §9 `internal/zview` tests pass; Phase 6 clips are served from MinIO with correct 206 ranges |
| 10 | **Worker + playback engine** (demo page) | A real 300 s clip plays and seeks in Chrome, Safari and on an iPhone; tamper and incomplete fixtures show the ui-spec §4.4 states; memory targets measured |
| 11 | **Timeline component** | 60 fps pan/zoom with 4 cameras × 500 clips/day plus gaps on a mid-range phone; WIB and DST day boundaries correct; click-to-seek lands on the trigger offset |
| 12 | **Dashboard + Live view:** onboarding (paste, QR, remember, fingerprint check), filters, event list, integrity badges, export, relay and storage status, Live grid, PWA | The Playwright e2e passes; no CSP reports during the e2e; the key never reaches the main thread |
| 13 | **Hardening:** enforce CSP, finish rate limits, Lighthouse PWA pass, one-page family onboarding guide | e2e green with CSP enforced; the service worker never caches `/api` or `/live` |

---

## 11. Amendments to the normative specs

### `contract.md`

* §1: add D11–D19 (§2 above).
* §2.2: the relay host also serves live view and the restream (D16).
* §7.1: the driver anchors are `local.base_dir` and the S3 `bucket`. Drop the `smb` and `sftp` wording. The root-repeat rule applies to `local` only.
* §7.3 step 1 and §7.4: drop `smb` and `sftp`. `local` uses `.part` + rename, `s3` uses multipart.
* §7.4: an unreadable recovered S3 object is left in place without a sidecar and expired by ILM (§6.5 here).
* §7.5.1: `status/relay.json` MAY carry `storage {state, since, backlog_bytes}` (additive).

### `relay-spec.md`

* §0–§2: replace "SMB over Tailscale" with "S3 (MinIO) over Tailscale". Add live and restream to the architecture.
* §1.2: live view is **no longer** a non-goal (D16). `s3` is built. `smb` and `sftp` are removed. Retention is MinIO ILM (D15). `mlock` remains deferred.
* §3.1: the zero-disk exception for `local` is tests only. Docker `read_only: true` with no volumes is the production enforcement. Drop the LaunchAgent/systemd text.
* §3.2: the budget formula in §8.1 here (`part_buffer = part_size_mib`, plus queue and live terms). Update the worked example.
* §4.2: gortsplib **v5**.
* §4.4: the Tapo endpoint, pull cadence, Renew-as-health and built-in rules in §4.3 here.
* §4.6: the muxer library is mediacommon `fmp4` (D19).
* §4.8: add the circuit breaker and retry classification (§6.3 here).
* §4.9: the drivers are `local` (tests) and `s3`. Remove `smb` and `sftp` rows. The S3 driver semantics are in §6.2 here.
* §4.11: replaced by MinIO ILM (§6.7 here).
* §4.12: add `zrelay_storage_breaker_state`, `zrelay_storage_breaker_transitions_total{to}`, `zrelay_recorder_lag_total{camera}`, `zrelay_live_viewers{camera}` and `zrelay_restream_clients{camera}`.
* §5.1: drop `KEYCHAIN:`. The default master key is `FILE:/run/secrets/zrelay_master`.
* §5.2–§5.3: replaced by §8.1 here.
* §6: add `zrelay onvif discover` defaults (port 2020). The commands are otherwise unchanged.
* §7–§9: replaced by §3.1, §9 and §10 here.

### `ui-spec.md`

* §0, §2.1–§2.3: single origin, `--mode both` only (D18). Topology A moves to future work.
* §1.2: live view is **no longer** a non-goal. Multi-camera synchronized grid **playback** of clips remains a non-goal; the live grid is separate.
* §2.4: `connect-src 'self'`.
* §3.3: CORS removed.
* §3.4: the `video-link` endpoint is removed. `/api/v1/status` adds `storage`. Add the `/live/*` proxy (§7.1 here).
* §3.6: replaced by §8.2 here.
* §4.2: add the `live.offer` and `live.close` messages. All requests are same-origin.
* §4.7: add the Live view.
* §7: replaced by §10 here.

---

## 12. Future work

* Topology A (split app/data origins, CORS) if the data API ever moves off the Mac mini.
* An encrypted on-disk spool for long outages (declined for now, D14).
* Sealed metadata (sidecar schema 3), asymmetric recipients and per-person revocation.
* Linux `mlock`, other S3 targets (R2/AWS) beyond config, and `gcs`/`multi`.
* Offline alerts (webhook/ntfy).
* Opt-in audio transcode for clips.
