# zrelay runbook

This runbook tells you how to install and operate zrelay v2. zrelay runs on the Mac mini
(OrbStack). MinIO runs on the Synology NAS. The procedures follow design §8.3 and the decisions
D11 to D19 in `docs/superpowers/specs/2026-11-06-zrelay-v2-design.md`.

Overview of the system:

* Two containers, `zrelay` and `zview-server`, come from `deploy/compose.yaml`. The two containers
  have `read_only: true` and no volumes. Thus nothing can write to disk (zero-disk, G5).
* zrelay records event clips, encrypts them, and sends them to MinIO. Its S3 credentials can make
  objects, but they cannot delete an archive object (D15).
* A MinIO lifecycle (ILM) rule removes old clips. zrelay has no retention code. The rule also
  operates when zrelay is stopped.
* The Mac mini and the Synology are on the tailnet. MinIO is available only on the tailnet. The
  camera can be on the home LAN (see section 8).

Do all commands on the Mac mini, from the repository root, unless a step gives a different
location. `mc` is the MinIO client. `<alias>` is the name that you give to your MinIO server one
time with `mc alias set`.

## 1. MinIO: the bucket

Use a bucket that contains only zrelay data. You cannot limit the list permission of the relay to
the `zrelay/` prefix for a request without a prefix (see section 2). Thus, with a dedicated bucket,
the relay sees only the key names of zrelay objects.

```sh
mc alias set <alias> https://nas.<tailnet>.ts.net:9000 <admin-access-key> <admin-secret-key>
mc mb <alias>/cctv
mc version info <alias>/cctv        # must report that versioning is not enabled
```

**Keep bucket versioning off.** If versioning is on, an expiry rule keeps the old versions, and
MinIO never gets the storage space back (design §6.7). A new bucket does not have versioning. If
`mc version info` shows that versioning is on, make a new bucket. Do not use the old bucket.

`deploy/zrelay.example.yaml` and the two policy files use the names `cctv` (the bucket) and
`zrelay` (the `storage.root` in `zrelay.yaml`). If you use other names, change the `Resource` ARNs
(`arn:aws:s3:::cctv/zrelay/...`) and the prefix conditions in the two policy files.

## 2. MinIO: policies and users

Make two users. Each user has its own policy.

| User | Policy file | Permissions |
|---|---|---|
| `zrelay` (the relay) | `deploy/minio-relay-policy.json` | `PutObject`, `GetObject`, `ListMultipartUploadParts`, `AbortMultipartUpload` on `cctv/zrelay/*`; `ListBucket` on the bucket for the prefix `zrelay/`; `ListBucketMultipartUploads` on the bucket; `DeleteObject` **only** on `cctv/zrelay/clips/*/.*` (staging sidecars, which have names that start with a dot). |
| `zview` (zview-server) | `deploy/minio-zview-policy.json` | `GetObject` on `cctv/zrelay/*`; `ListBucket` on the bucket. No other permissions. |

```sh
mc admin policy create <alias> zrelay-relay deploy/minio-relay-policy.json
mc admin policy create <alias> zrelay-zview deploy/minio-zview-policy.json

mc admin user add <alias> zrelay <relay-secret-key>
mc admin policy attach <alias> zrelay-relay --user zrelay

mc admin user add <alias> zview <zview-secret-key>
mc admin policy attach <alias> zrelay-zview --user zview
```

Use long random secret keys (`openssl rand -hex 24`). Do not use the admin key again in this
installation.

Why the relay cannot delete the archive (D15): the only `DeleteObject` statement applies to keys
whose last path element starts with a dot. The names of archive objects (`<name>.zrec`,
`<name>.json`) never start with a dot. Thus, if an attacker gets control of the relay host, the
relay credentials cannot delete the recordings. `TestRelayPolicyDeniesArchiveDelete`
(`go test -tags integration ./deploy/`) checks this on a real MinIO.
`TestRelayPolicyWorksWithTheS3Driver` checks that the policy still lets the relay do all of its
work.

The policy files go around two MinIO problems. The tests found these problems:

* MinIO does not accept an `s3:prefix` condition on `s3:ListBucketMultipartUploads`. Thus the
  policy gives this permission on the bucket without a prefix. Crash recovery uses it to list the
  uploads that are not complete.
* The health check is a `HeadBucket`. MinIO authorizes it as `s3:ListBucket` without an
  `s3:prefix` key. The extra `Null` statement permits exactly that. MinIO also applies this rule
  to a list request with an empty prefix. Thus the relay and zview users can list key names in
  all of the bucket. They can read and write only in `zrelay/`. For this reason, use a dedicated
  bucket.

## 3. MinIO: retention (ILM)

Two lifecycle rules control the retention (design §6.7). Select the number of days `<N>` that
MinIO keeps the clips.

```sh
mc ilm rule add <alias>/cctv --prefix "zrelay/clips/"           --expire-days <N>
mc ilm rule add <alias>/cctv --prefix "zrelay/status/coverage/" --expire-days <N>
mc ilm rule ls <alias>/cctv
```

Set `index.retention_days` in `zview.yaml` to the same `<N>`.

> **CAUTION:** If you also add a rule that stops incomplete multipart uploads, set it to
> **7 days or more**. Crash recovery looks only at today and yesterday. A shorter rule can stop an
> upload before recovery finds it.

Notes:

* The video and the sidecar of a clip expire separately, a few seconds apart. zview does not show
  a sidecar if its video is not there. Thus, in the worst case, a day index in the cache shows a
  clip, and the video request gives 404 ("video unavailable").
* zrelay writes `zrelay/status/relay.json` again and again. This object does not need a rule.

## 4. Keys

Make the household master key one time.

> **WARNING:** Keep the master key safe. If you lose it, you cannot decrypt the recordings. There
> is no recovery.

```sh
docker run --rm --entrypoint /zrelay zrelay:local key generate --qr
```

Before you run this command, build the image with
`docker compose -f deploy/compose.yaml build zrelay`. Or, from a checkout, run
`go run ./cmd/zrelay key generate --qr`. The command shows the `zk1-...` key and its fingerprint
(`key_id`). With `--qr`, it also shows a QR code in the terminal. Use this QR code to add the key
to the phones.

Keep the key in two locations that you control: a password manager and an offline copy. Then make
the files that zrelay and zview-server need:

```sh
# Write the master key into the file that the container reads.
# Paste the key at the prompt. The prompt does not show the key, and the key
# does not go into the shell history or the `ps` output.
umask 077
mkdir -p deploy/secrets
echo "Paste the master key, then press Enter (nothing is echoed):"
read -rs KEY && printf '%s' "$KEY" > deploy/secrets/zrelay_master; unset KEY

# Make the hash of the API credential. zrelay and zview-server use it to
# check the bearer token of live video and of the API.
ZR_KEY=$(cat deploy/secrets/zrelay_master) \
  docker run --rm --entrypoint /zrelay -e ZR_KEY zrelay:local key api-token-hash --key ENV:ZR_KEY \
  > deploy/secrets/api_token_sha256

# Show the key fingerprint.
ZR_KEY=$(cat deploy/secrets/zrelay_master) \
  docker run --rm --entrypoint /zrelay -e ZR_KEY zrelay:local key fingerprint --key ENV:ZR_KEY
```

`api-token-hash` shows `hex(SHA-256(K_api))`. zrelay (`live.api_token_sha256`) and zview-server
read it from the same file. To change the credential, put two hashes in the list
(`api_token_sha256` accepts two hashes). Keep the two hashes until the family uses the new key.

Each `--key` flag takes a reference (`ENV:NAME` or `FILE:/path`). It does not take the key itself.

> **WARNING:** Do not type or paste the key into a shell command. The shell keeps the command in
> `~/.zsh_history` or `~/.bash_history`. That is a plain-text copy outside `deploy/secrets`, and
> `chmod 600` does not protect it. If you typed the key into a command, delete that history entry.
> Also delete all synchronized copies and backup copies of the history file.

## 5. Docker secrets and the environment file

The files that follow are in `deploy/`. Do not commit them. The start of `deploy/compose.yaml`
gives the same list.

| File | Contents | Which container reads it |
|---|---|---|
| `secrets/zrelay_master` | The `zk1-...` master key | zrelay, at `/run/secrets/zrelay_master` |
| `secrets/s3_secret_key` | The secret key of the MinIO user `zrelay` | zrelay |
| `secrets/api_token_sha256` | The output of `zrelay key api-token-hash` | zrelay and zview-server |
| `secrets/s3_ro_secret_key` | The secret key of the MinIO user `zview` | zview-server |
| `.env` | `S3_ENDPOINT`, `S3_ACCESS_KEY` (the MinIO user `zrelay`), `S3_RO_ACCESS_KEY` (the user `zview`), `RESTREAM_USER`, `RESTREAM_PASS`, `CAMERA_IP`, `CAMERA_USERNAME`, `CAMERA_PASSWORD`. Copy it from `.env.example`. | zrelay reads all of the file. zview-server gets only `S3_ENDPOINT` and `S3_RO_ACCESS_KEY`. |

`.env` does not contain `TAILSCALE_IP`. `deploy.sh` gets it from `tailscale ip -4`.

```sh
chmod 600 deploy/secrets/* deploy/.env
cp deploy/zrelay.example.yaml deploy/zrelay.yaml
cp deploy/zview.example.yaml  deploy/zview.yaml
```

Make these changes in `deploy/zrelay.yaml`:

* Set `relay.http_listen: "0.0.0.0:9464"`. This value applies in the container. Compose publishes
  the port only on `127.0.0.1` of the host. Thus the network cannot get to the metrics. If you run
  zrelay outside a container, keep the default `127.0.0.1:9464`. Never make the metrics port
  public.
* Keep `relay.shutdown_grace_seconds` less than `stop_grace_period` (40 s) in `compose.yaml`.
* Set the camera block (`host`, credentials, `rtsp.path`) for each camera.

Before you start the system, check the configuration and the memory budget:

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

`deploy.sh` gets `TAILSCALE_IP` from `tailscale ip -4`. zrelay publishes the RTSP restream
(8554/tcp) and the WebRTC ICE port (8189/udp) only on the Tailscale IP. zrelay does not publish
the live signaling port 9465. zview-server connects to it as `http://zrelay:9465` on the compose
network.

## 7. HTTPS for zview: `tailscale serve`

zview-server listens on `127.0.0.1:8080` of the host. Use Tailscale to put HTTPS in front of it:

```sh
tailscale serve --bg --https=443 http://127.0.0.1:8080
tailscale serve status
```

The web app, `/api/v1/*`, and `/live/*` all use this one origin (D18). Phones open
`https://<mac-mini>.<tailnet>.ts.net/`.

## 8. OrbStack: Local Network permission

The camera is on the home LAN. It is not on the tailnet. On macOS Sequoia and later, an
application must have the **Local Network** permission to connect to LAN addresses. The traffic of
the containers goes out through OrbStack. Thus OrbStack must have this permission:

1. Open System Settings > Privacy & Security > Local Network.
2. Turn on **OrbStack**.
3. If OrbStack is not in the list, quit OrbStack and start it again.
4. Run a container that connects to a LAN address. macOS then asks for the permission.
5. Restart zrelay: `docker compose -f deploy/compose.yaml restart zrelay`.

If the permission is missing, the RTSP session and the ONVIF subscription do not start. The
metrics show `zrelay_camera_rtsp_up 0`, and the logs show `no route to host`. But
`ping <camera-ip>` from the host shell works.

> **NOTE:** A macOS update can remove the permission. After an update, check this permission
> first.

## 9. Family onboarding (QR)

Each family member imports the master key into the web app one time. The key does not go out of
the browser. The family guide is [`docs/family-onboarding.md`](../docs/family-onboarding.md).

> **WARNING:** Do not send the key in a chat or an email. Show the QR code to the person directly.

1. Run `zrelay key generate --qr` (section 4). It shows the QR code for the new key, in the
   uppercase form that the scanner uses.
2. Show the QR code from that terminal when you make the key. Or keep the QR code in your
   password manager. The web app also accepts the `zk1-...` text.
3. On the phone, open the zview URL (section 7). Open the **Key** tab. Scan or paste the key.
4. Make sure that the fingerprint in the web app is the same as the output of
   `zrelay key fingerprint`.

## 10. Monitor zrelay

`/healthz` and `/metrics` are on `127.0.0.1:9464`.

* `/healthz` gives 200 when zrelay runs and the storage circuit breaker was not out of the closed
  state for more than 5 minutes. If not, it gives 503 and the reason. The backlog absorbs a
  shorter outage. A shorter outage must not restart the container.
* `/metrics` gives Prometheus text. Monitor these metrics:

| Metric | Meaning | Alert if |
|---|---|---|
| `zrelay_storage_breaker_state` | 0 closed, 1 half-open, 2 open | `== 2` for more than 2 minutes |
| `zrelay_backlog_bytes` | Encrypted data that waits for upload | More than 80% of `egress_backlog_mb` |
| `zrelay_clips_dropped_total{reason}` | Lost clips (`backlog`, `shutdown`, `stream_lost`) | The value increases |
| `zrelay_camera_rtsp_up`, `zrelay_camera_events_up` | Link status for each camera | `== 0` for more than 5 minutes |
| `zrelay_upload_errors_total{kind}` | Failed storage requests | The errors continue |
| `zrelay_gop_max_seconds` | The longest GOP | More than `max_gop_seconds` (pre-roll then uses more memory) |
| `zrelay_recorder_lag_total` | Recorder queue overflows | The value increases |
| `zrelay_live_viewers`, `zrelay_restream_clients` | Open live sessions | For information only |
| `process_resident_memory_bytes`, `go_goroutines` | Process size | The value increases over some days |

The logs are JSON on stdout (`docker compose -f deploy/compose.yaml logs -f zrelay`). The logs
never contain keys, credentials, or frame data.

## 11. Outage tolerance: how long can MinIO be down?

zrelay never writes to disk. When storage is not available, finished clips and clips in progress
stay in memory, in the egress backlog (`relay.egress_backlog_mb`). When the backlog is full, zrelay
drops the oldest finished clip. Coverage records it as `backlog`. Then zrelay stops the largest
clip in progress. During this time, the circuit breaker stops the upload attempts. It also checks
if storage is available again.

The approximate tolerance is:

```
minutes = egress_backlog_mb * 8 * 1.048576 / (combined_recording_mbps * 60)
```

(`egress_backlog_mb * 1.048576` changes MiB to megabytes. `* 8` changes megabytes to megabits.)
Examples:

| `egress_backlog_mb` | Recording | Tolerance |
|---|---|---|
| 512 | One camera at 3.2 Mbps, continuous recording | About 22 minutes |
| 512 | Three cameras at 3.2 Mbps each (9.6 Mbps), all with continuous recording | About 7 minutes |
| 128 | One camera at 4 Mbps, continuous recording | About 4.5 minutes |

Event clips come in bursts. Thus the real tolerance is usually much longer than the values for
continuous recording. `zrelay_backlog_bytes` shows how full the backlog is.

To increase the tolerance, increase `egress_backlog_mb` and `max_memory_mb` together.
`zrelay config check` shows the budget, which must be less than `max_memory_mb`. If a clip still
waits when zrelay stops (SIGTERM), zrelay drops the clip and counts it as `shutdown`. zrelay
cannot write the coverage object while storage is down.

## 12. Soak test (72 hours, manual)

Before you use an installation, run it for 72 hours with the real cameras, and record the process
size. The container has `read_only: true` and no volumes. Thus an attempt to write a file fails
and stops the process. Thus **zero restarts** also prove that nothing wrote to disk.

Start the system (section 6). Write down the start time. Then, on the host, record a sample every
15 minutes:

```sh
mkdir -p ~/zrelay-soak
cat > ~/zrelay-soak/sample.sh <<'SH'
#!/bin/sh
# Adds one line: UTC time, RSS bytes, goroutines, restart count, container start time.
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

During the test, make sure that motion occurs:

* Keep the motion detection of the camera on. Cause motion a few times each day.
* Keep one live view open on a phone for one hour or more each day.

After 72 hours (288 samples), stop the sampler and do these checks:

```sh
kill %1 2>/dev/null; pkill -f zrelay-soak/sample.sh
awk -F, 'NR>1 && $2!="NA"{if(min==""||$2<min)min=$2; if($2>max)max=$2} END{print "rss min/max MiB:", min/1048576, max/1048576}' ~/zrelay-soak/soak.csv
awk -F, 'NR>1 && $3!="NA"{if(min==""||$3<min)min=$3; if($3>max)max=$3} END{print "goroutines min/max:", min, max}' ~/zrelay-soak/soak.csv
cut -d, -f4,5 ~/zrelay-soak/soak.csv | sort -u
docker compose -f deploy/compose.yaml logs zrelay | grep -ci "read-only file system"
```

The test passes if all of these conditions are true:

* The restart count is `0`, and there is only one start time.
* The maximum RSS is less than `max_memory_mb`.
* RSS does not increase continuously. The median of the last 6 hours is within 10% of the median
  of hours 6 to 12.
* The goroutine count stays flat. The maximum of the last 6 hours is within 20 goroutines of the
  maximum of the first day.
* No log line contains `read-only file system`.
* `zrelay_clips_dropped_total` increases only during outages that you caused.

### Soak results

_Not run yet. The owner writes the results here after the 72-hour run._

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
| Result (pass / fail) and notes | |
