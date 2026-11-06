# zrelay v2

zrelay records event clips from RTSP/ONVIF cameras, encrypts them end to end with the
`zrec` format, and uploads them to S3-compatible storage (MinIO). It also serves live
view (WebRTC/WHEP) and an RTSP restream to tailnet clients. zview is the browser viewer
that decrypts and plays the clips.

- Normative specs: [`docs/specs/`](docs/specs/), amended by the
  [v2 design](docs/superpowers/specs/2026-11-06-zrelay-v2-design.md).
- Agent and engineering rules: [`AGENTS.md`](AGENTS.md).
- Manual live checks: [`docs/LIVE-CHECKS.md`](docs/LIVE-CHECKS.md).

## Development

Requires Go 1.27 and Docker. Node 26 and pnpm 12 are needed for `web/`.

| Command | What it runs |
|---|---|
| `make vet` | `go vet ./...` and staticcheck (pinned in `go.mod`, run with `go tool staticcheck`) |
| `make test` | `go test -race ./...` |
| `make integration` | `go test -race -tags integration ./...` (Docker fixtures such as MinIO) |
| `make vectors` | Regenerate the cross-language `zrec` vectors in `testdata/zrec/v1` |
| `make web` | Install, lint and unit-test the SPA in `web/` |

`ffmpeg` and `ffprobe` are only ever run from a pinned Docker image, never from the host.
