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
## Task 8 — ONVIF events — rules, tracker, PullPoint client

**Automated in this task:** the Tapo rules, the trigger tracker and the PullPoint client run
against `testutil.StartFakeONVIF` (an in-process camera on ephemeral 127.0.0.1 ports) with the
fake clock: `TestTapoRules`, `TestInitializedActiveSetsStateWithoutNewChange`,
`TestPullTransportErrorRetriesSameSubscription` (5 dropped pulls, 5 `PullRetry`, one
subscription), `TestRenewEvery60s`, `TestFailedRenewResubscribesAfterUnsubscribe`,
`TestResubscribeBackoff` (5, 10, 20, 40, 60, 60 s), `TestNon200PullIsFailure`,
`TestSubscriptionAddressHostRewritten`, `TestRedirectRefused`, `TestDigestUsesCameraClockOffset`,
`TestClockResyncEvery30Minutes`, `TestConnectionCloseHeader`,
`TestReadyFalseAndKindsResetWhenSubscriptionEnds`, `TestUnmatchedTopicCounted` and
`TestResponseCappedAt1MiB` pass with `-race`. The fake mimics Tapo's dropped `PullMessages`
connections, but none of this ran against a real camera.
`TestLivePullPoint` is skipped unless `ZRELAY_LIVE_ONVIF_HOST` is set.

**Manual steps:**
1. On a host on the camera's network, from the repository root:
   `export ZRELAY_LIVE_ONVIF_HOST=<tapo-ip> ZRELAY_LIVE_ONVIF_USER=<camera-account>` and
   `read -s ZRELAY_LIVE_ONVIF_PASSWORD; export ZRELAY_LIVE_ONVIF_PASSWORD`.
2. Run: `ZRELAY_LIVE_ONVIF_SECONDS=150 go test -race -count=1 -run TestLivePullPoint -v ./internal/relay/events/`
3. During the 150 s, walk in front of the C220 (person), then stay out of view; if you can,
   drive a car or pet through the scene, and trigger a Tapo smart event such as baby cry.
4. Read the `signal`, `ready`, `pull retries`, `clock skew` and `unmatched` log lines.
5. `unset ZRELAY_LIVE_ONVIF_HOST ZRELAY_LIVE_ONVIF_USER ZRELAY_LIVE_ONVIF_PASSWORD`.

**Expected result:** `ready=true` once and `ready=false` only at shutdown (Renew every 60 s kept
one subscription alive for 150 s, so the C220 supports Renew); `signal kind=motion active=true`
then `active=false` when the scene settles; `kind=person` for the walk-through; a non-zero
`pull retries` count (Tapo drops idle pulls) with no resubscribe; `clock skew` small; `unmatched`
lists only topics the Tapo rules do not cover.

**If the result differs, update:**
- `ready=false` before shutdown, or a failed Renew: `renewEvery` and `renewBody` in
  `internal/relay/events/onvif.go`, and design §4.3 / relay-spec §4.4 (Renew-as-health).
- Wrong or missing kinds, or `IsVehicle` / the catch-all not seen: `TapoRules()` in
  `internal/relay/events/rules.go`, `TestTapoRules`, and the design §4.3 built-in table.
- Authentication failures: `buildEnvelope` in `internal/relay/events/soap.go` and
  `TestDigestUsesCameraClockOffset`.
- Unexpected topics in `unmatched`: add rules via `TapoRules()` and the table in design §4.3.
## Task 19 — `s3` recoverer

**Automated in this task:** `pkg/storage/s3/recover.go` (`ListIncomplete`, `RecoverCommit`,
`AbortIncomplete`). Against MinIO in Docker (`storagetest.MinIOImage`, `make integration`):
`TestS3Recoverer` (the `storagetest.RunRecoverer` suite, plus a crash after 2 full 5 MiB parts and a
1 MiB+123 partial buffer recovering exactly the first 10 MiB), `TestRecoverZeroPartsAborts`,
`TestListIncompleteRespectsAge` (`olderThan` 0 vs 1h) and `TestAbortIncompleteRemovesUpload` all
pass. Unit tests against an in-process fake (`make test`) cover paginated ListMultipartUploads and
ListParts, element-wise prefix and root filtering, the Initiated age filter with server clock skew,
gapped part numbers, a lost CompleteMultipartUpload response followed by a retried `RecoverCommit`,
a gone upload, zero parts, malformed candidates and a truncated listing whose markers do not
advance (an error, not a loop). A probe against the pinned MinIO showed that MinIO's
ListMultipartUploads honours only an empty prefix or an exact object name (a directory prefix such
as `<root>/clips/cam/` lists nothing), and that an empty-prefix listing is served from an in-memory
cache of uploads started since the server process began, so it returns nothing after a MinIO
restart. `ListIncomplete` therefore derives candidate keys from the staging sidecars
(`.<base>.json.part` maps to `<base>.zrec`, listed with ListObjectsV2 including dot-names), lists
uploads by each exact key, and unions that with the empty-prefix listing, deduplicated by upload id.
`TestListIncompleteSurvivesMinIORestart` (`make integration`, via
`storagetest.StartRestartableMinIO` and `MinIO.Restart`) crashes a writer after 2 parts, restarts
the container, and asserts the upload is listed and recovered as exactly those 2 parts; it failed
before the fix. The fake models the cache (`fakeRecoverS3.restart`). The Synology MinIO was not
reachable.

**Manual steps:**
1. On a host on the tailnet, set the relay's S3 credentials without echoing them:
   `read -s AWS_ACCESS_KEY_ID; read -s AWS_SECRET_ACCESS_KEY; export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY`,
   and set `EP=https://<synology-minio-host>:<port>`, `B=<bucket>`, `R=<root>` (the values from
   `zrelay.yaml`).
2. Start an upload with the relay credentials:
   `docker run --rm -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY amazon/aws-cli:2.22.0 --endpoint-url "$EP" --region us-east-1 s3api create-multipart-upload --bucket "$B" --key "$R/clips/livecheck/2026/11/06/000000000_01JC0000000000000000000000.zrec"`
   and note the `UploadId`.
3. List with no prefix (what the driver sends):
   `docker run --rm -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY amazon/aws-cli:2.22.0 --endpoint-url "$EP" --region us-east-1 s3api list-multipart-uploads --bucket "$B"`
4. List with a directory prefix, to record the server's prefix behaviour:
   the same command with `--prefix "$R/clips/livecheck/"`.
5. List the (empty) parts:
   `... s3api list-parts --bucket "$B" --key "$R/clips/livecheck/2026/11/06/000000000_01JC0000000000000000000000.zrec" --upload-id <UploadId>`
6. Abort it:
   `... s3api abort-multipart-upload --bucket "$B" --key "$R/clips/livecheck/2026/11/06/000000000_01JC0000000000000000000000.zrec" --upload-id <UploadId>`,
   then repeat step 3.
7. Restart check. Repeat step 2 (note the new `UploadId`), then restart the Synology MinIO (Container
   Manager, or `docker restart <minio-container>` over SSH) and wait until it answers:
   `docker run --rm -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY amazon/aws-cli:2.22.0 --endpoint-url "$EP" --region us-east-1 s3api head-bucket --bucket "$B"`.
8. List with no prefix, then by the exact object name:
   `... s3api list-multipart-uploads --bucket "$B"` and
   `... s3api list-multipart-uploads --bucket "$B" --prefix "$R/clips/livecheck/2026/11/06/000000000_01JC0000000000000000000000.zrec"`.
9. Clean up: abort the upload from step 7 as in step 6, then
   `unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY`.

**Expected result:** steps 2, 3, 5 and 6 succeed under the relay policy (design §6.5); step 3 lists
the upload with an `Initiated` time in UTC within a few seconds of the host clock; step 5 returns
no parts; after step 6, step 3 no longer lists it. Step 4 may list nothing (MinIO's exact-name
prefix behaviour); that is expected. After the restart, step 8's empty-prefix listing may be empty
(MinIO's cache is gone) but the exact-name listing MUST show the upload from step 7 with its
original `Initiated` time; that exact-key lookup is what `ListIncomplete` relies on through each
staging sidecar.

**If the result differs, update:**
- Step 3 denied (AccessDenied) while step 4 is allowed: the relay MinIO policy restricts
  `s3:ListBucketMultipartUploads` by an `s3:prefix` condition. Grant it on the bucket without a
  prefix condition in the runbook policy (design §6.5), or, if the server matches byte prefixes,
  send `Prefix: <root>/` in `ListIncomplete` (`pkg/storage/s3/recover.go`) and change the fake's
  prefix rule in `fakeRecoverS3.listUploads` (`pkg/storage/s3/recover_test.go`) to match.
- Step 8's exact-name listing is empty after the restart (the Synology MinIO build does not keep
  uploads across restarts, or scopes them differently): the sidecar-derived lookup in
  `ListIncomplete` (`stagedFinalKeys`, `pkg/storage/s3/recover.go`) cannot find crash leftovers;
  record the behaviour, then change the lookup, `fakeRecoverS3.listUploads`/`restart`
  (`pkg/storage/s3/recover_test.go`), `TestListIncompleteSurvivesMinIORestart`, and design §6.5 /
  Contract §7.4 recovery wording to match.
- `Initiated` far from the host clock: the age filter in `ListIncomplete` trusts the server clock
  against `time.Now()`; fix NTP on the NAS, or widen the 2-minute candidate age in the caller
  (Contract §7.4, design §6.5).
- Step 5 or 6 denied: add `s3:ListMultipartUploadParts` / `s3:AbortMultipartUpload` to the relay
  policy (design §6.5).

**Status:** not run

## Task 27 — Startup crash recovery

**Automated in this task:** `internal/relay/recovery` runs every row of Contract §7.4's two tables
(both present, upload without staging, staging with committed video and no sidecar, staging with a
real sidecar, staging alone, descriptor mismatch, empty/unreadable descriptor, younger than the age
threshold, scope of days and cameras, idempotent second run) against the `local` driver and, under
`-tags integration`, against a throwaway MinIO container. `storage.Recoverer.ListStaging` is covered
by a new `storagetest.RunRecoverer` case for both drivers and by a fake-S3 unit test. Not covered
automatically: the Synology MinIO build, the relay's real S3 policy (design §6.5) and a real
`kill -9` of `zrelay` mid-clip (recording is wired in later tasks).

**Manual steps:**
1. On the Mac mini with the relay running against the Synology MinIO, start a clip (trigger motion on
   the Tapo C220) and wait until the log shows the clip's staging sidecar was written and at least one
   8 MiB part uploaded (more than ~10 s of footage at the configured bitrate).
2. `docker kill -s KILL zrelay` (SIGKILL; no shutdown path runs).
3. With the relay credentials, list the leftovers:
   `AWS_ACCESS_KEY_ID=<relay-key> AWS_SECRET_ACCESS_KEY=<relay-secret> aws --endpoint-url http://<nas>:9000 s3api list-multipart-uploads --bucket <bucket> --prefix <root>/clips/<cam>/`
   and `aws ... s3 ls s3://<bucket>/<root>/clips/<cam>/<YYYY>/<MM>/<DD>/` (the dot-named
   `.<HHMMSSmmm>_<ULID>.json.part` must be listed; no `.json` yet).
4. Wait more than 2 minutes, then `docker start zrelay` and wait for the recovery log line.
5. Repeat step 3 and fetch the new sidecar:
   `aws ... s3 cp s3://<bucket>/<root>/clips/<cam>/<YYYY>/<MM>/<DD>/<HHMMSSmmm>_<ULID>.json -`
6. Open the clip in zview and play it.

**Expected result:** after step 4 there is no multipart upload and no `.json.part` left for the clip;
the `.zrec` exists with the size of the uploaded parts; the `.json` sidecar has `"status":"incomplete"`,
`"end_reason":"recovered"`, `end_time`, `duration_ms`, `video.plaintext_size_bytes` and
`video.seek_index` all `null`, `"triggers":[]`, and `video.size_bytes` equal to the object size. zview
lists the clip as incomplete and plays up to the truncation point. The relay's S3 policy allowed every
call, including `DeleteObject` on the dot-named staging key.

**If the result differs, update:**
- `AccessDenied` on deleting the staging sidecar or listing multipart uploads: the relay policy in the
  runbook and design §6.5 (`s3:DeleteObject` on `<root>/clips/*/.*`, `s3:ListBucketMultipartUploads`).
- The upload is not found after a restart (MinIO cache behaviour): `ListIncomplete`
  (`pkg/storage/s3/recover.go`) and Task 19's live check.
- The staging sidecar lacks `schema` or `recorder.time_source`, so recovery counts the clip as orphaned:
  the step-0 writer (Task 25/26) or the staging requirements in `buildSidecar`
  (`internal/relay/recovery/recovery.go`) and Contract §7.3 step 0.
- Server timestamps far from the host clock: the staging age check in `recovery.scan` and the 2-minute
  age (Contract §7.4).

**Status:** not run
