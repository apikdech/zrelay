# zrelay runbook

How to set up and operate zrelay v2 on the Mac mini (OrbStack) with MinIO on the Synology.
It follows design §8.3 and the decisions D11 to D19 in
`docs/superpowers/specs/2026-11-06-zrelay-v2-design.md`.

The shape of the system, for orientation:

* Two containers, `zrelay` and `zview-server`, from `deploy/compose.yaml`. Both have
  `read_only: true` and no volumes, so nothing can be written to disk (zero-disk, G5).
* zrelay records event clips, encrypts them and uploads them to MinIO. Its S3 credentials can
  create objects but can never delete an archive object (D15).
* Retention is a MinIO lifecycle (ILM) rule, not zrelay code. It keeps running while zrelay is down.
* The camera, the Mac mini and the Synology are all on the tailnet. MinIO is tailnet-only.

All commands below run on the Mac mini from the repository root unless a step says otherwise.
`mc` is the MinIO client; `<alias>` is an alias you set once with `mc alias set`.

## 1. MinIO: the bucket

Use a bucket that holds only zrelay data. The relay's list permission cannot be narrowed to the
`zrelay/` prefix for a prefix-less request (see section 2), so a dedicated bucket keeps the
exposure to key names of zrelay's own objects.

```sh
mc alias set <alias> https://nas.<tailnet>.ts.net:9000 <admin-access-key> <admin-secret-key>
mc mb <alias>/cctv
mc version info <alias>/cctv        # must report that versioning is not enabled
```

**Bucket versioning stays off.** With versioning on, an expiry rule leaves noncurrent versions
behind and storage is never reclaimed (design §6.7). A freshly created bucket is not versioned.
If `mc version info` reports versioning enabled, create a new bucket instead of reusing it.

The names `cctv` (bucket) and `zrelay` (the `storage.root` of `zrelay.yaml`) are the ones used
in `deploy/zrelay.example.yaml` and in the two policy files. Using other names means editing the
`Resource` ARNs in both policy files (`arn:aws:s3:::cctv/zrelay/...`) and the prefix conditions.

## 2. MinIO: policies and users

Two users, each with its own policy.

| User | Policy file | Allowed |
|---|---|---|
| `zrelay` (the relay) | `deploy/minio-relay-policy.json` | `PutObject`, `GetObject`, `ListMultipartUploadParts`, `AbortMultipartUpload` on `cctv/zrelay/*`; `ListBucket` on the bucket for the prefix `zrelay/`; `ListBucketMultipartUploads` on the bucket; `DeleteObject` **only** on `cctv/zrelay/clips/*/.*` (dot-named staging sidecars). |
| `zview` (zview-server) | `deploy/minio-zview-policy.json` | `GetObject` on `cctv/zrelay/*`; `ListBucket` on the bucket. Nothing else. |

```sh
mc admin policy create <alias> zrelay-relay deploy/minio-relay-policy.json
mc admin policy create <alias> zrelay-zview deploy/minio-zview-policy.json

mc admin user add <alias> zrelay <relay-secret-key>
mc admin policy attach <alias> zrelay-relay --user zrelay

mc admin user add <alias> zview <zview-secret-key>
mc admin policy attach <alias> zrelay-zview --user zview
```

Use long random secret keys (`openssl rand -hex 24`). Do not reuse the admin key anywhere in
this deployment.

Why the relay cannot delete the archive (D15): the only `DeleteObject` statement matches keys
whose last path element starts with a dot. Archive objects (`<name>.zrec`, `<name>.json`) never
do, so a compromised relay host cannot wipe the footage with the relay's own credentials.
`TestRelayPolicyDeniesArchiveDelete` (`go test -tags integration ./deploy/`) checks this against
a real MinIO, and `TestRelayPolicyWorksWithTheS3Driver` checks that the policy still allows
everything the relay does.

Two MinIO quirks the policy files work around, found by those tests:

* MinIO rejects an `s3:prefix` condition on `s3:ListBucketMultipartUploads`, so that action is
  allowed on the bucket without a prefix. Crash recovery lists in-progress uploads with it.
* The health probe is a `HeadBucket`, which MinIO authorizes as `s3:ListBucket` with no
  `s3:prefix` key. The extra `Null` statement allows exactly that. MinIO treats a listing with
  an empty prefix the same way, so the relay and zview users can list key names of the whole
  bucket. Reading and writing stay inside `zrelay/`. This is the reason for the dedicated bucket.

## 3. MinIO: retention (ILM)

Retention is two lifecycle rules (design §6.7). Choose the number of days `<N>` you want to keep.

```sh
mc ilm rule add <alias>/cctv --prefix "zrelay/clips/"           --expire-days <N>
mc ilm rule add <alias>/cctv --prefix "zrelay/status/coverage/" --expire-days <N>
mc ilm rule ls <alias>/cctv
```

Set `index.retention_days` in `zview.yaml` to the same `<N>`.

Notes:

* A clip's video and sidecar expire independently, seconds apart. zview treats a sidecar whose
  video is gone as invalid, so at worst a cached day index shows a clip whose video then
  answers 404 ("video unavailable").
* If you also add a rule that aborts incomplete multipart uploads, it must be **at least 7 days**.
  Crash recovery scans only today and yesterday, so a shorter rule could abort an upload before
  recovery sees it.
* `zrelay/status/relay.json` is rewritten continuously and needs no rule.

## 4. Keys

Generate the household master key once and keep it safe. If it is lost, the footage cannot be
decrypted; there is no recovery.

```sh
docker run --rm --entrypoint /zrelay zrelay:local key generate --qr
```

(Build the image first with `docker compose -f deploy/compose.yaml build zrelay`, or run
`go run ./cmd/zrelay key generate --qr` from a checkout.) The command prints the `zk1-...` key,
its fingerprint (`key_id`) and, with `--qr`, a terminal QR code for onboarding phones.

Store the key in two places you control (a password manager and an offline copy). Then derive
what zview-server needs:

```sh
# Master key into the file the container reads (no trailing newline needed).
# Paste the key at the silent prompt: it is never typed into a command line,
# so it stays out of shell history and `ps` output.
umask 077
mkdir -p deploy/secrets
echo "Paste the master key, then press Enter (nothing is echoed):"
read -rs KEY && printf '%s' "$KEY" > deploy/secrets/zrelay_master; unset KEY

# Hash of the API credential, for the live-view and API bearer check.
ZR_KEY=$(cat deploy/secrets/zrelay_master) \
  docker run --rm --entrypoint /zrelay -e ZR_KEY zrelay:local key api-token-hash --key ENV:ZR_KEY \
  > deploy/secrets/api_token_sha256

ZR_KEY=$(cat deploy/secrets/zrelay_master) \
  docker run --rm --entrypoint /zrelay -e ZR_KEY zrelay:local key fingerprint --key ENV:ZR_KEY
```

`api-token-hash` prints `hex(SHA-256(K_api))`. Both zrelay (`live.api_token_sha256`) and
zview-server read it from the same file. To rotate the credential, put two hashes in the list
(`api_token_sha256` accepts up to two) while the family switches over.

Every `--key` flag takes a reference (`ENV:NAME` or `FILE:/path`), never the literal key.
Likewise never paste the key into any shell command: it would persist in `~/.zsh_history` or
`~/.bash_history`, a plaintext copy outside `deploy/secrets` that `chmod 600` does not cover. If
you did, delete that history entry (and any synced or backed-up copy of the history file).

## 5. Docker secrets and the environment file

The files below live in `deploy/` and are never committed. `deploy/compose.yaml` documents the same list at its head.

| File | Content | Mounted into |
|---|---|---|
| `secrets/zrelay_master` | the `zk1-...` master key | zrelay, at `/run/secrets/zrelay_master` |
| `secrets/s3_secret_key` | secret key of the MinIO user `zrelay` | zrelay |
| `secrets/api_token_sha256` | output of `zrelay key api-token-hash` | zrelay and zview-server |
| `secrets/s3_ro_secret_key` | secret key of the MinIO user `zview` | zview-server |
| `.env` | `S3_ENDPOINT`, `S3_ACCESS_KEY` (the MinIO user `zrelay`), `S3_RO_ACCESS_KEY` (the user `zview`), `RESTREAM_USER`, `RESTREAM_PASS`, `CAMERA_IP`, `CAMERA_USERNAME`, `CAMERA_PASSWORD` | zrelay (whole file); zview-server gets only `S3_ENDPOINT` and `S3_RO_ACCESS_KEY` |

`TAILSCALE_IP` is not in `.env`: `deploy.sh` sets it from `tailscale ip -4`.

```sh
chmod 600 deploy/secrets/* deploy/.env
cp deploy/zrelay.example.yaml deploy/zrelay.yaml
cp deploy/zview.example.yaml  deploy/zview.yaml
```

Edit `deploy/zrelay.yaml`:

* Inside the container, set `relay.http_listen: "0.0.0.0:9464"`. Compose publishes it only on
  `127.0.0.1` of the host, so metrics are never reachable from the network. Keep the default
  `127.0.0.1:9464` for any run outside a container. Never bind metrics publicly.
* `relay.shutdown_grace_seconds` must stay below `stop_grace_period` (40 s) in `compose.yaml`.
* The camera block (`host`, credentials, `rtsp.path`) for each camera.

Check the config and the memory budget before starting:

```sh
TAILSCALE_IP=$(tailscale ip -4 | head -n 1) docker compose -f deploy/compose.yaml run --rm --no-deps \
  --entrypoint /zrelay zrelay config check --config /etc/zrelay/zrelay.yaml
```

## 6. Start

```sh
deploy/deploy.sh
docker compose -f deploy/compose.yaml ps
curl -fsS http://127.0.0.1:9464/healthz
```

`deploy.sh` takes `TAILSCALE_IP` from `tailscale ip -4`. zrelay publishes the RTSP restream
(8554/tcp) and the WebRTC ICE port (8189/udp) on the Tailscale IP only; the live signaling port
9465 is not published and zview-server reaches it as `http://zrelay:9465` on the compose network.

## 7. HTTPS for zview: `tailscale serve`

zview-server listens on `127.0.0.1:8080` of the host. Put HTTPS in front of it with Tailscale:

```sh
tailscale serve --bg --https=443 http://127.0.0.1:8080
tailscale serve status
```

The SPA, `/api/v1/*` and `/live/*` are all served from that one origin (D18). Phones open
`https://<mac-mini>.<tailnet>.ts.net/`.

## 8. OrbStack: Local Network permission

The camera is on the home LAN, not on the tailnet. On macOS Sequoia and later an application
needs the **Local Network** permission to reach LAN addresses. The container's traffic leaves
through OrbStack, so OrbStack must have it:

1. System Settings, Privacy & Security, Local Network.
2. Turn on **OrbStack**. If it is not listed, quit OrbStack, start it again and run any
   container that connects to a LAN address; macOS then asks.
3. Restart the stack: `docker compose -f deploy/compose.yaml restart zrelay`.

Symptom of a missing permission: the RTSP session and the ONVIF subscription never come up
(`zrelay_camera_rtsp_up 0`, `no route to host` in the logs) while `ping <camera-ip>` works from
the host shell. The permission can be dropped by a macOS update; check it first after one.

## 9. Family onboarding (QR)

Each family member imports the master key into the SPA once; the key never leaves the browser.

1. `zrelay key generate --qr` (section 4) prints the QR code for the key it creates, in the
   uppercase form the scanner expects. Show that QR code from that terminal while the key is
   being created, or keep the QR in your password manager. The SPA also accepts the pasted
   `zk1-...` text.
2. On the phone, open the zview URL (section 7), go to Key, and scan or paste the key.
3. Confirm the fingerprint shown in the SPA equals the output of `zrelay key fingerprint`.

Do not send the key through chat or email in clear text; show the QR in person.

## 10. Observing zrelay

`/healthz` and `/metrics` are on `127.0.0.1:9464`.

* `/healthz` is 200 while zrelay is running and the storage circuit breaker has not been out of
  the closed state for more than 5 minutes; otherwise 503 with the reason. A shorter outage is by
  design absorbed by the backlog and must not restart the container.
* `/metrics` is Prometheus text. The ones to watch:

| Metric | Meaning | Alert when |
|---|---|---|
| `zrelay_storage_breaker_state` | 0 closed, 1 half-open, 2 open | `== 2` for more than 2 minutes |
| `zrelay_backlog_bytes` | ciphertext waiting for upload | above 80% of `egress_backlog_mb` |
| `zrelay_clips_dropped_total{reason}` | clips lost (`backlog`, `shutdown`, `stream_lost`) | any increase |
| `zrelay_camera_rtsp_up`, `zrelay_camera_events_up` | link health per camera | `== 0` for more than 5 minutes |
| `zrelay_upload_errors_total{kind}` | failed storage requests | sustained rate |
| `zrelay_gop_max_seconds` | longest GOP seen | above `max_gop_seconds` (inflates pre-roll memory) |
| `zrelay_recorder_lag_total` | recorder queue overflows | any increase |
| `zrelay_live_viewers`, `zrelay_restream_clients` | open live sessions | informational |
| `process_resident_memory_bytes`, `go_goroutines` | process size | trend up over days |

Logs are JSON on stdout (`docker compose -f deploy/compose.yaml logs -f zrelay`). They never contain
keys, credentials or frame data.

## 11. Outage tolerance: how long can MinIO be down?

zrelay never writes to disk. While storage is unreachable, finished and in-progress clips wait
in memory, in the egress backlog (`relay.egress_backlog_mb`). When it is full, the oldest
finished clip is dropped (recorded in coverage as `backlog`), and then the largest in-progress
clip is cut. The circuit breaker stops the upload attempts meanwhile and probes for recovery.

The tolerance is roughly

```
minutes = egress_backlog_mb * 8 * 1.048576 / (combined_recording_mbps * 60)
```

(`egress_backlog_mb * 1.048576` converts MiB to megabytes; `* 8` to megabits.) Examples:

| `egress_backlog_mb` | Recording | Tolerance |
|---|---|---|
| 512 | one camera at 3.2 Mbps, recording continuously | about 22 minutes |
| 512 | three cameras at 3.2 Mbps each (9.6 Mbps), all recording continuously | about 7 minutes |
| 128 | one camera at 4 Mbps continuously | about 4.5 minutes |

Event clips are bursty, so the real tolerance is usually much longer than the
continuous-recording figures. `zrelay_backlog_bytes` shows how full it is.

To raise the tolerance, raise `egress_backlog_mb` and `max_memory_mb` together; `zrelay config
check` prints the budget that must fit under `max_memory_mb`. A clip that is still waiting when
zrelay stops (SIGTERM) is dropped and counted as `shutdown`, because the coverage object cannot
be published while storage is down.

## 12. Soak test (72 hours, manual)

Before relying on a deployment, run it for 72 hours with the real camera or cameras and record
the process size. Because the container is `read_only: true` with no volumes, any attempt to
write a file would fail and crash the process, so **zero restarts** also proves that nothing
wrote to disk.

Start the stack (section 6), note the start, then on the host sample every 15 minutes:

```sh
mkdir -p ~/zrelay-soak
cat > ~/zrelay-soak/sample.sh <<'SH'
#!/bin/sh
# Appends: UTC time, RSS bytes, goroutines, restart count, container start time.
out="$HOME/zrelay-soak/soak.csv"
[ -s "$out" ] || echo "time,rss_bytes,goroutines,restart_count,started_at" > "$out"
while :; do
  m=$(curl -fsS http://127.0.0.1:9464/metrics) || m=""
  rss=$(printf '%s\n' "$m" | awk '/^process_resident_memory_bytes /{print $2}')
  gor=$(printf '%s\n' "$m" | awk '/^go_goroutines /{print $2}')
  ins=$(docker inspect -f '{{.RestartCount}},{{.State.StartedAt}}' zrelay-zrelay-1 2>/dev/null)
  echo "$(date -u +%FT%TZ),${rss:-NA},${gor:-NA},${ins:-NA,NA}" >> "$out"
  sleep 900
done
SH
chmod +x ~/zrelay-soak/sample.sh
nohup ~/zrelay-soak/sample.sh >/dev/null 2>&1 &
```

Keep motion happening: leave the camera's detection on and trigger it a few times a day, and
keep one live view open from a phone for at least an hour per day.

After 72 hours (288 samples), stop the sampler and check:

```sh
kill %1 2>/dev/null; pkill -f zrelay-soak/sample.sh
awk -F, 'NR>1 && $2!="NA"{if(min==""||$2<min)min=$2; if($2>max)max=$2} END{print "rss min/max MiB:", min/1048576, max/1048576}' ~/zrelay-soak/soak.csv
awk -F, 'NR>1 && $3!="NA"{if(min==""||$3<min)min=$3; if($3>max)max=$3} END{print "goroutines min/max:", min, max}' ~/zrelay-soak/soak.csv
cut -d, -f4,5 ~/zrelay-soak/soak.csv | sort -u
docker compose -f deploy/compose.yaml logs zrelay | grep -ci "read-only file system"
```

Pass criteria: restart count `0` and one distinct start time; maximum RSS under `max_memory_mb`;
no steady climb in RSS (the median of the last 6 hours within 10% of the median of hours 6 to 12);
goroutines flat (no upward trend, last-6-hour maximum within 20 goroutines of the first-day
maximum); no `read-only file system` lines; `zrelay_clips_dropped_total` unchanged by anything but
deliberate outages.

### Soak results

_Not run yet. The owner fills this in after the 72-hour run._

| Item | Value |
|---|---|
| Start (UTC) | |
| End (UTC) | |
| zrelay version / image | |
| Cameras and bitrate | |
| `max_memory_mb` / `egress_backlog_mb` | |
| RSS min / max (MiB) | |
| RSS median, hours 6 to 12 / last 6 hours (MiB) | |
| Goroutines min / max | |
| Container restarts | |
| `read-only file system` log lines | |
| Clips recorded / dropped | |
| Verdict (pass / fail) and notes | |
