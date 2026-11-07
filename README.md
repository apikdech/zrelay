# zrelay v2

zrelay records event clips from RTSP and ONVIF cameras. It encrypts each clip with the `zrec`
format and sends the clip to S3-compatible storage (MinIO). It also gives live video
(WebRTC/WHEP) and an RTSP restream to devices on your tailnet. zview is the web viewer. It
decrypts and plays the clips in the browser.

## Components

| Component | Function | Location |
|---|---|---|
| `zrelay` | The recorder. It keeps one RTSP session and one ONVIF event subscription for each camera. When a camera reports motion or a person, zrelay records a clip that starts before the event (pre-roll). It changes the clip to fMP4, encrypts it with the master key, and sends it to MinIO. If MinIO is not available, zrelay keeps the clips in memory and tries again later. zrelay never writes to disk. It also gives live video (WHEP) and an RTSP restream. | `cmd/zrelay`, `internal/relay/` |
| `zview-server` | The viewer server. It reads MinIO with read-only credentials and makes an index of the clips for each day. It serves the web app and the `/api/v1/*` data API. It sends live-video requests to zrelay. It never sees decrypted video or keys. | `cmd/zview-server`, `internal/zview/` |
| Web app (zview) | A Svelte PWA that zview-server serves. Use it to import the key (paste or QR code), see the timeline, play and export clips, and watch live video. A Web Worker in the browser decrypts all video. | `web/` |
| `zdecrypt` | A command-line tool. It checks a `.zrec` clip and decrypts it to MP4. It can also change the key of a clip (rewrap). | `cmd/zdecrypt` |
| MinIO | The S3 storage on the NAS. It is not in this repository. zrelay can write objects, but it cannot delete archive objects. A MinIO lifecycle rule removes old clips. | `deploy/minio-*-policy.json` |

```
Camera ──RTSP + ONVIF──► zrelay ──encrypted clips (S3)──► MinIO
                           │  └─ live video (WebRTC), RTSP restream :8554
                           ▼
Browser ◄──HTTPS (tailscale serve)── zview-server ◄──read-only S3── MinIO
```

In production, the two services run as read-only Docker containers on one host (a Mac mini
with OrbStack). MinIO runs on the NAS. All connections go through Tailscale. For the full design,
see [`docs/superpowers/specs/2026-11-06-zrelay-v2-design.md`](docs/superpowers/specs/2026-11-06-zrelay-v2-design.md).

## Where to put your settings

The configuration files do not contain secrets. Each value is one of these:

- A plain value.
- An `ENV:NAME` reference. The service reads the value from `deploy/.env`.
- A `FILE:/path` reference. The service reads the value from a file in `deploy/secrets/`. In the
  container, these files are in `/run/secrets/`.

zrelay and zview-server do not accept a secret as plain text in a configuration file.

| Setting | Where to put it | Which configuration reads it |
|---|---|---|
| Camera IP, user name, password | `deploy/.env`: `CAMERA_IP`, `CAMERA_USERNAME`, `CAMERA_PASSWORD` | `deploy/zrelay.yaml` → `cameras[].host`, `cameras[].credentials` |
| Camera RTSP port and path, ONVIF port, name, brand | `deploy/zrelay.yaml` → `cameras[].rtsp`, `cameras[].events` | For a Tapo C220: port 554, path `/stream1`, ONVIF port 2020 |
| More cameras | One more entry under `cameras:` in `zrelay.yaml` for each camera. Give each camera its own `ENV:` names in `.env`. | |
| S3 endpoint (MinIO URL) | `deploy/.env`: `S3_ENDPOINT` | `zrelay.yaml` and `zview.yaml` → `storage.s3.endpoint` |
| S3 bucket, region, root prefix | `zrelay.yaml` and `zview.yaml` → `storage.s3.bucket`, `storage.s3.region`, `storage.root` | The values must be the same in the two files and in the MinIO policy files. |
| Relay S3 access key and secret key | Access key: `.env` → `S3_ACCESS_KEY`. Secret key: the file `deploy/secrets/s3_secret_key`. | `zrelay.yaml` → `storage.s3` |
| zview read-only S3 access key and secret key | Access key: `.env` → `S3_RO_ACCESS_KEY`. Secret key: the file `deploy/secrets/s3_ro_secret_key`. | `zview.yaml` → `storage.s3` |
| Master key (`zk1-…`) | The file `deploy/secrets/zrelay_master`. `zrelay key generate` makes the key. | `zrelay.yaml` → `encryption.master_key` |
| API token hash (for live video and the API) | The file `deploy/secrets/api_token_sha256`. `zrelay key api-token-hash` makes the hash. | `zrelay.yaml` → `live.api_token_sha256`, `zview.yaml` → `server.api_token_sha256` |
| RTSP restream user name and password | `.env`: `RESTREAM_USER`, `RESTREAM_PASS` | `zrelay.yaml` → `restream` |
| Tailscale IP | Not necessary. `deploy/deploy.sh` gets it from `tailscale ip -4`. | `zrelay.yaml` → `live.advertise_ip`, the ports in `compose.yaml` |
| Pre-roll, post-roll, maximum clip length, memory limits | `zrelay.yaml` → `defaults` and `relay`. Each camera can have its own values. | |
| Retention days | A MinIO lifecycle rule (runbook section 3) and `zview.yaml` → `index.retention_days` | |

Use these files as a start: [`deploy/.env.example`](deploy/.env.example),
[`deploy/zrelay.example.yaml`](deploy/zrelay.example.yaml) and
[`deploy/zview.example.yaml`](deploy/zview.example.yaml). Copy each file and remove `.example`
from the name. Git ignores `deploy/.env`, `deploy/zrelay.yaml`, `deploy/zview.yaml`, and
`deploy/secrets/`. Thus you cannot commit them by mistake.

## Try it on your computer (no camera, no NAS)

`make demo` starts a simulated Tapo camera. The camera sends a test video in a loop. Every 60
seconds, it reports motion and a person. The demo also writes a full installation into `.demo/`:
configuration files for the local storage driver, a `.env` file, and a temporary master key.
Then it shows the commands that start zrelay and zview-server.

Do these steps:

1. In terminal 1, start the simulated camera: `make demo`
2. In terminal 2, start the recorder:
   `go run ./cmd/zrelay --env-file .demo/.env run --config .demo/zrelay.yaml`
3. Build the web app one time: `(cd web && pnpm install && pnpm build)`
4. In terminal 3, start the viewer: `go run ./cmd/zview-server --config .demo/zview.yaml`
5. Open <http://127.0.0.1:8080>.
6. Import the demo key. Terminal 1 shows the key.

The first event occurs one minute after the demo starts. Its clip has 5 s of pre-roll, the 8 s
event, and 5 s of post-roll. The clip shows on the Timeline a few seconds after the event stops.

You can also do these steps:

- Open the **Live** tab to see the camera.
- Play the restream in VLC: `rtsp://demo:demo@127.0.0.1:8554/demo-cam`
- Decrypt a clip manually. The clips are in `.demo/archive/`.
  `go run ./cmd/zdecrypt --key FILE:.demo/secrets/zrelay_master --in <clip>.zrec --sidecar <clip>.json --out clip.mp4`

> **NOTE:** The relay status updates every 60 seconds. For the first minute, the status can show
> that the camera is down.

The demo is different from production in two ways:

- The demo uses the local storage driver. Production uses MinIO through S3. `make integration`
  tests the S3 path with MinIO in Docker.
- The demo uses only `127.0.0.1`. It does not use Tailscale or HTTPS.

## Install the production system

For the full procedure, see [`deploy/runbook.md`](deploy/runbook.md). These are the main steps:

1. Prepare MinIO on the NAS (runbook sections 1 to 3). Make the bucket, the `zrelay` and `zview`
   users with their policy files, and the retention rules.
2. Make the master key and the API token hash in `deploy/secrets/` (section 4).

   > **WARNING:** Keep a copy of the master key in a password manager. If you lose the key, you
   > cannot decrypt the recordings.

3. Write `deploy/.env` from `deploy/.env.example` (section 5).
4. Write the two S3 secret-key files in `deploy/secrets/`.
5. Write `deploy/zrelay.yaml` and `deploy/zview.yaml` from the example files.
6. Check the configuration with `zrelay config check`. It shows the memory budget and the key
   fingerprint.
7. Start the two containers with `deploy/deploy.sh` (section 6).
8. Give zview an HTTPS address with `tailscale serve` (section 7).
9. On macOS, give OrbStack the Local Network permission (section 8). Without it, zrelay cannot
   connect to the camera.
10. Give the key to the family phones with the QR code (see
    [`docs/family-onboarding.md`](docs/family-onboarding.md)).

Before you use the system, do the checks in [`docs/LIVE-CHECKS.md`](docs/LIVE-CHECKS.md). These
checks need the real camera, the NAS, and the phones. First, run `zrelay onvif discover` on your
camera. It shows if the built-in Tapo rules agree with the events that your camera sends.

## Development

You need Go 1.27 and Docker. For `web/`, you also need Node 26 and pnpm 12.

| Command | What it does |
|---|---|
| `make vet` | Runs `go vet ./...` and staticcheck. `go.mod` pins staticcheck, and `go tool staticcheck` runs it. |
| `make test` | Runs `go test -race ./...`. |
| `make integration` | Runs `go test -race -tags integration ./...`. These tests start MinIO and other fixtures in Docker. |
| `make chaos` | Simulates a 10-minute storage outage with three test cameras. It takes about 15 minutes. CI does not run it. |
| `make vectors` | Makes the cross-language `zrec` test vectors in `testdata/zrec/v1` again. |
| `make web` | Installs, lints, type-checks, and tests the web app in `web/`. |
| `make web-e2e` | Runs the Playwright browser tests (Chromium and WebKit). |
| `make demo` | Starts the local demo (see "Try it on your computer"). |

The project runs `ffmpeg` and `ffprobe` only from a pinned Docker image. It never runs them on
the host.

## Documentation

- Specifications: [`docs/specs/`](docs/specs/). The
  [v2 design](docs/superpowers/specs/2026-11-06-zrelay-v2-design.md) changes them. If the two do
  not agree, the design is correct.
- Operation: [`deploy/runbook.md`](deploy/runbook.md). Manual checks:
  [`docs/LIVE-CHECKS.md`](docs/LIVE-CHECKS.md).
- Rules for engineers and agents: [`AGENTS.md`](AGENTS.md).
