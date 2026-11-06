# Playback clip fixtures

A real clip recorded by the relay pipeline (`app.Run`) into MinIO over the S3 multipart path, plus two derived variants, for the
playback engine tests (`web/e2e/playback.spec.ts`) and later end-to-end tests.

| Directory | What it is |
|---|---|
| `complete/` | The recorded clip: `zrec.sidecar/2` `status: complete`, 34.3 s, 640x360 H.264 (`avc1.640016`), 36 chunks of 64 KiB, one seek-index entry per 2 s GOP. |
| `tamper/` | `complete/` with one ciphertext bit flipped in chunk 18 (`manifest.json` `tamper_chunk`). Chunks 0-17 authenticate, chunk 18 fails with `ErrAuth`. |
| `incomplete/` | `complete/` cut in the middle of its final chunk, with the sidecar of a recovered clip (Contract §7.4): `status: incomplete`, `end_reason: recovered`, null `end_time`, `duration_ms`, `plaintext_size_bytes` and `seek_index`, empty `triggers`. The reader reports `ErrTruncated` on the last chunk. |

All three share one `clip_id`, camera (`front-door`) and key, so each is a separate storage
tree. The trees are laid out like a `local` driver `base_dir` (the committed S3 objects, copied out with GETs), which is what `zview-server` serves in tests: point `zview-server` at e.g.
`testdata/clips/complete` with `storage.root: zrelay`. (Copy it first; do not serve the
checked-in tree directly from a test.)

`manifest.json` holds the master key (`master_key`, the Contract §4.5 vector key `00..1f`, so
`k_api_sha256` is `testdata/zrec/v1/vectors.json` `derived.k_api_sha256_hex`), the clip id,
`tamper_chunk` and `tamper_fail_at_ms` (media time of the last fragment starting at or before the
tampered chunk). This key protects only these synthetic frames and is public.

The picture is the synthetic `testsrc2` pattern of `testdata/video/h264-640x360-gop2-10s.h264`
looped, never real footage.

## Regenerate

```sh
ZRELAY_FIXTURES_OUT=$PWD/testdata/clips \
  go test -tags fixtures -run TestGenerateClipFixtures -count=1 -timeout 5m ./internal/relay/app
```

The generator is `internal/relay/app/fixtures_gen_test.go` (build tag `fixtures`, so it is not part
of `make test`). It starts the in-process RTSP test server looping the H.264 fixture and the fake
ONVIF camera and a throwaway MinIO container (`storagetest.StartMinIO`, so **Docker must be
running**), runs the real `app.Run` with `storage.driver: s3` against it, holds a motion trigger
for 33 s, then reads the committed video and sidecar objects back with S3 GETs, writes them under
`<variant>/zrelay/clips/...` (the `local` driver's layout) and derives the two variants from those
bytes. It takes about 35 s plus the MinIO image pull on a first run. Regenerating changes the clip id and start time, so commit `manifest.json` and all
three trees together.

Verify a variant with the decryptor (the `ZK` value is `master_key` from `manifest.json`):

```sh
export ZK=zk1-000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f-2418
go run ./cmd/zdecrypt --key ENV:ZK --in testdata/clips/complete/zrelay/clips/front-door/<day>/<name>.zrec --out clip.mp4
```

`tamper/` exits 2 with `chunk authentication failed: chunk 18`; `incomplete/` exits 2 unless
`--allow-incomplete` is given, then writes the verified prefix.
