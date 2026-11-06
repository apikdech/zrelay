# Technical Specification: Zero-Disk Encrypted NVR Relay (`zrelay`) — v2

**Status:** Draft v2.2 · **Depends on:** `contract.md` (the *Contract*). All formats, keys, the sidecar schema and the storage layout are defined there and are only referenced here.
**Amendments (Round 1 / v2.2):** §1.2, §3.1, §4.11, §10 — `sftp`/`s3` drivers, retention and Linux `mlock` are deferred this round (design: `docs/superpowers/specs/2026-10-15-zrelay-recorder-design.md` §2, §6.5). §3.2, §4.8, §4.9 — an `smb` driver (go-smb2, no OS mount) replaces `sftp` this round; `part_buffer` is 0 for it, and `Commit` carries the SMB durability caveat. §4.4 — Tapo-aware PullPoint behaviour; any active event starts a clip. §4.12 — adds `zrelay_onvif_pull_retries_total`. §5.1–§5.3 — `--env-file`, `ENV:` refs for camera `host`/`events.port`, `smb` config schema and root rule, `part_size_mib` check dropped, legacy `retention`/`mlock` keys rejected. §6 — `--env-file`. §7–§9 — stack, layout and phases reflect the Round 1 stages.

## 0. Changes from v1

* **Encryption format.** Whole-file AES-GCM with a raw key is replaced by chunked streaming AEAD with a per-clip DEK (Contract §3–4). This fixes the memory-ceiling violation: a 300 s clip previously needed about 226 MiB for a single `Seal`.
* **Streaming uploads.** Clips upload *while recording*, through resumable writers with atomic commit and crash recovery. The old queue of whole clips is gone.
* **Storage interface.** It is now split into streaming `Writer` and `Reader` interfaces shared with `zview-server`. A `local` driver is added. `gcs` and `multi` are deferred.
* **Event rules.** Rules match the topic **and** the data item. Handling is added for `Initialized` messages, cameras that never send "inactive", subscription renewal and camera clock skew. There is also a new `onvif discover` tool.
* **Clip lifecycle.** An explicit state machine covers split, stream-loss, codec-change, backlog-overflow and shutdown.
* **Resources.** A per-camera memory budget formula is enforced through `GOMEMLIMIT`, with a byte-accounted backlog and a defined overflow policy.
* **Health data.** Heartbeat and hourly coverage objects let viewers distinguish "no events" from "recorder down".
* **Time and naming.** UTC everywhere, ms + ULID filenames, audio dropped (Contract D9, D10).
* **Secrets.** Config secrets now use `ENV:` / `FILE:` / `KEYCHAIN:` references. SFTP host-key verification is added (the SFTP driver itself is deferred in v2.2, §4.9).
* **Tooling.** New `zrelay key …` commands; `zdecrypt` no longer takes the key as a literal flag.
* **Documentation fixes.** The GCM tag is correctly described (GHASH, not Poly1305). The sidecar struct now matches the JSON. Performance numbers are restated as benchmark targets.

---

## 1. Scope

### 1.1 Goals

* A headless Go daemon for macOS (Apple Silicon) and Linux.
* Works with any camera that provides H.264/H.265 over RTSP and events over ONVIF Profile S/T.
* Records **event clips only** (Contract D1), with pre-roll and post-roll.
* Encrypts on the relay host and ships to a remote store the owner does not trust for confidentiality (Contract §2).
* No plaintext video on the relay's disks, and bounded memory.

### 1.2 Non-goals (v2)

* Continuous recording.
* Audio.
* Live view.
* Transcoding.
* Multiple relays writing to the same root.
* Defending against compromise of the relay host (Contract D5).
* GCS and multi-backend fan-out.
* Deferred this round (Round 1 design §2): the `sftp` and `s3` storage drivers, retention / auto-delete (§4.11) and Linux `mlock` (§3.1). The storage interface (§4.9) is unchanged, so each can be added later as a new driver or option without recorder changes.

---

## 2. Architecture

```
 IP cameras (RTSP :554 · ONVIF events on vendor port)
     │ LAN
     ▼
 zrelay (relay host — trusted)
 ├─ Supervisor ── spawns one pipeline per camera, jittered backoff 1s→60s
 │
 ├─ Camera pipeline (×N)
 │   ├─ RTSP client ──► Pre-roll ring (byte-bounded, IDR-aligned GOPs)
 │   ├─ EventSource (ONVIF PullPoint) ──► Rule matcher ──► Trigger tracker
 │   └─ Clip state machine ── open / split / finalize
 │         └─► fMP4 muxer (1 fragment per GOP, seek index)
 │               └─► zrec writer (chunked AEAD, hold-back) ──► ciphertext chunks
 │
 ├─ Egress
 │   ├─ Backlog accountant (egress_backlog_mb, overflow policy)
 │   └─ Upload workers ──► storage.ObjectWriter (resumable, atomic commit)
 │
 ├─ Status publisher (relay.json every 60s, hourly coverage)
 ├─ Retention job (deferred this round, §4.11)
 └─ HTTP 127.0.0.1: /healthz /metrics
     │
     ▼ SMB over Tailscale (sftp, s3 deferred; local for tests)
 Storage root (NAS) — layout per Contract §7
```

---

## 3. Constraints & Guarantees

### 3.1 Zero-disk (precise definition)

* `zrelay` MUST NOT create, write or truncate files on the relay host.
  * Exception: the `local` storage driver, when explicitly configured. This round it is intended for tests only. Production uses the `smb` driver, which speaks SMB itself: it creates no local files and uses no OS mount, so footage can never silently land on the relay host's disk because a network mount disappeared.
  * `os.CreateTemp`, file caches and on-disk queues are prohibited.
* Logs go to stdout/stderr only.
* **Outside process control, and documented:**
  * OS swap (macOS encrypts swap by default);
  * core dumps (the systemd unit SHOULD set `LimitCORE=0`);
  * launchd/journald log retention (never log secrets or frame data);
  * crash reports.
* Deferred this round (the host is macOS; the `relay.mlock` key is not accepted): on Linux, `relay.mlock: true` MAY call `mlockall(MCL_CURRENT|MCL_FUTURE)`. This requires `CAP_IPC_LOCK` or a sufficient `RLIMIT_MEMLOCK`; the relay fails to start if the call fails.

### 3.2 Memory budget

At startup the process sets `debug.SetMemoryLimit(max_memory_mb)`. `zrelay config check` MUST print the computed budget and fail if it exceeds `max_memory_mb × 0.8`.

Per camera, with `B = max_bitrate_kbps × 1000 / 8` bytes/s and `G = max_gop_seconds`:

```
ring_bytes    = B × (pre_roll_seconds + G) × 1.25
active_bytes  = 2 × chunk_size + B × G + part_buffer      // part_buffer: s3.part_size_mib (s3 deferred); smb, local (and sftp, deferred): 0
camera_bytes  = ring_bytes + active_bytes
total         = Σ camera_bytes + egress_backlog_mb + 32 MiB runtime overhead
```

`part_buffer` accounts for the **replay window** — bytes accepted by `ObjectWriter.Write` that are not yet durable and may need re-sending after a resume (§4.8). Those bytes live in the backlog ring, not inside the driver, so the term is charged once. For `smb` and `local` (and the deferred `sftp`) the window is zero because `Write` returns only on acknowledgement (an SMB WRITE response, or the operating system accepting the write) and `DurableOffset` tracks it; for the deferred `s3` it is one in-flight part.

Worked example: 3 Tapo cameras at 3 200 kbps (400 KiB/s), 15 s pre-roll, 2 s GOP, `smb` driver (`part_buffer` = 0).

* **Ring:** about 8.5 MiB per camera.
* **Active:** about 2.8 MiB per camera.
* **Total:** 3 × 11.3 + 128 + 32 ≈ **194 MiB**, which is under the 256 MiB limit.

Upload bandwidth has to keep up too. Three cameras recording at once need about 9.6 Mbps upstream; the relay exposes `zrelay_backlog_bytes` so you can confirm it does.

### 3.3 Performance targets (verify by benchmark; not assumptions)

| Target | Condition |
|---|---|
| `zrec` seal ≥ 1 GiB/s per core | Apple M-series and x86 with AES-NI |
| CPU < 5% of one core per camera while recording | 2K H.264 at 3 Mbps, no transcoding |
| Trigger-to-clip-open < 50 ms | Measured from ONVIF message receipt |

---

## 4. Modules

### 4.1 Supervisor & camera lifecycle

* Starts one pipeline per configured camera. A failure in one camera never affects another.
* Per-camera states: `connecting → priming → ready → degraded (rtsp or events down) → connecting`.
  * `ready` requires the RTSP buffer to hold at least one IDR **and** the event subscription to be active.
  * Transitions feed coverage intervals (Contract §7.5).
* Reconnect uses exponential backoff with full jitter, 1 s to 60 s, reset after 5 minutes of stability.
* `zrelay` uses exactly **one** RTSP session and **one** event subscription per camera. Tapo cameras limit concurrent clients, so this leaves a slot for the vendor app.

### 4.2 RTSP ingestion

* Library: `github.com/bluenviron/gortsplib` (pin the latest major at implementation time), TCP transport.
* **Codec parameters:** read from SDP (`sprop-parameter-sets`) and from in-band parameter-set NAL units. A change mid-stream triggers a `codec_change` split (Contract §5).
* **Access units:** reassemble NAL units into access units carrying PTS, DTS, an IDR flag and bytes.
* **Wall-clock mapping:** use the RTCP sender report NTP↔RTP mapping when the camera provides it (`time_source: rtcp`). Otherwise use the host arrival time of the first packet (`time_source: arrival`). The host MUST run NTP.
* **GOP measurement:** track the maximum observed GOP duration and export it as `zrelay_gop_max_seconds`. Log a warning if it exceeds `max_gop_seconds`. Some "smart" encoding modes stretch GOPs to 10 s or more, which inflates pre-roll and memory.
* Audio RTP streams are not set up (D10).

### 4.3 Pre-roll ring buffer

* A circular store of **whole GOPs**, each a slice of access units, with a running byte total.
* **Retention rule:** keep the newest GOP that starts at or before `now − pre_roll`, plus every GOP after it. Evict older GOPs whole.
* **Hard cap:** if the byte total exceeds `ring_bytes` (§3.2), evict the oldest GOP anyway and increment `zrelay_preroll_truncated_total`.
* Buffers come from a size-classed `sync.Pool` to limit GC churn. No per-frame allocation on the hot path beyond pool misses.
* On clip open, the ring hands the retained GOPs to the muxer by reference. It keeps receiving new GOPs, which become shared by both until the clip consumes them.

### 4.4 Event sources

```go
package events

type Kind string // "motion" | "person" | "vehicle" | "pet" | "other"

type Signal struct {
    Camera   string
    Kind     Kind
    Active   bool
    At       time.Time // host time of receipt, adjusted by the camera clock offset where the message has UtcTime
    RawTopic string
}

type Source interface {
    // Run blocks until ctx is done, emitting signals. It MUST reconnect internally
    // and report readiness changes via the ready callback.
    Run(ctx context.Context, emit func(Signal), ready func(bool)) error
}
```

**ONVIF PullPoint implementation**

Prefer a minimal custom SOAP client for the event service.

**Connection**

* **Clock offset:** call `GetSystemDateAndTime` on connect and every 30 minutes. Use the offset for the WS-UsernameToken `Created` timestamp; digest authentication fails under clock skew. Export the offset as `zrelay_clock_skew_seconds`.
* **Endpoint discovery:** `GetCapabilities` / `GetServices` on the configured port.

**Subscription lifecycle**

* `CreatePullPointSubscription` with `InitialTerminationTime = PT120S`.
* Loop on `PullMessages(Timeout=PT10S, MessageLimit=32)`.
* `Renew` at 50% of the remaining lifetime. If `Renew` is unsupported or faults, recreate the subscription. Whether the Tapo C220 supports `Renew` MUST be confirmed live with `zrelay onvif discover`.
* **Tapo-aware pull behaviour.** Tapo firmware drops most `PullMessages` HTTP connections, so a **transport error on `PullMessages` is normal**: retry on the **same** subscription after a short pause (about 250 ms). Do **not** recreate on transport errors. Count each retry in `zrelay_onvif_pull_retries_total{camera}` so a flapping camera is visible without logging every drop.
* Recreate the subscription only on: a SOAP fault indicating the subscription is invalid or expired, a failed `Renew`, or **no successful pull for 120 s** (stale), using backoff. Before recreating, send `Unsubscribe` best-effort to free the camera's limited subscription slot.
* If the camera returns a subscription address of `127.0.0.1` or `localhost`, rewrite it to the camera host.
* Send `Connection: close` on SOAP requests.
* On shutdown, send `Unsubscribe` (best effort).

**Message handling**

* Each message is matched by **topic regex + data item name + active value**, e.g. `SimpleItem Name="IsMotion" Value="true"`.
* Messages with `PropertyOperation="Initialized"` carry the current state after (re)subscribe:
  * an active one sets state as if `Changed`, without starting a duplicate clip;
  * an inactive one is ignored.
* Unmatched topics are counted in `zrelay_events_unmatched_total{camera,topic}` and logged at debug level.

**Which events trigger a clip.** Any event the camera emits over ONVIF with an "active" state starts a clip (owner requirement: record for every event); classification only labels it. Rules are written **per observed topic**, mapped to `motion` / `person` / `vehicle` / `pet` where the topic or item name makes that clear and to `other` otherwise (for example tamper or line-crossing). The §5.3 rule against a bare match-all `.*` without an `item` stays. Whether person, pet or vehicle detection is exposed over ONVIF varies by camera and firmware; if only motion is exposed, clips are labelled `motion`.

**Trigger tracker** (per camera, per kind)

* Kinds are `active` or `inactive`.
* A kind is forced inactive `trigger_timeout_seconds` after its last active signal. Some cameras never send the inactive message.
* The camera is "triggered" when any kind is active.

**`zrelay onvif discover --camera <id> [--listen 60s]`**

* Prints `GetEventProperties` topics and data items.
* Streams raw messages live, with `PropertyOperation` and every `SimpleItem`.
* Suggests `rules:` YAML: one rule per observed topic.
* Flags any observed topic that the suggested (or configured) rules do not cover. Such topics stay counted in `zrelay_events_unmatched_total`, so a firmware update that adds a topic is visible.
* **All example rules in this spec are illustrative. Confirm them per camera and firmware with this tool.** In particular, whether person detection is exposed over ONVIF varies by Tapo model and firmware.

### 4.5 Clip state machine (per camera)

| State | On | Action → Next |
|---|---|---|
| `Idle` | triggered | Open clip: ULID with timestamp = first retained frame time; new DEK; init segment; flush ring GOPs → `Recording` |
| `Recording` | all kinds inactive | Start post-roll timer → `PostRoll` |
| `PostRoll` | triggered | Cancel timer (coalesce) → `Recording` |
| `PostRoll` | timer elapsed | Finalize (`post_roll_elapsed`) → `Idle` |
| `Recording`/`PostRoll` | duration ≥ `max_clip_seconds`, at next IDR | Finalize (`max_duration`); immediately open the next segment at that IDR with the same `series_id`, `segment_index+1`, no pre-roll → same state |
| any active | parameter sets change | Finalize at the previous frame (`codec_change`); open the next segment if still triggered |
| any active | RTSP lost | Finalize with frames so far (`stream_lost`) → `Idle`; coverage gap starts. If no fragment was ever written, **abort** instead: an empty plaintext is an error (Contract §4.3.4), so the object and the staging sidecar are discarded and no sidecar is published |
| any active | backlog policy forces it (§4.8) | Finalize (`backlog_overflow`) → `Suppressed` |
| `Suppressed` | backlog < 50% of limit | → `Idle` (coverage records the gap) |
| any | SIGTERM | Finalize (`shutdown`); wait for uploads up to `shutdown_grace_seconds` |

Trigger bookkeeping per clip records `first_at_ms` / `last_at_ms` per kind, plus the raw topics, for the sidecar.

**Clock mixing.** `Signal.At` comes from host time adjusted by the ONVIF clock offset (§4.4), while clip start comes from the RTCP NTP mapping (§4.2). The two can disagree under skew, so a naive subtraction can produce a negative or past-the-end offset. Trigger offsets MUST be clamped to `[0, duration_ms]` before they reach the sidecar.

The state machine MUST take an injectable clock and be tested table-driven against a fake `Source`.

### 4.6 fMP4 muxer

* Library: `github.com/Eyevinn/mp4ff`.
* Follows the Contract §5 profile: init segment, then one `moof`+`mdat` per GOP, video only.
* Writes directly into the `zrec` writer (`io.Writer`). No whole-clip buffering.
* While writing, it records `init_segment_bytes`, the `seek_index` (media time and plaintext offset of each `moof`) and `plaintext_size_bytes` for the sidecar.
* A GOP is held in memory only until its fragment is written (≤ `B × G`).

### 4.7 zrec writer

* Implements Contract §4.3: header first, hold-back sealing, final flag on `Close`.
* The DEK exists only inside the writer. Its buffer is zeroed after `Close` or `Abort`.
* The writer emits ciphertext chunks into the clip's **backlog ring** (§4.8), never directly into a `storage.ObjectWriter`. The ring is bounded and non-blocking, so the camera pipeline is never blocked by network latency; a separate drain worker performs the blocking write.
* `Abort` is a normal path, not only an error path: backlog eviction (§4.8) can kill a clip mid-stream. It zeroes the DEK, releases the ring, and leaves the object uncommitted.

**Why the ring is mandatory.** `storage.ObjectWriter` embeds `io.Writer` and is explicitly allowed to block for backpressure (§4.9), while the muxer runs on the camera's hot path. Without an intervening buffer those two requirements are contradictory. The ring is that seam, and it is also the accounting unit for `egress_backlog_mb`.

### 4.8 Egress & backlog policy

**Upload streams**

* Each open clip gets an upload stream: a **seekable ring** of ciphertext bytes drained by one worker into a `storage.ObjectWriter`.
* At most `upload_workers` streams upload concurrently. Other streams queue, and their bytes count against the backlog.

**Resume contract (who retains what)**

`ObjectWriter` embeds `io.Writer`, which has no rewind, so the resume rule has to say explicitly who can re-deliver bytes:

* `Write` MAY return before its bytes are durable. `DurableOffset()` reports how far durability has actually reached.
* The **ring retains bytes until `DurableOffset()` passes them**, and releases them only then. This is why it is a ring rather than a FIFO.
* After a transient error the drain worker calls `Resume`, which reconnects and repositions the driver, then re-sends from the offset `Resume` returned. The driver does not replay on the caller's behalf, and the caller does not guess the offset.
* An `ObjectWriter` has a **single owner** and is not safe for concurrent use. The backlog accountant never calls `Abort` on a writer it does not own: eviction signals the owning drain-worker goroutine, which calls `Abort` itself. A lock inside the driver would make eviction wait behind a hung network write, and every driver must behave identically here.

The offset comes from `Resume` rather than from `DurableOffset` because only the driver can repair its own position — a filesystem driver stats and truncates the staging file, an S3 driver discards the part that failed. `Resume` MUST return an offset greater than or equal to the last value `DurableOffset` reported. `DurableOffset` is a monotonic floor: the ring releases bytes once it passes them, so an offset behind it would ask the caller for bytes it has already dropped. Filesystem, SMB and S3 drivers satisfy this naturally — a staging file is at least as long as the bytes `write(2)` accepted, an `smb` staging object is at least as long as the bytes its SMB WRITE responses acknowledged (after reconnecting, the driver stats the `.part` and continues from its size), and a discarded S3 part resumes exactly at the end of the last completed part.

Keeping the replay buffer in the ring rather than in the driver means the bytes are charged against `egress_backlog_mb` exactly once. If both layers buffered, in-flight bytes would be either double-counted or invisible to the accountant.

**Backlog accountant**

Tracks bytes that have been produced but are not yet durably written, across all streams. When the total exceeds `egress_backlog_mb`, it applies these steps in order until back under the limit:

1. **Evict the oldest finalized-but-uncommitted clip.** Abort its upload, release its bytes, record the drop in coverage, and increment `zrelay_clips_dropped_total{reason="backlog"}`.
2. **If no finalized clip remains,** force-finalize the in-progress clip with the largest backlog (`backlog_overflow`). Its camera enters `Suppressed`.

**Resumable writers**

* Drivers track a *durable offset*: acknowledged SMB/local writes (SMB WRITE responses for `smb`), or completed S3 parts and acknowledged SFTP writes once those deferred drivers exist.
* On a transient error, the writer reconnects and resumes from that offset. Bytes after it remain in the backlog.
* Retries use backoff capped at 30 s. A clip is abandoned only by the backlog policy or at shutdown.

### 4.9 Storage providers (shared package `pkg/storage`)

```go
package storage

type ObjectInfo struct {
    Key     string
    Size    int64
    ModTime time.Time
    ETag    string
}

type ObjectWriter interface {
    io.Writer                              // may block (backpressure)

    // DurableOffset reports how many bytes the driver has accepted and will
    // not ask for again. During streaming this means accepted by the
    // operating system (or, for s3, in a completed part) -- NOT fsynced.
    // The durability barrier is Commit. This is what the documented kill -9
    // recovery (Contract §7.4) needs, and it avoids an fsync per chunk on a
    // high-latency network mount.
    DurableOffset() int64

    // Resume reconnects if needed, repositions the driver to a known-good
    // offset, and returns it. The caller MUST continue writing from exactly
    // that offset. io.Writer has no rewind, so without this the driver and
    // the caller can silently disagree about where the next byte belongs and
    // the committed object ends up with duplicated or missing bytes.
    //
    // The returned offset is always >= the last DurableOffset: that value is
    // a monotonic floor the backlog ring relies on to release bytes (§4.8).
    Resume(ctx context.Context) (int64, error)

    // Commit atomically publishes the object (rename / CompleteMultipartUpload)
    // and is the durability barrier: filesystem drivers fsync the staging file
    // and then the directory chain from the object's directory up to the storage
    // root before returning (a new directory's entry is durable only once its own
    // parent is fsynced). The smb driver instead flushes the staging file
    // (SMB FLUSH) and then renames it: SMB has no directory fsync, so durability
    // of directory metadata is the server's responsibility (documented in the
    // driver).
    Commit(ctx context.Context) error

    Abort(ctx context.Context) error
}

type Writer interface {
    Create(ctx context.Context, key string) (ObjectWriter, error)
    PutSmall(ctx context.Context, key string, data []byte, contentType string) error // atomic, ≤ 1 MiB
    Delete(ctx context.Context, key string) error
}

// ObjectRange carries the whole-object metadata a Range response needs, so callers
// do not have to pair every OpenRange with a Stat. Objects are immutable once
// committed (Contract §7.3), so the values cannot go stale mid-read.
type ObjectRange struct {
    io.ReadCloser
    Total int64   // full object size, not the length of this range
    ETag  string
}

type Reader interface {
    // List yields at most one non-nil error and then stops. Prefix matching is
    // recursive and by whole path element: an object matches iff its key equals
    // prefix or begins with prefix + "/", so List("clips/front") does NOT return
    // clips/front-door/... (an S3 driver must filter its byte-prefix results to
    // match). An empty prefix lists everything; a prefix naming nothing yields an
    // empty sequence, not an error; a malformed prefix yields ErrInvalidKey. Order
    // is unspecified. Entries whose base name begins with "." are omitted
    // (Contract §7.2); callers that break early are safe.
    List(ctx context.Context, prefix string) iter.Seq2[ObjectInfo, error]
    Stat(ctx context.Context, key string) (ObjectInfo, error)
    OpenRange(ctx context.Context, key string, off, n int64) (*ObjectRange, error) // n = -1 → to EOF
}

// Incomplete describes one crash-recovery candidate (Contract §7.4).
type Incomplete struct {
    Key        string    // final key the object takes once committed
    StagingKey string    // ".<name>.part"; empty for s3
    UploadID   string    // s3 multipart upload id; empty otherwise
    Size       int64     // best-effort durable bytes (may be 0); never the intended length
    ModTime    time.Time
}

type Recoverer interface { // optional; used at startup (Contract §7.4)
    ListIncomplete(ctx context.Context, prefix string, olderThan time.Duration) ([]Incomplete, error)
    RecoverCommit(ctx context.Context, inc Incomplete) (ObjectInfo, error)
}

// ReadProvider is what zview-server binds to, so a data-mode deployment
// with read-only credentials cannot be handed write methods at all.
type ReadProvider interface {
    Reader
    Name() string
    HealthCheck(ctx context.Context) error
    Close() error
}

type Provider interface {
    ReadProvider
    Writer
}
```

`NewRangeReaderAt(ctx, r Reader, key string, size int64) io.ReaderAt` adapts a `Reader` for random-access chunk decryption. It lives here rather than in `pkg/zrec` so that `pkg/zrec` keeps zero project dependencies — the cross-language test vectors are only meaningful if the Go and TypeScript readers agree on bytes rather than on a shared helper.

Every key passes through a single `storage.CleanKey` function. It rejects `..`, absolute paths, backslashes and control characters. It **permits a leading `.`**, because crash recovery addresses staging objects by explicit key (Contract §7.2).

| Driver | Streaming write | Atomic commit | Notes |
|---|---|---|---|
| `local` | file writes | streamed objects: `.part` + `rename`; small objects (`PutSmall`): `.tmp` + `rename`, never `.part`; file and directory chain fsynced at `Commit` | For tests this round (and, in principle, network mounts; production uses `smb`). The only driver that touches the relay host's filesystem. A filesystem has no entity tag, so this driver synthesizes `ETag` from size, mtime (ns) and inode. Video and sidecar objects are immutable once committed (Contract §7.3), but `status/` objects are overwritten, and size+mtime alone can repeat on coarse-mtime mounts (SMB, exFAT). Overwrites publish by rename, which installs a fresh inode while the old one is alive, so the tag changes. Supported on unix only. Every driver's `ETag` MUST change when content changes. |
| `smb` | `github.com/hirochachacha/go-smb2` (pure Go, NTLM, SMB 3.x preferred); no OS mount and no local files; one SMB session per upload worker | `.<name>.part` + rename; `DurableOffset` = bytes acknowledged by SMB WRITE responses; `Resume` reconnects, stats the `.part` and continues from its size (≥ the last `DurableOffset`, §4.8); `Commit` = SMB FLUSH on the file, then rename (no directory fsync exists in SMB — the server owns metadata durability; documented in the driver) | `<mount>` = `//host/share/base_dir` (Contract §7.1). `PutSmall`: `.<name>.tmp` + rename, replacing an existing object (`status/` objects are overwritten); if the library cannot rename-with-replace, fall back to delete+rename and document the brief non-atomic window (acceptable only for `status/` objects; sidecars are never overwritten). `ETag` MUST change when content changes: the SMB file ID if the library exposes it, otherwise size + mtime (ns) + change time; the builder chooses and documents it. `List` MUST tolerate Synology-created `@eaDir` and `#recycle` entries (skip them; they never match Contract §7.2 names). Implements `Provider` and `Recoverer` with the same semantics as `local`. |
| `sftp` (deferred) | `golang.org/x/crypto/ssh` + `github.com/pkg/sftp` | `.part` + `PosixRename`, falling back to remove+rename | Not built this round (SFTP was tested by the owner and is not usable on the NAS); future driver. **MUST** verify the host key against `known_hosts`. Keeps one SSH connection per worker. |
| `s3` (deferred) | `aws-sdk-go-v2` multipart, `part_size_mib` ≥ 5 | `CompleteMultipartUpload` | Not built this round; future driver. Works with MinIO and R2. Sidecars use `PutObject`. |

Deferred: `sftp` and `s3` (above; each is a new driver plus a config value), `gcs`, and `multi` (fan-out with `min_success`).

### 4.10 Status & coverage publisher

Writes `status/relay.json` every 60 s and hourly coverage objects, following Contract §7.5. Both use `PutSmall`. A failure is logged and retried at the next tick.

### 4.11 Retention (optional; deferred this round)

Not built in Round 1: the owner manages NAS space manually (the Synology shared-folder quota is the safety net), and the `retention` config block is rejected (§5.3). The design below is retained for a later round.

* Runs daily at `retention.run_at_utc` and deletes clips older than `retention.days`.
* Deletion order is sidecar, then video (Contract §7.3).
* Coverage objects are deleted on the same schedule.
* The relay's storage credentials need delete permission for this. That is acceptable, since deletion is out of scope (Contract D5).

### 4.12 Observability

`http_listen` defaults to `127.0.0.1:9464`. Never bind it publicly.

* `/healthz` returns 200 when the supervisor is running and storage `HealthCheck` passed within the last 5 minutes.
* `/metrics` exposes Prometheus metrics:
  * `zrelay_camera_rtsp_up{camera}`, `zrelay_camera_events_up{camera}`
  * `zrelay_clips_total{camera,end_reason}`, `zrelay_clips_dropped_total{camera,reason}`
  * `zrelay_backlog_bytes`, `zrelay_ring_bytes{camera}`, `zrelay_gop_max_seconds{camera}`, `zrelay_preroll_truncated_total{camera}` (§4.3)
  * `zrelay_upload_bytes_total`, `zrelay_upload_errors_total{kind}`, `zrelay_upload_resumes_total`
  * `zrelay_onvif_resubscribe_total{camera}`, `zrelay_onvif_pull_retries_total{camera}` (§4.4 transport retries on the same subscription), `zrelay_clock_skew_seconds{camera}`, `zrelay_events_unmatched_total{camera,topic}`
  * No retention metrics this round (§4.11).
* Logs are structured JSON (`log/slog`) on stdout. They MUST NOT contain keys, credentials, DEKs or frame payloads.

---

## 5. Configuration

### 5.1 Secret references

Secret fields accept only references. Literal values are rejected unless `allow_literal_secrets: true`, which is intended for development only.

| Form | Meaning |
|---|---|
| `ENV:NAME` | Environment variable. |
| `FILE:/path` | File contents with the trailing newline trimmed. Warn if the mode is looser than `0600`. |
| `KEYCHAIN:service/account` | macOS Keychain generic password, read by running `/usr/bin/security find-generic-password -s <service> -a <account> -w` (no cgo). Its output is never logged. |

Prefer `KEYCHAIN:` or `FILE:` for the master key. An `ENV:` value set in a launchd plist or systemd unit is stored in that file on disk. The master key therefore defaults to `KEYCHAIN:zrelay/master` rather than `.env`: `.env` is plaintext on the SSD and that key decrypts every clip. `ENV:` and `FILE:` remain possible (`FILE:` is the alternative for a headless boot, because Keychain access needs the owner's login session).

**`--env-file <path>`.** `zrelay` and `zdecrypt` accept `--env-file <path>`, which loads `KEY=VALUE` lines into the process environment **before** `ENV:` references are resolved. Variables already set in the real environment win. This lets the owner keep camera and NAS settings in the repository's gitignored `.env`.

**Refs outside secret fields.** A camera's `host` and `events.port` accept `ENV:` references as well as literals, so existing entries such as `CAMERA_IP` and `ONVIF_PORT` are reused rather than duplicated.

### 5.2 Schema

```yaml
version: 2

relay:
  id: "home-mini"
  max_memory_mb: 256
  egress_backlog_mb: 64
  upload_workers: 2
  shutdown_grace_seconds: 30
  http_listen: "127.0.0.1:9464"

encryption:
  master_key: "KEYCHAIN:zrelay/master"   # zk1-… (Contract §3.1)
  chunk_size_kib: 1024

storage:
  driver: "smb"                    # local | smb   (sftp, s3 deferred)
  root: "zrelay"                   # key prefix inside the share; the mount MUST NOT repeat it (Contract §7.1)
  smb:
    host: "ENV:NAS_SMB_HOST"       # e.g. nas.<tailnet>.ts.net:445
    share: "cctv"
    base_dir: ""                   # path inside the share; MUST NOT repeat root
    username: "ENV:NAS_SMB_USER"
    password: "ENV:NAS_SMB_PASS"
  local:
    base_dir: "/path/for/tests"    # effective path: /path/for/tests/zrelay/clips/…

# The sftp and s3 storage blocks, and the retention and mlock keys, are deferred
# and not accepted this round (§5.3).

defaults:                          # inherited by every camera; override per camera
  pre_roll_seconds: 15
  post_roll_seconds: 10
  max_clip_seconds: 300
  max_bitrate_kbps: 4000           # budget input, not an encoder setting
  max_gop_seconds: 4
  trigger_timeout_seconds: 60

cameras:
  - id: "front-door"               # ^[a-z0-9][a-z0-9-]{0,63}$ (Contract §4.1)
    name: "Front Door"
    brand: "tapo"
    tags: ["exterior", "entrance"]
    host: "ENV:CAMERA_IP"          # literal or ENV: ref
    credentials:
      username: "ENV:CAM_FRONT_USER"
      password: "ENV:CAM_FRONT_PASS"
    rtsp:
      port: 554
      path: "/stream1"
    events:
      source: "onvif"
      port: "ENV:ONVIF_PORT"       # literal number or ENV: ref
      rules:                       # ILLUSTRATIVE — confirm with `zrelay onvif discover`
        - kind: motion
          topic: '(?i)RuleEngine/CellMotionDetector/Motion$'
          item: "IsMotion"
          active_value: "true"
        - kind: person
          topic: '(?i)RuleEngine/PeopleDetector/People$'
          item: "IsPeople"
          active_value: "true"

  - id: "backyard"
    name: "Backyard"
    brand: "reolink"
    tags: ["exterior", "garden"]
    host: "192.168.1.51"
    credentials:
      username: "ENV:CAM_BACKYARD_USER"
      password: "ENV:CAM_BACKYARD_PASS"
    rtsp: { port: 554, path: "/h264Preview_01_main" }
    events:
      source: "onvif"
      port: 8000
      rules:
        - { kind: motion,  topic: '(?i)MotionAlarm',     item: "State", active_value: "true" }
        - { kind: person,  topic: '(?i)PeopleDetect',    item: "State", active_value: "true" }
        - { kind: vehicle, topic: '(?i)VehicleDetect',   item: "State", active_value: "true" }
```

### 5.3 Validation (`zrelay config check`)

The check fails on any of the following:

* duplicate or invalid camera IDs;
* an unresolvable secret reference;
* an invalid or bad-checksum master key;
* `chunk_size` outside the Contract bounds, **or not a power of two** (Contract §4.1);
* a `storage.driver` other than `local` or `smb`;
* a mount whose trailing path element repeats `storage.root` (Contract §7.1): `local.base_dir`, or for `smb` the last element of `smb.base_dir`, or `smb.share` when `base_dir` is empty;
* an unknown key. Parsing is strict, so the legacy `retention` and `mlock` keys, and the deferred `storage.sftp` and `storage.s3` blocks (including `part_size_mib`, which is no longer validated), are rejected rather than silently ignored;
* a memory budget over 80% of `max_memory_mb`;
* a rule with an empty `topic` or a match-all pattern (`.*`) without an `item`.

On success it prints the budget table and the key fingerprint.

---

## 6. CLI

| Command | Purpose |
|---|---|
| `zrelay run --config <path>` | Run the daemon. |
| `zrelay config check --config <path>` | Validate the config and print the memory budget and key fingerprint. |
| `zrelay onvif discover --config <path> --camera <id> [--listen 60s]` | Topic and data-item discovery plus rule suggestions (§4.4). |
| `zrelay key generate [--qr]` | Print a new `zk1-…` key, its fingerprint and an optional terminal QR (uppercase form). |
| `zrelay key fingerprint --key <ref>` | Print `key_id`. |
| `zrelay key api-token-hash --key <ref>` | Print `hex(SHA-256(K_api))` for `zview-server`. Run it on a trusted machine, **never on the NAS**. |
| `zdecrypt --key <ref> --in <file.zrec> [--out <file.mp4 \| ->] [--sidecar <file.json>] [--allow-incomplete]` | Decrypt and verify. `--out -` streams to stdout (e.g. `\| vlc -`). |
| `zdecrypt rewrap --from <ref> --to <ref> --in <file.zrec> --out <file.zrec>` | Key rotation (Contract §3.4). |

Every `--key` flag takes a reference (`ENV:`, `FILE:`, `KEYCHAIN:`), never a literal, so the key stays out of shell history and `ps` output.

`zrelay` (every subcommand) and `zdecrypt` also accept `--env-file <path>` (§5.1): `KEY=VALUE` lines are loaded into the process environment before any `ENV:` reference is resolved, and variables already set in the real environment win.

`zdecrypt` exit codes:

| Code | Meaning |
|---|---|
| 0 | OK |
| 2 | Authentication failure |
| 3 | Truncated |
| 4 | Trailing data |
| 5 | Unknown key |
| 6 | Sidecar/header mismatch |
| 1 | Other error |

---

## 7. Technology Stack

| Layer | Choice | Notes |
|---|---|---|
| Language | Go (current stable, ≥ 1.24) | `crypto/hkdf` from the standard library, `iter`, `log/slog`. |
| RTSP | `bluenviron/gortsplib` (latest major) | Pure Go, TCP. |
| fMP4 | `Eyevinn/mp4ff` | Streaming fragment writer. |
| ONVIF events | Minimal custom SOAP client | PullPoint, WS-UsernameToken digest, clock offset. |
| Crypto | `crypto/aes`, `crypto/cipher`, `crypto/hkdf`, `crypto/rand` | Hardware-accelerated on ARMv8 and AES-NI. |
| SMB | `github.com/hirochachacha/go-smb2` | Pure Go, no OS mount (§4.9). |
| SFTP (deferred) | `golang.org/x/crypto/ssh`, `github.com/pkg/sftp` | Future driver, not built this round. Host key verification required. |
| S3 (deferred) | `aws-sdk-go-v2/service/s3` | Future driver, not built this round. Multipart streaming. |
| Metrics | `prometheus/client_golang` | Localhost only. |
| QR (CLI) | e.g. `github.com/mdp/qrterminal` | For key onboarding. |

---

## 8. Repository Layout (shared monorepo)

```
cmd/zrelay/  cmd/zdecrypt/  cmd/zview-server/
pkg/zrec/          # format writer/reader, key derivation, text key encoding (Contract §3–4)
pkg/sidecar/       # sidecar/status/coverage types + validation (Contract §6–7)
pkg/storage/       # interfaces + local, smb drivers (sftp, s3 deferred)
pkg/secretref/     # ENV:/FILE:/KEYCHAIN: resolution, --env-file loading
internal/relay/    # supervisor, rtsp, ring, events, clip FSM, mux, egress, status (retention deferred)
internal/zview/    # zview-server
web/               # zview SPA (includes web/src/lib/zrec — TS reader)
testdata/zrec/v1/  # cross-language vectors (Contract §4.5)
docs/specs/        # contract.md, relay-spec.md, ui-spec.md
```

---

## 9. Phased Implementation Guide (for LLM code generation)

Implement the phases in order. Each phase is done only when its acceptance criteria pass in CI or in a documented manual check.

**Round 1 mapping.** Round 1 builds these phases as the stages of the design (`docs/superpowers/specs/2026-10-15-zrelay-recorder-design.md` §7), with the deltas stated in each phase below:

| Round 1 stage | Phases |
|---|---|
| Stage 1 — Foundations | Phase 0 (without `zdecrypt`), and the interface, `local` driver and conformance suite of Phase 1 |
| Stage 2 — Tracks A, B, C | A: the `smb` driver of Phase 1 and `zdecrypt`; B: Phase 2; C: the event source and `discover` of Phase 3 |
| Stage 3 — Integration | The clip state machine of Phase 3, and Phase 4 |
| Stage 4 — Operations | Phase 5 |

### Phase 0 — `pkg/zrec`, `pkg/secretref`, `zdecrypt`

* Key text encoding and HKDF derivations, the streaming writer with an injectable RNG, and a random-access reader with the exact error taxonomy (Contract §4.4). `pkg/secretref` includes `--env-file` loading and `KEYCHAIN:` via `security` (§5.1).
* **Acceptance:**
  * Vectors in `testdata/zrec/v1/` are generated and committed.
  * Property tests pass over random sizes (1 B .. 5 × `chunk_size`, including exact multiples).
  * Every negative vector yields its specified error.
  * `zdecrypt rewrap` round-trips. (Round 1: `zdecrypt`, including `rewrap` and the exit codes, moves to Track A of Stage 2.)

### Phase 1 — `pkg/storage`

* `local` and `smb` drivers (the `sftp` and `s3` drivers are deferred this round), with resumable `ObjectWriter`, atomic commit and `Recoverer`, plus a shared driver conformance suite that every driver passes unchanged.
* **Acceptance:**
  * With a fault-injecting TCP proxy killing connections mid-upload, the committed object is byte-identical to the source. The `smb` driver runs against Samba in Docker.
  * `.part` names are invisible to `List`.
  * Recovery commits a partial object.
  * Path traversal keys are rejected.
  * A Synology live smoke test (write, list, rename-over-existing, the path the share exposes, no `#recycle` entries) is written with the `smb` driver but runs only once the NAS is ready (`docs/runbook/synology-setup.md`).

### Phase 2 — RTSP, ring buffer, muxer (offline pipeline)

* Record from a real camera, or a test RTSP server replaying a file, into `zrec` via the `local` driver in a test directory. `ffmpeg` and `ffprobe` run from a Docker image and are never installed on the host; video fixtures are synthetic and small.
* **Acceptance:**
  * `zdecrypt | ffprobe` shows a valid fMP4 with every fragment starting on an IDR.
  * `seek_index` offsets point at `moof` boxes.
  * The ring's byte total stays ≤ `ring_bytes` over a one-hour soak (which may run against the live camera).
  * Pre-roll ≥ configured value.

### Phase 3 — Event sources & clip state machine

* The ONVIF PullPoint client with the Tapo-aware behaviour of §4.4, the rule matcher and the trigger tracker. The FSM runs with a fake clock and a fake `Source`. Build `zrelay onvif discover`.
* **Acceptance:**
  * Table-driven tests cover coalescing, max-duration split with a shared `series_id`, missing-inactive timeout, `Initialized` handling, stream loss and codec change.
  * The ONVIF client is tested against a fake ONVIF HTTP server, including dropped connections, a `127.0.0.1` subscription address and SOAP faults.
  * `discover` prints topics from a real Tapo camera, and the generated rules are captured in the example config.
  * Subscription survives a forced camera reboot.

### Phase 4 — Egress, backlog policy, sidecars, status & coverage, recovery (retention deferred)

* **Acceptance:**
  * A chaos test runs 3 synthetic 4 Mbps cameras with constant triggers while storage is unreachable for 10 minutes. The process RSS stays under `max_memory_mb`, drops appear in coverage and metrics, and uploads resume afterwards.
  * A crash test with `kill -9` mid-clip produces an `incomplete` sidecar on restart.
  * Commit ordering is verified: a sidecar never exists without its video.

### Phase 5 — Operations

* Implement `/healthz` and `/metrics`, `config check` budget output (including the §5.3 root rule), a per-user macOS LaunchAgent plist and a README runbook. The plist MUST NOT set `StandardOutPath` or `StandardErrorPath` to files and MUST NOT copy `.env` values into itself; logs go to the unified log at `info` level. A LaunchAgent (not a LaunchDaemon) is required so `KEYCHAIN:` secrets are reachable; `FILE:` is the documented alternative for a headless boot. The systemd unit example (`LimitCORE=0`) is deferred with Linux support.
* **Acceptance:**
  * A 72-hour live soak on the relay host shows no goroutine or memory growth, RSS under 256 MiB, and no recorder-originated file writes on the host (verified by `fs_usage` / `lsof` sampling).
  * Every metric in §4.12 is present.

---

## 10. Future Work

* **Encrypted on-disk spool** for long outages. Ciphertext only, so it is compatible with G5 if the zero-disk rule is relaxed.
* **Sealed metadata** (sidecar schema 3), to hide activity patterns from storage (reverses Contract D3).
* **Asymmetric recipients** (HPKE / ECDH P-256), per-person keys and revocation, and protection of past clips if the relay host is stolen.
* **Additional backends:** the deferred `sftp` and `s3` drivers (each a new driver plus a config value, §4.9), `gcs` and `multi`.
* **Retention** (§4.11) and **Linux `mlock`** (§3.1), deferred from Round 1.
* **Opt-in audio transcode** to AAC or Opus.
* **Offline alerts** (camera or relay down) via webhook or ntfy.
