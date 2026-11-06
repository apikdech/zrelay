# Manual live checks

Steps that agents could not run: they need the real Tapo C220, the Synology MinIO, phones,
a 72-hour soak, or a deployed URL. Each entry lists the task, exact commands, the expected
result, and what to update if the result differs. Nothing here has been run yet.

## Task 6 — Access units and the RTSP session

**Automated in this task:** `internal/relay/camera` (`RunRTSP`, access-unit assembly, `Redact`) is
tested against the in-process test RTSP server (`testutil.StartRTSPServer`) with the synthetic
H.264 and H.265 fixtures: first AU is IDR, ≥ 140 AUs over the 10 s fixture with strictly increasing
DTS, `ParamGen` bump and a second `Codec` call on an SPS change, monotonic DTS across an RTP
timestamp wrap (`StartRTPTimestamp = 0xFFFF0000`), `time_source` `rtcp` with sender reports and
`arrival` without, credential redaction, H.265, PCMA audio, AUs before the first IDR dropped, and
return on session loss. All pass under `make vet test`. An opt-in live test,
`TestLiveCameraSession`, was added; it skips unless `ZRELAY_LIVE_RTSP_URL` is set and was not run
against a camera.

**Manual steps:**
1. On a host on the camera's network, from the repository root, enter the camera credentials
   without echoing them: `read -s ZRELAY_LIVE_RTSP_URL` and type
   `rtsp://<user>:<password>@<tapo-ip>:554/stream1`, then `export ZRELAY_LIVE_RTSP_URL`.
2. Run: `ZRELAY_LIVE_RTSP_SECONDS=60 go test -race -count=1 -run TestLiveCameraSession -v ./internal/relay/camera/`
3. Repeat step 2 with `stream2` (sub-stream) in the URL.
4. Run once more with a wrong password and confirm the failure message shows no password or
   `user:password` text.
5. `unset ZRELAY_LIVE_RTSP_URL`.

**Expected result:** the test passes; the log shows exactly one `Codec` line with `kind=h264`
(or `h265` if the camera is set to H.265), the configured resolution (e.g. 2560x1440 for
`stream1` on a C220), and `time_source=rtcp` if the camera sends RTCP sender reports; AU count
about fps × 60; max GOP at or below the camera's I-frame interval; wall drift vs media within
±1 s; audio codec `PCMA` (or `PCMU`) with a non-zero audio packet count when the camera's
microphone is on. Step 4 fails with a message containing `***` or no credentials at all.

**If the result differs, update:**
- `time_source=arrival` on a camera that should send RTCP, or a 2 s delay before the first
  video: `timeSourceWait` in `internal/relay/camera/au.go` (raise it if the first sender report
  arrives later than 2 s of media time) and `TestWallClockUsesRTCPWhenPresent`.
- More than one `Codec` line without a real parameter-set change (e.g. the camera rotates SEI or
  re-sends a byte-different but equivalent SPS): `assembler.inBandParams` in `au.go`, design
  §4.1 (parameter-set generation) and `TestParamGenBumpsOnSPSChange`.
- No audio packets with the microphone on: `setupAudio` in `internal/relay/camera/rtsp.go`
  (the camera may advertise a sample rate or channel count other than 8 kHz mono).
- Session errors or timeouts: `rtspTimeout` in `rtsp.go` and design §4.1.
- RTP wrap (every ~13.25 h at 90 kHz) is only exercised by the long soak; a DTS discontinuity
  there points at `unwrapper` in `au.go` and `TestAccessUnitsMonotonicAcrossRTPWrap`.

**Status:** not run

## Task 6 (review fixes) — Access units and the RTSP session

**Automated in this task:** Fixes after review, all under `make vet test`:
`TestVideoPathStampsAccessUnits` packetizes both fixtures with gortsplib's `rtph264`/`rtph265`
encoders (payload limits 1 MiB, 4000 and 1450 bytes, so every AU fits in one RTP packet, some
do, or none do; plus H.264 with the marker bit cleared) and feeds them through the same
depacketize-and-assemble path as `RunRTSP`; every AU arrives with PTS = DTS = index × 6000 and
the arrival-mode Wall. Before the fix the single-packet case delivered 121 of 150 AUs and the
4000-byte case 90 of 150. `TestCodecAnnouncedWhenChangingAUIsDropped` (a parameter-set change on
a dropped AU is still announced by `Codec` before the first AU of the new `ParamGen`),
`TestWallClockRTCPNeverStepsBack` (Wall never steps back when the sender-report mapping does),
`TestRedactUsernameOnly`, `TestRedactKeepsErrorsIs`, `TestRunRTSPInvalidURL`, `TestSelectVideo`
(errNoVideo) and `TestSelectAudio` (PCMU) pass. None of this ran against a camera.

**Manual steps:**
1. On a host on the camera's network, from the repository root: `read -s ZRELAY_LIVE_RTSP_URL`,
   type `rtsp://<user>:<password>@<tapo-ip>:554/stream2` (the sub-stream, whose small P-frames
   fit in one RTP packet), then `export ZRELAY_LIVE_RTSP_URL`.
2. Point the camera at a static scene (a wall), so P-frames are small.
3. Run: `ZRELAY_LIVE_RTSP_SECONDS=120 go test -race -count=1 -run TestLiveCameraSession -v ./internal/relay/camera/`
4. Repeat step 3 with `stream1`.
5. `unset ZRELAY_LIVE_RTSP_URL`.

**Expected result:** for both streams the AU count is within 2 % of fps × 120 (no GOPs lost to
resynchronisation), exactly one `Codec` line, and the logged wall drift vs media stays within
±1 s.

**If the result differs, update:**
- AU count well below fps × 120 with single-packet AUs: `h264Depacketizer` /
  `markerDepacketizer` in `internal/relay/camera/rtsp.go` (the gortsplib decoder's timestamp
  behaviour may differ from the v5.6.7 model they mirror) and `TestVideoPathStampsAccessUnits`.
- Wall drifting ahead of the camera clock after an NTP step on the camera: the Wall floor in
  `assembler.emit` (`internal/relay/camera/au.go`) and `TestWallClockRTCPNeverStepsBack`.

**Status:** not run

## Task 4 — Memory budget + `zrelay config check` + container images

**Automated in this task:** `config.Budget` / `BudgetReport.WriteTable` (design §8.1 formula,
`fps_est = 15`, `LivePerViewerBytes = 2 MiB`, runtime 32 MiB, limit 80% of `max_memory_mb`) and the
`cmd/zrelay` dispatcher (`config check --config`, global and per-subcommand `--env-file`; exit 0 OK,
1 budget over 80%, 2 usage error or invalid config) are covered by `TestBudgetWorkedExample`
(Total 679 131 231 bytes: over at 768, OK at 1024), `TestConfigCheckExitCodes` and the env-file,
secret-leak and usage tests; all pass under `make vet test`. `docker build -f deploy/Dockerfile.zrelay .`
succeeded (scratch image, about 5 MB, `User 65534:65534`, entrypoint
`["/zrelay","run","--config=/etc/zrelay/zrelay.yaml"]`). With the example config and dummy
secrets mounted read-only, `docker run --rm --read-only --env-file <dummy.env> -v
<example>:/etc/zrelay/zrelay.yaml:ro -v <dummy-secrets>:/run/secrets:ro --entrypoint /zrelay
<img> config check --config /etc/zrelay/zrelay.yaml` printed the table (TOTAL 620 241 378 bytes,
591.5 MiB, limit 614.4 MiB, OK) and exited 0. `docker compose -f deploy/compose.yaml config` was
rendered against dummy inputs (read_only, no volumes or tmpfs, secrets at `/run/secrets`, ports
`${TAILSCALE_IP}:8554/tcp`, `${TAILSCALE_IP}:8189/udp`, `127.0.0.1:9464`, `ulimits core 0`).
`zrelay run` does not exist yet (a later task), so `deploy.sh` could not start a running daemon,
and nothing was run on the Mac mini or against Tailscale.

**Manual steps:**
1. On the Mac mini, from the repository root: `cp deploy/zrelay.example.yaml deploy/zrelay.yaml`
   and set `relay.http_listen: "0.0.0.0:9464"` in `deploy/zrelay.yaml` (inside the container; the
   host publish stays `127.0.0.1:9464`).
2. Create `deploy/.env` with `S3_ENDPOINT`, `S3_ACCESS_KEY`, `RESTREAM_USER`, `RESTREAM_PASS`,
   `CAMERA_IP`, `CAMERA_USERNAME`, `CAMERA_PASSWORD`, and `mkdir -m 700 deploy/secrets` holding
   `zrelay_master`, `s3_secret_key` and `api_token_sha256`, each `chmod 600`.
3. `export TAILSCALE_IP=$(tailscale ip -4 | head -n 1)`, then
   `docker compose -f deploy/compose.yaml build` and
   `docker compose -f deploy/compose.yaml run --rm --no-deps --entrypoint /zrelay zrelay config check --config /etc/zrelay/zrelay.yaml; echo "exit $?"`
4. Once `zrelay run` exists: `unset TAILSCALE_IP; ./deploy/deploy.sh`, then
   `docker inspect zrelay-zrelay-1 --format '{{.HostConfig.ReadonlyRootfs}} {{json .Mounts}} {{json .HostConfig.PortBindings}}'`
   and `curl -s http://127.0.0.1:9464/healthz`; from another tailnet host
   `nc -vz <tailscale-ip> 9464` and `nc -vz <tailscale-ip> 8554`.

**Expected result:** step 3 prints the budget table with one `camera tapo-c220` row, `TOTAL`
591.5 MiB and `budget OK`, then `exit 0`, with no `WARN secret file mode` lines and no
"permission denied" on `/run/secrets/*` (the files are read as uid 65534). Step 4: `deploy.sh`
picks up the Tailscale IP by itself; `ReadonlyRootfs` is `true`; `Mounts` lists only the config and
the three secrets (bind, read-only) with no volume or tmpfs; ports bind 8554/tcp and 8189/udp on the
Tailscale IP and 9464 on `127.0.0.1` only; `/healthz` answers locally; 9464 is refused from the
other host while 8554 connects.

**If the result differs, update:**
- Secrets unreadable by uid 65534 under OrbStack (permission denied with `chmod 600`): the
  `secrets:` block in `deploy/compose.yaml` (bind-mounted secrets keep the host owner and mode;
  use the long syntax with `uid`/`gid`/`mode` if OrbStack honours it, or document the needed mode
  in `deploy/runbook.md`) and design §8.3.
- `config check` over budget with the real camera settings: the camera's `max_bitrate_kbps` /
  `max_gop_seconds` or `relay.max_memory_mb` in `deploy/zrelay.yaml`; if the queue term is the
  cause, that is the Task 11 revision of `budgetFPSEstimate` / the queue formula in
  `internal/relay/config/budget.go`, design §8.1 and `TestBudgetWorkedExample`.
- `/metrics` unreachable through `127.0.0.1:9464`: the `http_listen` note at the top of
  `deploy/compose.yaml` and the runbook.
- `tailscale` not on `PATH` on macOS: `deploy/deploy.sh` (use
  `/Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4`).

**Status:** not run
