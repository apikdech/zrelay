# zrelay v2

zrelay records event clips from RTSP/ONVIF cameras, encrypts them end to end with the
`zrec` format, and uploads them to S3-compatible storage (MinIO). It also serves live
view (WebRTC/WHEP) and an RTSP restream to tailnet clients. zview is the browser viewer
that decrypts and plays the clips.

## Components

| Component | What it does | Where |
|---|---|---|
| `zrelay` | The recorder daemon. Holds one RTSP session and one ONVIF event subscription per camera. On motion/person events it cuts a clip (with pre-roll), muxes it to fMP4, encrypts it with the master key and uploads it to MinIO. If MinIO is down it queues clips in memory behind a circuit breaker and never writes to disk. It also serves live view (WHEP) and an RTSP restream. | `cmd/zrelay`, `internal/relay/` |
| `zview-server` | The viewer backend. Reads MinIO with read-only credentials, indexes clips by day, serves the web app and the `/api/v1/*` data API, and proxies live-view signaling to zrelay. It never sees plaintext video or keys. | `cmd/zview-server`, `internal/zview/` |
| Web app (zview) | Svelte PWA served by zview-server: key onboarding (paste or QR), timeline, playback, export and a live grid. All decryption happens in a browser Web Worker. | `web/` |
| `zdecrypt` | CLI that verifies and decrypts a `.zrec` clip to MP4, and re-wraps clips to a new key. | `cmd/zdecrypt` |
| MinIO | Not part of this repo: the S3 store on the NAS. zrelay can write but never delete archive objects; retention is a MinIO lifecycle rule. | `deploy/minio-*-policy.json` |

```
Camera ──RTSP + ONVIF──► zrelay ──encrypted clips (S3)──► MinIO
                           │  └─ live view (WebRTC), RTSP restream :8554
                           ▼
Browser ◄──HTTPS (tailscale serve)── zview-server ◄──read-only S3── MinIO
```

In production both services run as read-only Docker containers on one host (a Mac mini with
OrbStack), MinIO runs on the NAS, and everything talks over Tailscale. The full design is in
[`docs/superpowers/specs/2026-11-06-zrelay-v2-design.md`](docs/superpowers/specs/2026-11-06-zrelay-v2-design.md).

## Where your settings go

Configs never hold secrets directly. A value is either plain config, an `ENV:NAME` reference
(read from `deploy/.env`), or a `FILE:/path` reference (a file in `deploy/secrets/`, mounted at
`/run/secrets/` in the container). Literal secrets are rejected.

| Setting | Put it in | Referenced from |
|---|---|---|
| Camera IP, username, password | `deploy/.env`: `CAMERA_IP`, `CAMERA_USERNAME`, `CAMERA_PASSWORD` | `deploy/zrelay.yaml` → `cameras[].host`, `cameras[].credentials` |
| Camera RTSP port/path, ONVIF port, name, brand | `deploy/zrelay.yaml` → `cameras[].rtsp`, `cameras[].events` | (Tapo C220: port 554, `/stream1`; ONVIF port 2020) |
| More cameras | another entry under `cameras:` in `zrelay.yaml`, with its own `ENV:` names in `.env` | |
| S3 endpoint (MinIO URL) | `deploy/.env`: `S3_ENDPOINT` | `zrelay.yaml` and `zview.yaml` → `storage.s3.endpoint` |
| S3 bucket, region, root prefix | `zrelay.yaml` and `zview.yaml` → `storage.s3.bucket`, `.region`, `storage.root` | must match in both files and in the MinIO policy files |
| Relay S3 access key / secret key | `.env`: `S3_ACCESS_KEY` / file `deploy/secrets/s3_secret_key` | `zrelay.yaml` → `storage.s3` |
| zview read-only S3 access key / secret key | `.env`: `S3_RO_ACCESS_KEY` / file `deploy/secrets/s3_ro_secret_key` | `zview.yaml` → `storage.s3` |
| Master key (`zk1-…`) | file `deploy/secrets/zrelay_master` (from `zrelay key generate`) | `zrelay.yaml` → `encryption.master_key` |
| API token hash (live view and API auth) | file `deploy/secrets/api_token_sha256` (from `zrelay key api-token-hash`) | `zrelay.yaml` → `live.api_token_sha256`, `zview.yaml` → `server.api_token_sha256` |
| RTSP restream login | `.env`: `RESTREAM_USER`, `RESTREAM_PASS` | `zrelay.yaml` → `restream` |
| Tailscale IP | nothing: `deploy/deploy.sh` reads `tailscale ip -4` | `zrelay.yaml` → `live.advertise_ip`, compose ports |
| Pre/post-roll, max clip length, memory limits | `zrelay.yaml` → `defaults`, `relay` (per camera overrides allowed) | |
| Retention days | a MinIO lifecycle rule (runbook §3), plus `zview.yaml` → `index.retention_days` | |

Starting points: [`deploy/.env.example`](deploy/.env.example),
[`deploy/zrelay.example.yaml`](deploy/zrelay.example.yaml) and
[`deploy/zview.example.yaml`](deploy/zview.example.yaml). Copy them without the `.example` part.
`deploy/.env`, `deploy/zrelay.yaml`, `deploy/zview.yaml` and `deploy/secrets/` are git-ignored.

## Try it locally (no camera, no NAS)

`make demo` starts a fake Tapo camera (looping test video, plus motion and person events every
60 s) and writes a ready-to-run setup into `.demo/`: configs using the local storage driver, a
`.env`, and a throwaway master key. It prints the exact commands; in short:

```sh
make demo                                                            # terminal 1: fake camera
go run ./cmd/zrelay --env-file .demo/.env run --config .demo/zrelay.yaml   # terminal 2: recorder
(cd web && pnpm install && pnpm build)                               # once
go run ./cmd/zview-server --config .demo/zview.yaml                  # terminal 3: viewer
```

Open <http://127.0.0.1:8080> and import the demo key the first terminal printed. The first event
fires a minute after the demo starts, and its clip (5 s pre-roll, the 8 s event, 5 s post-roll)
shows on the Timeline a few seconds after the event ends. The Live
tab shows the camera, and VLC plays `rtsp://demo:demo@127.0.0.1:8554/demo-cam`. Clips land in
`.demo/archive/`; decrypt one by hand with
`go run ./cmd/zdecrypt --key FILE:.demo/secrets/zrelay_master --in <clip>.zrec --sidecar <clip>.json --out clip.mp4`.
The relay status dot updates every 60 s, so it can show the camera as down for the first minute.

The demo differs from production in two ways: storage is the local driver (production is
MinIO over S3; that path is covered by `make integration` against MinIO in Docker), and
everything is on `127.0.0.1` without Tailscale or HTTPS.

## Run it for real

Follow [`deploy/runbook.md`](deploy/runbook.md). In order:

1. MinIO on the NAS: create the bucket, the `zrelay` and `zview` users with the two policy
   files, and the retention rules (runbook §1–3).
2. Generate the master key and the API token hash into `deploy/secrets/` (§4). Keep the key in
   a password manager: lost key, lost footage.
3. Fill in `deploy/.env` from `deploy/.env.example`, the two S3 secret-key files, and
   `deploy/zrelay.yaml` / `deploy/zview.yaml` from the examples (§5). Check it with
   `zrelay config check` (prints the memory budget and the key fingerprint).
4. `deploy/deploy.sh` builds and starts both containers (§6); `tailscale serve` puts HTTPS in
   front of zview (§7). On macOS, allow OrbStack Local Network access so it can reach the
   camera (§8).
5. Onboard family phones with the QR code ([`docs/family-onboarding.md`](docs/family-onboarding.md)).

Before relying on it, work through [`docs/LIVE-CHECKS.md`](docs/LIVE-CHECKS.md): the checks that
need the real camera, NAS and phones. In particular, run `zrelay onvif discover` against your
camera to confirm the built-in Tapo event rules match what it actually sends.

## Development

Requires Go 1.27 and Docker. Node 26 and pnpm 12 are needed for `web/`.

| Command | What it runs |
|---|---|
| `make vet` | `go vet ./...` and staticcheck (pinned in `go.mod`, run with `go tool staticcheck`) |
| `make test` | `go test -race ./...` |
| `make integration` | `go test -race -tags integration ./...` (Docker fixtures such as MinIO) |
| `make chaos` | 10-minute storage-outage test with three synthetic cameras (about 15 min; not in CI) |
| `make vectors` | Regenerate the cross-language `zrec` vectors in `testdata/zrec/v1` |
| `make web` | Install, lint, type-check and unit-test the SPA in `web/` |
| `make web-e2e` | Playwright browser tests (Chromium and WebKit) |
| `make demo` | The local demo above |

`ffmpeg` and `ffprobe` are only ever run from a pinned Docker image, never from the host.

## Documentation

- Normative specs: [`docs/specs/`](docs/specs/), amended by the
  [v2 design](docs/superpowers/specs/2026-11-06-zrelay-v2-design.md) (the design wins on conflict).
- Operations: [`deploy/runbook.md`](deploy/runbook.md). Manual checks: [`docs/LIVE-CHECKS.md`](docs/LIVE-CHECKS.md).
- Agent and engineering rules: [`AGENTS.md`](AGENTS.md).
