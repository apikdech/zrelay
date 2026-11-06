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
   Since Task 10b, `config check` parses the master key, so `zrelay_master` must hold a valid
   `zk1-` key, not a dummy string. Create it from the `key:` line of
   `docker compose -f deploy/compose.yaml run --rm --no-deps --entrypoint /zrelay zrelay key generate`,
   written as the bare `zk1-...` text only (no `key:` label, no QR), e.g.
   `printf '%s\n' 'zk1-...' > deploy/secrets/zrelay_master && chmod 600 deploy/secrets/zrelay_master`.
3. `export TAILSCALE_IP=$(tailscale ip -4 | head -n 1)`, then
   `docker compose -f deploy/compose.yaml build` and
   `docker compose -f deploy/compose.yaml run --rm --no-deps --entrypoint /zrelay zrelay config check --config /etc/zrelay/zrelay.yaml; echo "exit $?"`
4. Once `zrelay run` exists: `unset TAILSCALE_IP; ./deploy/deploy.sh`, then
   `docker inspect zrelay-zrelay-1 --format '{{.HostConfig.ReadonlyRootfs}} {{json .Mounts}} {{json .HostConfig.PortBindings}}'`
   and `curl -s http://127.0.0.1:9464/healthz`; from another tailnet host
   `nc -vz <tailscale-ip> 9464` and `nc -vz <tailscale-ip> 8554`.

**Expected result:** step 3 prints the budget table with one `camera tapo-c220` row, `TOTAL`
591.5 MiB and `budget OK`, a `key fingerprint: <xxxx-xxxx-xxxx-xxxx>` line equal to the output of
`docker compose -f deploy/compose.yaml run --rm --no-deps --entrypoint /zrelay zrelay key fingerprint --key FILE:/run/secrets/zrelay_master`
(and to the `fingerprint:` line printed by `key generate`), then `exit 0`, with no `WARN secret file mode` lines and no
"permission denied" on `/run/secrets/*` (the files are read as uid 65534). Step 4: `deploy.sh`
picks up the Tailscale IP by itself; `ReadonlyRootfs` is `true`; `Mounts` lists only the config and
the three secrets (bind, read-only) with no volume or tmpfs; ports bind 8554/tcp and 8189/udp on the
Tailscale IP and 9464 on `127.0.0.1` only; `/healthz` answers locally; 9464 is refused from the
other host while 8554 connects.

**If the result differs, update:**
- Fingerprint line missing or different from `key fingerprint`, or `config check` exits 2 with
  `encryption.master_key: ...` on a key made by `key generate`: the fingerprint code in
  `cmd/zrelay/main.go` / `internal/relay/config/budget.go`, `zrec.ParseMasterKey` (a trailing
  newline or CRLF in the secret file), and `TestConfigCheckPrintsKeyFingerprint` /
  `TestKeyFingerprint`; if the secret file was written with a `key:` label or QR block, fix this
  step instead.
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

## Task 9 — Supervisor, camera states, `onvif discover`, and phase-1 live check

**Automated in this task:** `camera.Supervise` runs against `testutil.StartRTSPServer` and
`testutil.StartFakeONVIF` (ephemeral 127.0.0.1 ports; the fake gained `EventTopic` and a
`GetEventProperties` reply). With `-race`: `TestSupervisorStates` (connecting, priming, ready;
`Disconnect()` gives degraded, then connecting, priming, ready again), `TestBackoffFullJitterBounded`
(delays within [0, min(60 s, 1 s·2^n)], reset after 5 min), `TestEventsDownKeepsVideoAndMarksDegraded`
(every `CreatePullPointSubscription` faults: `degraded`, `Events=="down"`, a Hub viewer still gets
video), `TestOneCameraFailureIsolated`, `TestTriggerTimeoutExpires`, and `TestDiscoverPrintsTopicsAndSuggestsRules`,
`TestDiscoverFlagsTopicsNoRuleCovers`, `TestDiscoverSurvivesMissingEventProperties`, `TestOnvifDiscover`
(CLI, against the fake). The test-only probe `internal/testutil/cmd/camprobe` (supervisor only; logs
state, AU rate, goroutines and RSS every `--interval`, default 5 min; not shipped) was smoke-run for
2 minutes against the test RTSP server and the fake ONVIF camera:
`ZRELAY_CAMPROBE_SMOKE=2m go test -count=1 -timeout 10m -run LogsStates -v ./internal/testutil/cmd/camprobe/`.
Result: `state=ready` from second 2 on, `au_per_s=15.0` on every line, `goroutines=20` on every line,
RSS 21.6 MiB at 10 s, 23.5 MiB at 2 min (growth levelled off at about 23.4 MiB). This proves the
probe and supervisor run; it says nothing about the real C220, and 2 minutes cannot show a leak.

**Manual steps:** (host on the camera's network; repository root)
1. Make a config that resolves on this host. `deploy/zrelay.example.yaml` points its `FILE:` secrets
   at `/run/secrets`, which does not exist here:
   `D=$(mktemp -d) && printf 'zk1-example-master-key\n' > $D/zrelay_master && printf 's3-secret\n' > $D/s3_secret_key && printf 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n' > $D/api_token_sha256 && sed "s#FILE:/run/secrets/#FILE:$D/#" deploy/zrelay.example.yaml > $D/zrelay.yaml`
2. Write `.env` (never commit it) with the real `CAMERA_IP`, `CAMERA_USERNAME` and `CAMERA_PASSWORD`,
   plus placeholder values the example config also resolves: `S3_ENDPOINT=https://nas.example.ts.net:9000`,
   `S3_ACCESS_KEY=x`, `TAILSCALE_IP=100.64.0.7`, `RESTREAM_USER=x`, `RESTREAM_PASS=x`.
3. Check the config: `go run ./cmd/zrelay --env-file .env config check --config $D/zrelay.yaml`.
4. Discover, walking in front of the camera (person), then standing still, then (if possible) a pet or a
   vehicle in view, and one smart event such as baby cry or tamper:
   `mkdir -p docs/runbook-notes && go run ./cmd/zrelay --env-file .env onvif discover --config $D/zrelay.yaml --camera tapo-c220 --listen 120s | tee docs/runbook-notes/c220-onvif.md`
5. Record in `docs/runbook-notes/c220-onvif.md`, under the pasted output: the camera model and firmware
   version, every advertised topic and data item, every topic and item actually observed (with the values
   seen), whether `IsPeople`, `IsPet` and `IsVehicle` ever appeared, and the Summary lines (pull
   retries, resubscribes, clock skew). Read the file once and remove anything that looks like a
   credential before committing it.
6. Probe for one hour, with the camera idle or in normal use, and no other zrelay running against it:
   `go run ./internal/testutil/cmd/camprobe --env-file .env --config $D/zrelay.yaml --camera tapo-c220 --interval 5m --duration 1h | tee docs/runbook-notes/c220-camprobe.log`
   (add `--interval 30s` first for a quick look). Then `rm -rf $D`.

**Expected result:**
- Step 4: the `GetEventProperties` section lists `RuleEngine/CellMotionDetector/Motion` with `IsMotion`
  and `RuleEngine/TPSmartEventDetector/TPSmartEvent` with the smart items; the notifications section shows
  `IsMotion=true` while you walk, `IsPeople=true` for the person; "resubscribes" is 0 over the 120 s
  (Renew every 60 s worked); "pull retries" is non-zero (Tapo drops idle pulls); clock skew is small. The
  "Coverage" section says every observed topic is covered by the built-in Tapo rules, or names the ones that
  are not.
- Step 6: 12 stats lines, `state=ready rtsp=up events=up` on all of them (a brief `degraded` between lines
  is acceptable only if it recovers), `au_per_s` close to the camera's frame rate, `goroutines` constant
  (within ±2) from the first line to the last, and `rss_kib` flat: after the first 15 minutes it grows by
  less than about 5 MiB over the remaining 45 and shows no steady upward trend.

**If the result differs, update:**
- `IsVehicle` or `IsPet` never appear, or other items do (for example a differently named person item):
  `TapoRules()` in `internal/relay/events/rules.go` (confirm or delete the `IsVehicle` row and the
  catch-all), `TestTapoRules` in `internal/relay/events/rules_test.go` with one case per topic and item the
  camera actually emitted, and the built-in rules table in design §4.3
  (`docs/superpowers/specs/2026-11-06-zrelay-v2-design.md`) and the example comment in
  `deploy/zrelay.example.yaml`.
- Unexpected extra topics: add rules from the "Suggested rules" output to `TapoRules()` and design §4.3.
- "resubscribes" above 0, or a Renew fault in the logs: `renewEvery`/`renewBody` in
  `internal/relay/events/onvif.go`, design §4.3 and relay-spec §4.4 (Renew-as-health), and
  `TestRenewEvery60s`.
- `GetEventProperties failed` on the real camera: `getEventPropertiesBody`/`printProperties` in
  `internal/relay/events/discover.go` and the fake's `eventProperties` in `internal/testutil/fakeonvif.go`.
- Goroutines or RSS keep growing in step 6: look first at the Hub subscriber queues
  (`internal/relay/camera/hub.go`), the assembler (`au.go`), `RunRTSP` teardown (`rtsp.go`) and the
  supervisor loops (`supervisor.go`); add a regression test before fixing.
- `state` never reaches `ready`, or flaps: the priming/ready rules in `compute` (`supervisor.go`),
  design §4.2 and `TestSupervisorStates`.
## Task 10b — Key CLI subcommands and `config check` fingerprint

**Automated in this task:** `zrelay key generate [--qr]`, `key fingerprint --key REF` and
`key api-token-hash --key REF` (in `cmd/zrelay/key.go`) and the `key fingerprint:` line of
`config check` are covered by the `cmd/zrelay` tests (`TestKeyGenerate`,
`TestKeyGenerateQRUsesUppercaseForm`, `TestKeyFingerprint*`, `TestKeyAPITokenHash*`,
`TestKeyCLIRejectsLiteral`, `TestKeyCLIUnsupportedSchemeIsNotCalledLiteral`,
`TestKeyGenerateRandFailureExitsOne`, `TestConfigCheckPrintsKeyFingerprint`) and
`internal/relay/config` `TestKeyFingerprint`, all passing under `make vet test`. The QR output was
compared with `qrterminal` output of the uppercase key, but no phone scanned it.

**Manual steps:**
1. `docker compose -f deploy/compose.yaml run --rm --no-deps --entrypoint /zrelay zrelay key generate --qr`
   and scan the QR code with the zview phone app; note the `fingerprint:` line.
2. Write the `key:` value to `deploy/secrets/zrelay_master` (bare text, `chmod 600`), then
   `docker compose -f deploy/compose.yaml run --rm --no-deps --entrypoint /zrelay zrelay key fingerprint --key FILE:/run/secrets/zrelay_master`
   and `... key api-token-hash --key FILE:/run/secrets/zrelay_master`.
3. `... key fingerprint --key zk1-anything` and `... key fingerprint --key KEYCHAIN:x`.

**Expected result:** step 1 prints `key: zk1-<64 hex>-<4 hex>` and `fingerprint: xxxx-xxxx-xxxx-xxxx`,
and the phone app decodes the QR to the same key (it accepts the uppercase form) and shows the same
fingerprint. Step 2 prints the same fingerprint, then a 64-hex hash that matches the one entered in
`api_token_sha256` for zview-server. Step 3 exits 2: the first says `--key takes a reference ... never
a literal` and does not echo the value, the second says the `KEYCHAIN:` scheme is unsupported.

**If the result differs, update:** the QR config in `keyGenerate` (`cmd/zrelay/key.go`: error
level, `HalfBlocks`, quiet zone) and `TestKeyGenerateQRUsesUppercaseForm` if the phone cannot scan
it; `zrec.Derive` / `zrec.FormatKeyID` and Contract §3 if the app derives a different fingerprint or
token hash.
## Task 11 — Live view (WHEP)

**Automated in this task:** `internal/relay/live` is tested end to end with an in-process pion
client on loopback (ephemeral ports only): `TestWHEPOfferAnswerDeliversVideo` (H.264 RTP within
3 s, first decoded slice is an IDR), `TestWHEPAudioNegotiatedWhenPresent` (PCMA track),
`TestWHEPNoAudioTrackWhenCameraHasNone`, `TestWHEPAuth` (401, no pion session created),
`TestWHEPMaxViewers` (503), `TestWHEPDeleteEndsSession`, `TestWHEPUnknownCamera404`,
`TestWHEPWrongContentType415`, `TestWHEPBodyLimit413`, `TestWHEPBadOffer400`,
`TestViewerLagResyncsAtIDR`, `TestSessionClosedWhenPeerCloses` (clean path: peer closes DTLS, slot freed),
`TestSessionClosedWhenPeerGoesSilent` (client socket closed without a DTLS close; ICE timeouts shortened
through the `iceDisconnected`/`iceFailed` variables), `TestICETimeoutsClosePeerWithin30Seconds`
(defaults sum to 30 s), `TestServerCloseAtEachNegotiationStep` (Close forced deterministically through a test hook at the
"created", "attached" and "negotiated" points of a POST: 503, pion session closed, slot freed;
mutation-checked against the attach guard and both 503 branches) and
`TestServerCloseDuringNegotiationStress` (Close racing in-flight POSTs, under `-race`), `TestWHEPDisabledWithZeroMaxViewers` (503), `TestWHEPRejectsH265With501`,
`TestPeerThatNeverConnectsIsClosedAfterTimeout` (fake clock), `TestNewServerValidation`/`Defaults`,
`TestServerCloseEndsSessions`. `BenchmarkViewerMemory` (four viewer processes at 400 KiB/s) measured
about 2.2 MiB of heap and stacks per added viewer (2.27 to 2.36 MB across three runs), so
`config.LivePerViewerBytes` is now 3 MiB and design §8.1 says so. Not covered: a real browser, a
real camera, and ICE across the tailnet. The 30 s silent-peer timeout is tested only with shortened
ICE timeouts; the production values (5 s disconnected + 25 s failed) are not waited out.

**Manual steps:**
1. On the Mac mini, with zrelay running against the Tapo C220, `live.advertise_ip` set to the
   Tailscale IP and `live.ice_udp_port: 8189` published on that IP only, serve
   `internal/relay/live/testdata/whep.html` from the same origin as `/live/*` (for example through
   the zview-server `--mode both` proxy, or any reverse proxy that forwards `/live/` to
   `http://zrelay:9465`).
2. Compute the bearer token, unpadded base64url of `K_api` (`pkg/zrec.BearerToken`; use the CLI
   command the key task provides) and paste it into the page's token box. Do not log it.
3. On a phone on the tailnet (Safari iOS, then Chrome Android) open the page, enter the camera id,
   press Start.
4. Open more tabs up to `live.max_viewers: 4`, then one more, and confirm the extra one fails
   with 503.
5. Press Stop and confirm the session ends; instead switch the phone off Wi-Fi and confirm the
   session is freed within about 30 s.
6. If the camera has a microphone enabled, unmute the `<video>` and listen.
7. Watch `docker stats zrelay` with four viewers for 10 minutes.

**Expected result:** video starts within about one GOP (up to 2 to 4 s) on every browser, with no
green frames at the start; audio plays when the camera sends G.711; the fifth viewer gets 503; a
stopped or vanished viewer frees its slot; memory per viewer stays near 3 MiB.

**If the result differs, update:** a browser that fails to negotiate H.264: `h264Profiles` in
`internal/relay/live/server.go`; garbled first frames: SPS/PPS prepending in `viewer.annexB`
(`session.go`); no audio: `audioWatcher` in `server.go`, `negotiate` in `whep.go`, and whether the
camera sends PCMA or PCMU; ICE fails across Tailscale: `SetICEAddressRewriteRules` in `newAPI`
and the published port in `deploy/compose.yaml`; memory off: `config.LivePerViewerBytes`, design
§8.1 and `TestBudgetWorkedExample`.
## Task 12 — RTSP restream

**Automated in this task:** `internal/relay/restream` (`New`, `Start`, `Close`, `Clients`, `Addr`) is
tested end to end on ephemeral ports. A looping synthetic H.264 camera (`testutil.StartRTSPServer`)
feeds a `camera.Hub` through `camera.RunRTSP`; a gortsplib TCP client with credentials reads 30 RTP
packets (`TestRestreamServesVideo`), no or wrong credentials get 401 (`TestRestreamAuthRequired`),
RTSP-over-HTTP and over-WebSocket tunnels get 403 (`TestRestreamRejectsTunnels`), forcing UDP fails
SETUP with 461 (`TestRestreamUDPRefused`), `Hub.Lost()` ends the client session and the stream
returns on the next `Codec` call (`TestRestreamClientsDroppedOnCameraLost`), and a PCMA track is
published once the camera sends audio (`TestRestreamAudioTrackWhenPresent`). All pass under
`go test -race`. Plan Step 5 (the `camprobe --live --restream` extension) is deferred to T12b, so
the steps below cannot run until it lands.

**Manual steps:**
1. After T12b lands, on the Mac mini run camprobe with supervisor, live and restream only:
   `go run ./cmd/camprobe --live --restream` with the camera URL and restream credentials supplied
   as `ENV:` refs (exact flags per T12b).
2. On a laptop on the tailnet open `rtsp://<restream user>:<password>@<mac-mini-tailscale-ip>:8554/tapo-c220`
   in VLC (Media, Open Network Stream).
3. Run `go run ./internal/relay/live/testdata` on the Mac mini and open its `whep.html` on a phone
   over the tailnet.
4. Run `ffprobe -rtsp_transport udp rtsp://<user>:<pass>@<ip>:8554/tapo-c220` from the laptop.
5. Stop the camera's network for 10 s, restore it, and watch VLC.

**Expected result:** VLC plays the live Tapo C220 picture (video, plus G.711 audio if the camera
sends it) within a few seconds; the phone plays via WHEP; the UDP probe is refused (461); a wrong
password is refused (401); when the camera session drops, VLC disconnects and a manual reconnect
succeeds once the camera is back.

**If the result differs, update:** `publisher.publish`/`writeVideo` in
`internal/relay/restream/restream.go` (RTP timestamps are `uint32(AU.PTS)`; parameter sets come only
from the SDP, not in-band) and `TestRestreamServesVideo`; if players need in-band SPS/PPS before an
IDR, prepend `Codec` parameter sets in `writeVideo`. If the audio track is missing for the first
seconds, see `writeAudio` (the track appears when the first audio packet arrives, which republishes
the stream and drops early clients) and `TestRestreamAudioTrackWhenPresent`. Design §5.2.

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

## Task 27 (fix) — Startup crash recovery: age filter and step-0 format
**Automated in this task:** `recovery.scan` now lists every incomplete (`ListIncomplete(ctx, prefix, 0)`) and applies `OlderThan` to videos and staging sidecars alike against `Config.Now`; a young interrupted video is left untouched and its staging sidecar is treated as paired. New sub-tests: `OldStagingWithYoungVideoIsUntouched`, `StagingWithoutSchemaIsRecovered` (both on `local` and MinIO), `TestDefaultOlderThan` (OlderThan 0 = 2 min), `TestRunRejectsBadConfig` (nil Provider/Recoverer, zero `Now`). `make vet test` and `make integration` pass.
**Dependency for Task 25/26 (step-0 writer):** recovery supplies `schema` (`sidecar.Schema`) when the staging sidecar lacks it, but every other field `sidecar.Decode` and `Validate` require, including `recorder.*`, must still be in the staging sidecar. `testClip.stagingDoc` in `internal/relay/recovery/harness_test.go` is the reference shape. A staging sidecar that fails validation is counted as Orphaned.
**Manual steps:**
1. Against the Synology MinIO, start a clip, `kill -9` the relay after about 3 minutes of recording, and restart it within 2 minutes. Run `zrelay` startup recovery, then restart again after 2 more minutes.
**Expected result:** the first restart changes nothing (no `Discarded`, the staging sidecar and the multipart upload are still there); the second restart recovers the clip (`Recovered: 1`) with an `incomplete` sidecar.
**If the result differs, update:** the age comparison in `recovery.scan` (`internal/relay/recovery/recovery.go`) and Contract §7.4. If host and MinIO clocks differ, see the clock-skew note in the Task 27 entry above.
**Status:** not run

## Task 12b — camprobe live and restream modes (Task 12 Step 5)

This entry carries out Task 12's deferred Step 5 and replaces its manual steps 1 and 3, which named a
`./cmd/camprobe` path and a `go run ./internal/relay/live/testdata` program that did not exist yet.

**Automated in this task:** `camprobe` (`internal/testutil/cmd/camprobe`) gained `--live` and
`--restream`. Each starts, next to the supervised camera (RTSP + ONVIF, no recorder, no storage), the
WHEP server (`live:` block of the config, served over HTTP on `live.listen`) and the RTSP restream
(`restream:` block), and logs `live listening addr=...` and `restream listening addr=...`. A flag whose
config block is disabled exits 2 (`TestCamprobeLiveRestreamNeedConfig`). `TestCamprobeLiveAndRestream`
runs it against the test RTSP camera and fake ONVIF on loopback ephemeral ports: a WHEP offer without a
bearer gets 401, with the bearer gets 201 and an answer SDP with `m=video`, and a gortsplib TCP client with
the restream credentials reads 30 RTP packets; the probe exits 0 and prints neither the restream password
nor the bearer token. It passes under `go test -race`. New helper `internal/relay/live/testdata/main.go`
(`go run ./internal/relay/live/testdata`) serves `whep.html` on one origin and reverse-proxies `/live/`
to `--upstream`; smoke-run on loopback (page served, `/live/` answers 502 with no upstream). Not
covered: a real camera, VLC, a phone, ICE across the tailnet.

**Manual steps:**
1. On the Mac mini, build the config and secrets as in the Task 9 live check (steps 1 to 3 there:
   `$D/zrelay.yaml` from `deploy/zrelay.example.yaml`, `.env` with the camera credentials,
   `TAILSCALE_IP=<mac mini tailscale ip>`, `RESTREAM_USER=viewer`, `RESTREAM_PASS=<a password>`). Then make
   the WHEP token and its hash (K_api is a random 32 bytes; the token is its unpadded base64url):
   `head -c 32 /dev/urandom > $D/kapi && shasum -a 256 $D/kapi | cut -d' ' -f1 > $D/api_token_sha256 && base64 < $D/kapi | tr '+/' '-_' | tr -d '=\n' > $D/token`.
   Do not paste the token into logs or notes.
2. Start the probe (no other zrelay running against the camera):
   `go run ./internal/testutil/cmd/camprobe --env-file .env --config $D/zrelay.yaml --camera tapo-c220 --live --restream --interval 30s`
   Wait for `state=ready` and the two `listening addr=` lines.
3. On a laptop on the tailnet, VLC, Media, Open Network Stream:
   `rtsp://viewer:<password>@<mac-mini-tailscale-ip>:8554/tapo-c220`.
4. On the Mac mini, in a second terminal:
   `go run ./internal/relay/live/testdata --listen <mac-mini-tailscale-ip>:8080 --upstream http://127.0.0.1:9465`
   On a phone on the tailnet open `http://<mac-mini-tailscale-ip>:8080/`, paste the contents of
   `$D/token`, camera id `tapo-c220`, press Start.
5. From the laptop: `ffprobe -rtsp_transport udp rtsp://viewer:<password>@<ip>:8554/tapo-c220` and the same
   with a wrong password.
6. Cut the camera's network for 10 s, restore it, and watch VLC and the phone.
7. Stop the probe with Ctrl-C and confirm it exits within a few seconds. Then `rm -rf $D`.

**Expected result:** VLC shows the live C220 picture within a few seconds (plus G.711 audio if the camera
sends it); the phone page plays video (muted autoplay) within about one GOP and `ice: connected` shows in
its log; the UDP probe is refused (461) and the wrong password gets 401; when the camera drops, VLC
disconnects and a manual reconnect works once the probe logs `state=ready` again; the probe's
`goroutines` and `rss_kib` stay flat over the run; Ctrl-C exits 0.

**If the result differs, update:** VLC fails or shows garbage: `publisher.publish`/`writeVideo`
(`internal/relay/restream/restream.go`) and `TestRestreamServesVideo`; the phone fails: the notes of the
Task 11 entry (`newAPI` in `internal/relay/live/server.go`, `live.advertise_ip`, firewall for UDP
`live.ice_udp_port` on the Tailscale interface); `--live` or `--restream` rejected or no `listening` line:
`startServers` and `run` in `internal/testutil/cmd/camprobe/main.go` and `TestCamprobeLiveAndRestream`;
the page cannot reach `/live/`: `internal/relay/live/testdata/main.go`.

## Task 35 (fix) — Crypto/data worker: real-browser worker and recovered clip
**Automated in this task:** Vitest covers the worker core, `attachWorker`, the `worker.ts` bootstrap under a stubbed `WorkerGlobalScope`, the `getWorkerClient()`/`workerClient.call` singleton with a stubbed `globalThis.Worker`, a Go-shaped recovered sidecar (`seek_index: null`), 401 fallback memory, and bounded response bodies. `make vet test` and `make web` pass. None of it runs a real browser Worker.
**Manual steps:**
1. Start a stack with a recovered clip: run zrelay against MinIO, kill it mid-clip (`docker kill zrelay`), restart it so crash recovery writes an `incomplete` sidecar, then start `zview-server`.
2. Build the SPA (`cd web && pnpm build`), open it in Chromium and WebKit over the tailnet URL, import the master key, and open the recovered clip from the clip list.
3. In DevTools, confirm `GET /api/v1/clips/{id}` returned `"seek_index": null` and the worker did not report ErrSchema; check that the Network panel shows a `new Worker` module script load for the worker bundle and no `Authorization` header or `zk1-` text in main-thread messages.
**Expected result:** The incomplete clip opens, plays up to the last intact chunk, and ends with a "truncated" notice rather than an auth or schema error. The worker script loads in both browsers.
**If the result differs, update:** `web/src/worker/schemas.ts` (`video.seek_index`, `crossChecks`), `web/src/worker/clips.ts` (`allowIncomplete`), `web/src/lib/workerClient.ts` (`getWorkerClient` Worker URL/options), the Vite worker build config in `web/vite.config.ts`, and the matching tests in `schemas.test.ts`, `worker.test.ts`, `workerClient.test.ts`.
## Task 37 — Timeline component
**Automated in this task:** `web/src/lib/timeline/` (Canvas 2D `Timeline.svelte`, pure `model.ts`, `zoom.ts`, `timezone.ts`, `render.ts`). Vitest: `localDayRange` for Asia/Jakarta, Asia/Kolkata and America/New_York across both DST changes (23 h and 25 h days); glyph and color mapping (P orange, V green, A purple, M blue, • gray); hit-test snap at 24 px landing on the first trigger offset; coverage complement shaded as not monitored; zoom presets 24 h, 6 h, 1 h, 10 min, 1 min; dirty-flag rAF scheduler. Playwright (`web/e2e/timeline.spec.ts`, Chromium and WebKit): click, snap, wheel zoom, double-click and drag pan. Chromium perf test: 4 x 500 synthetic clips with coverage gaps, scripted wheel-zoom and drag for 3 s; one run measured an average draw cost of 0.73 ms per frame (180 frames, max 2.8 ms). The test asserts average draw cost per frame below 16.7 ms.
**Manual steps:**
1. On the Mac mini, serve the SPA with a build that includes the timeline view and open the Perf page `web/e2e/page/timeline.html?data=perf` through the dev server (`cd web && pnpm exec vite --host 127.0.0.1 --port 0`, note the printed port) from a mid-range phone on the tailnet (use the Mac's tailnet address instead of 127.0.0.1 with `--host 0.0.0.0` only for this check).
2. On the phone, pan with one finger and pinch-zoom continuously for 10 s. In Safari Web Inspector (iOS) or Chrome `chrome://inspect` (Android) record a Timelines/Performance trace.
3. Open the real zview with a day that has clips on 4 cameras; set the display zone to `Asia/Jakarta` and then to the viewer zone; confirm the ruler labels follow the zone.
4. Open a day containing a DST change in a forced `America/New_York` zone (2026-03-08 and 2026-11-01) and confirm the day is 23 h and 25 h wide.
**Expected result:** 60 fps (frames under 16.7 ms) during pan and pinch on the phone; pinch and one-finger pan do not scroll the page; the now line shows only when viewing today; hovering a hatched region labels it "not monitored"; the ruler matches the forced zone.
**If the result differs, update:** per-frame cost in `drawTimeline` (`web/src/lib/timeline/render.ts`: batching, hatch pattern) and `layoutLanes` culling (`model.ts`); gesture constants in `Timeline.svelte` (pinch, wheel sensitivity); ui-spec §4.6 performance target if the target changes.
**Status:** not run

## Task 26 — Drain workers, commit protocol, shutdown
**Automated in this task:** `internal/relay/egress` (`Egress`, `Run`, `Shutdown`, `BacklogBytes`, drain workers) is tested against a scriptable fake `storage.Writer` and the fake clock: `TestCommitOrdering` (staging Put, object Commit, final Put, staging Delete; no sidecar before Commit), `TestResumeAfterTransientWriteError` (lagging `DurableOffset`, object bytes identical to the stream), `TestBreakerOpenParksWorkersAndBacklogGrows` and `...DrainsAfterClose`, `TestEvictionAbortsByOwner` (Abort on the worker goroutine that called Create), `TestWorkersLimit`, `TestShutdownWithOpenBreakerExitsWithinGrace` (30 s fake grace, both clips aborted, `OnDrop(...,"shutdown")` twice), plus healthy-drain shutdown, recorder abandon and force-finalize forwarding. Review fixes: `TestWriteFailuresOpenBreakerAndParkWorker` (writes fail with no manual `Breaker.Failure`: breaker opens after 5, worker parks, `Run` returns on cancel), `TestWriteFailuresBackOff`, `TestCommittedClipSurvivesBacklogOverflow`, `TestUploadGoneRestartsFromStream`, `TestUploadGoneAbandonsWhenBytesReleased` (no drop reason outside the coverage enum), `TestCommitNotFoundRestartsFromStream`, `TestPersistWithoutBreakerRetriesAfterDelay`, `TestFinalSidecarErrorLeavesStagingForRecovery`, `TestCameraOf`. Second review round: `TestBufferedWritesDoNotMaskOutage` and `TestBufferedWritesBackOffPartUploads` (a fake writer that buffers like the S3 driver and touches storage only when a 4 x 256 KiB part fills: with storage down the breaker opens after exactly 5 part-upload failures, attempts stay bounded while the fake clock is frozen, the worker parks), `TestBufferedWritesCommitWhenHealthy`, `TestCommittedClipSurvivesBacklogOverflow` (now a real overflow: the overflowing clip is force-finalized, the committed clip is neither evicted nor reported), `TestAccountantSkipsCommittedClip`, `TestDropOfCommittedClipIsNotReported`, `TestCommitSucceedsDespiteEvictionStillStoresFinalSidecar`, `TestStagingDeleteKeepsFailingClipStaysCommitted`, `TestShutdownInterruptsFinalSidecarPut`. All pass with `-race`. Nothing ran against a real MinIO.
**Manual steps:**
1. Start the stack against the Synology MinIO with a small backlog: `egress_backlog_mb: 16`, `shutdown_grace_seconds: 30`, `upload_workers: 2`, one camera recording continuously (`docker compose up -d zrelay`).
2. Block MinIO from the Mac mini: on the Synology, `iptables -I INPUT -p tcp --dport 9000 -j DROP` (or stop the MinIO container). Wait until `curl -s 127.0.0.1:9464/metrics | grep zrelay_storage_breaker_state` shows `2`.
3. While it is down, watch the backlog grow with the relay's own metric, which does not depend on storage (status writes are skipped while the breaker is open, design §6.3, so `status/relay.json` cannot show it): `watch -n 5 "curl -s 127.0.0.1:9464/metrics | grep zrelay_backlog_bytes"` (relay-spec §4.12). The `egress: clip dropped ... reason=backlog` log lines (`docker logs -f zrelay`) show when the limit is hit. Then send `docker stop -t 60 zrelay` and time it: `time docker stop -t 60 zrelay`.
4. Restore MinIO, restart zrelay, wait for the recovery sweep to finish (about one minute), then run both of these:
   - `mc ls --incomplete --recursive <alias>/<bucket>/clips/` (unfinished multipart uploads).
   - `mc ls --recursive <alias>/<bucket>/clips/ | grep '/\.'` (dot-named staging sidecars, `.<HHMMSSmmm>_<clip_id>.json.part`).
   Expected: the first prints nothing; the second prints nothing, except staging sidecars of clips whose video is committed but whose final sidecar the recovery sweep has not yet promoted (Contract §7.4), which must disappear after a second restart.
5. Repeat steps 1 and 2, but restore MinIO before stopping; confirm the clips appear in `clips/<camera>/...` with `.zrec` and `.json`, and the dot-named staging sidecars are gone.
**Expected result:** the container exits within about `shutdown_grace_seconds` (not 60 s) with one log line `egress: clip dropped ... reason=shutdown` per buffered clip and `zrelay_clips_dropped_total{reason="shutdown"}` incremented accordingly; after recovery, clips buffered while MinIO was down but not dropped commit with final sidecars and no leftover staging sidecar; a backlog over 16 MiB logs `reason=backlog` drops, oldest finalized clip first.
**If the result differs, update:** `Egress.Shutdown` and `Egress.abort` (`internal/relay/egress/egress.go`, `worker.go`: the real-time `abortTimeout`, which bounds `Abort` against an unreachable MinIO), the S3 driver's `Abort`/`Resume` context handling (`pkg/storage/s3`), and design §6.3/§6.4 plus `TestShutdownWithOpenBreakerExitsWithinGrace`.
## Task 34 — Live proxy, zview image, compose
**Automated in this task:** `zview.NewLiveProxy` (`internal/zview/liveproxy.go`) with tests against an `httptest` upstream: body and `Authorization` forwarded, bad auth answered 401 without contacting the upstream, `Location` rewritten to the proxy path, 413 (declared and unknown length, exactly 64 KiB accepted), 415, 405 for other methods, upstream 503 passed through, 502 when the upstream is down. `cmd/zview-server` mounts the proxy on `live.upstream` (`TestMountsLiveProxyOnConfiguredUpstream`). The upstream transport ignores `HTTP_PROXY`/`HTTPS_PROXY` (`TestLiveTransportIgnoresProxyEnvironment`), and a relative upstream `Location` is resolved against the outgoing URL before rewriting. zview-server receives only `S3_ENDPOINT` and `S3_RO_ACCESS_KEY` from `deploy/.env` (verified with `docker compose config` and stand-in files, which also fails fast when either is missing). `make vet test` passes. `docker build -f deploy/Dockerfile.zview .` (node 26 + pnpm 12 build, Go build, scratch with the SPA at `/app`, 13 MB) and `docker build -f deploy/Dockerfile.zrelay .` succeed, and `docker compose -f deploy/compose.yaml config -q` validates with stand-in config and secret files. The stack was not started.
**Manual steps:**
1. Create the files named in the header of `deploy/compose.yaml`: `deploy/.env` (`S3_ENDPOINT` and `S3_RO_ACCESS_KEY` are the only entries zview-server receives, interpolated by compose; the zrelay entries `S3_ACCESS_KEY`, `RESTREAM_USER`, `RESTREAM_PASS`, `CAMERA_*` go to zrelay only), `deploy/zrelay.yaml`, `deploy/zview.yaml` (from `deploy/zview.example.yaml`), and `deploy/secrets/{zrelay_master,s3_secret_key,api_token_sha256,s3_ro_secret_key}`.
2. `sh deploy/deploy.sh` on the Mac mini (uses `tailscale ip -4`).
3. `docker compose -f deploy/compose.yaml ps` and `curl -s http://127.0.0.1:8080/healthz`.
4. `tailscale serve --bg --https=443 http://127.0.0.1:8080`, then open the HTTPS URL from a phone on the tailnet, import the key and open a camera's live view.
5. `docker inspect zrelay-zview-server-1 --format '{{.HostConfig.ReadonlyRootfs}} {{.Mounts}}'` (the image is scratch, so `docker exec` is unavailable).
6. From the Mac: `curl -si -X POST http://127.0.0.1:8080/live/front -H 'Content-Type: application/sdp' --data 'v=0'` (expect 401) and the same with `-H "Authorization: Bearer <K_api>"` (expect an upstream status, not 502).
**Expected result:** both containers `Up`; `/healthz` returns `ok`; the SPA loads over HTTPS; live view connects through the proxy (`Location` is `/live/<camera>/<session>`, never `http://zrelay:9465/...`); `ReadonlyRootfs` is `true` and `Mounts` is `[]`; `docker inspect zrelay-zview-server-1 --format '{{.Config.Env}}'` lists `S3_ENDPOINT` and `S3_RO_ACCESS_KEY` but none of `CAMERA_*`, `RESTREAM_*`, `S3_ACCESS_KEY`; step 6 returns 401 without the token and never 502 with it.
**If the result differs, update:** 502 from the proxy: `live.upstream` in `deploy/zview.yaml` or `live.listen` in `deploy/zrelay.yaml` (`NewLiveProxy`, `DefaultLiveUpstream` in `internal/zview/config.go`); missing SPA: the `COPY --from=web` line in `deploy/Dockerfile.zview` and the Vite output dir; container fails to start: `deploy/compose.yaml` zview-server `configs`/`secrets` and the `deploy.sh` preflight list; `Location` wrong: `proxyLocation` and `TestLiveProxyRewritesLocation`.
## Task 38 — Key onboarding and keyring UI
**Automated in this task:** `web/src/routes/onboarding/` (`Onboarding.svelte`, `Scanner.svelte`, `keyInput.ts`, `mismatch.ts`, `idleLock.ts`) and `web/src/lib/qr/scan.ts`. Vitest with jsdom and `@testing-library/svelte`: live "typo in key" versus valid versus malformed feedback; import through the worker protocol with the field cleared afterwards; `location.href` never contains `zk1` after import and the form's submit is default-prevented; the remember toggle; the "This key doesn't match recent recordings" warning (latest 20 clips, both fingerprints); Forget empties the real `createWorker` IndexedDB keystore (fake-indexeddb); Lock; the idle auto-lock setting (persisted, fires after the configured minutes with the tab hidden); QR scanning of a real rendered QR code through `jsqr`, and the `BarcodeDetector` path with a stubbed detector. No real camera, `getUserMedia` or real `BarcodeDetector` was exercised.
**Manual steps:**
1. Serve the built SPA over the tailnet HTTPS URL (`tailscale serve`) with the Onboarding view reachable. Generate a key QR: `zrelay key generate` prints the `zk1-...` string; render it with `qrencode -t ANSIUTF8 'zk1-...'` (or any QR tool).
2. On an iPhone, open the page in Safari and then as an installed Home Screen app. Tap "Scan QR code", grant camera permission, and point at the QR. Repeat on Android Chrome (has `BarcodeDetector`) and iOS Safari (uses `jsqr`).
3. Tick "Remember this device", import, then check the keyring shows the fingerprint (`xxxx-xxxx-xxxx-xxxx`) matching `zrelay key fingerprint` output / `status/relay.json` `key_id`. Reload the page: the key is still listed as remembered.
4. Import a different, valid key: the warning "This key doesn't match recent recordings" appears with both fingerprints.
5. Set "Auto-lock when tab is hidden" to 1 min, switch to another app for over a minute, return: the worker is locked (non-remembered keys gone). Tap Forget on a remembered key and confirm DevTools > Application > IndexedDB `zview-keys` is empty.
6. In DevTools Network and the address bar, confirm no request URL, fragment or history entry contains `zk1`.
**Expected result:** the QR scans in the installed iOS app (where Safari's storage is not shared) and in Safari; the camera light turns off when the scanner closes or after a successful scan; fingerprint matches the relay; the mismatch warning appears only for a non-matching key; Forget empties the IndexedDB store; no URL contains the key.
**If the result differs, update:** `openCamera` (constraints, `playsInline`, canvas grab) and `createDecoder` in `web/src/lib/qr/scan.ts` plus `scan.test.ts`; `Scanner.svelte`; the mismatch window in `web/src/routes/onboarding/mismatch.ts` (`RECENT_CLIPS`) and ui-spec §4.1; idle behaviour in `idleLock.ts`.
**Status:** not run

## Task 38 (review fixes) — Key onboarding: suspended-tab auto-lock, rejected key, camera release
**Automated in this task:** jsdom tests for `openCamera` (constraints `{video:{facingMode:{ideal:'environment'}}, audio:false}`, tracks stopped when `play()` rejects, `grab()` null without video dimensions, canvas pixels otherwise, all with a stubbed `getUserMedia`); `idleLock.test.ts` for a timer that never fires while the clock passes the deadline (locks once on return to visible); Onboarding tests for HTTP 401 on the clip list ("The server did not accept this key"), rejected worker calls on import, lock, forget and auto-lock (generic alert, no key text), in-flight recording checks discarded by Lock and Forget, and no key hex in the page or `location.href`. A real suspended browser tab and a real camera were not exercised.
**Manual steps:**
1. On an iPhone Home Screen app, set "Auto-lock when tab is hidden" to 1 min, import a key without "Remember this device", lock the phone (or switch apps) for 5 minutes, then reopen the app.
2. Import a valid key that the server does not accept (for example a key from another household) on a fresh device and watch the status line under the form.
3. Open the scanner, then close it and deny camera permission once; confirm the camera indicator turns off in each case.
**Expected result:** on reopening in step 1 the keyring shows "Locked: session keys were discarded." immediately, even though timers were suspended; step 2 shows "The server did not accept this key" instead of "Could not check recent recordings"; the camera indicator is off after closing and after a denied permission.
**If the result differs, update:** the visibility handling and clock check in `web/src/routes/onboarding/idleLock.ts` (consider `pagehide`/`freeze` events) with `idleLock.test.ts`; the 401 mapping in `checkRecordings` in `Onboarding.svelte`; `openCamera` in `web/src/lib/qr/scan.ts` with `scan.test.ts`.
**Status:** not run
## Task 29 — `zrelay run` wiring, chaos and crash tests
**Automated in this task:** `app.Run` (`internal/relay/app/app.go`) wires storage, breaker, recovery (retried on each breaker close until it has succeeded once, and every minute while the breaker stays closed), the per-camera pipelines (supervisor, recorder, clip machine, backlog policy), live view, restream, egress, the status publisher and the `/healthz` endpoint, and shuts down in the order clip machines, `Egress.Shutdown`, status flush, live/restream/HTTP, ONVIF unsubscribe. `zrelay run --config <path> [--env-file <path>]` handles SIGTERM and SIGINT (`cmd/zrelay/run_test.go` sends a real SIGTERM to the test process). Tests against a fake RTSP server and a fake ONVIF camera on the `local` driver: `TestRunRecordsClipToLocal` (trigger, sidecar and video appear, stop within `shutdown_grace_seconds`, coverage flushed, no staging sidecar left), `TestRunShutdownWithStorageDownExitsWithinGrace`, `TestRunRetriesRecoveryOnBreakerClose`, `TestRunServesHealthzAndRestream`; `recorder.WithBacklogPolicy` (`TestRunAppliesBacklogPolicy`). Integration (`make integration`, MinIO in Docker): `TestKill9ProducesIncomplete` runs the compiled `zrelay` as a subprocess, SIGKILLs it once a multipart upload has a completed part, restarts the app in-process with a clock three minutes ahead (recovery only takes uploads older than two minutes) and checks the `incomplete`/`recovered` sidecar and that the video opens with `AllowIncomplete`. `make chaos` (`TestChaos`, tag `chaos`, not part of CI): three 4 Mbps cameras, triggers always active, 128 MiB backlog, `max_memory_mb` 256, MinIO behind the fault proxy blackholed for 10 real minutes; asserts breaker open then closed, backlog drops in coverage, a clip committed after the outage and `/memory/classes/total:bytes` never above 256 MiB. `make chaos` ran once with the full 10-minute outage and passed (635 s): [superseded by the sections "Task 29 (review fixes)" and "Task 29 (chaos fix)" below: that run did not reproduce] the breaker opened 1 minute into the outage and closed after it, 54 `backlog` drops were recorded in coverage, a clip was committed after the outage, and shutdown stayed within the 30 s grace. The runtime peaked at 252.7 MiB of 256 MiB measured as `/memory/classes/total:bytes` minus `/memory/classes/heap/released:bytes` (what `debug.SetMemoryLimit` bounds); `total:bytes` itself, which keeps counting pages already returned to the OS, peaked at 270.0 MiB, so the literal "total never above 256 MiB" does not hold (see the task report) [superseded: the GC percent fix in "Task 29 (review fixes)" brought `total:bytes` under 256 MiB; current numbers are in "Task 29 (chaos fix)"]. Two production fixes came out of it: the s3 driver now gets an HTTP client with a response-header timeout (`s3HTTPClient`; without it a partitioned endpoint hung requests forever and the breaker never opened), and `app.Run` sets `debug.SetMemoryLimit(max_memory_mb)` (relay-spec §3.2).
**Manual steps:**
1. Follow the `deploy/runbook.md` draft (Task 30 finalizes it): create the MinIO bucket and the relay and read-only users with the policies of design §6.5, write `deploy/.env`, `deploy/zrelay.yaml` and the secrets named in the `deploy/compose.yaml` header, then run `sh deploy/deploy.sh` on the Mac mini.
2. `docker compose -f deploy/compose.yaml logs -f zrelay` and confirm `camera state camera=<id> state=ready`, `startup recovery done` and `zrelay running`.
3. Walk past the Tapo C220. Confirm a clip: `mc ls --recursive <alias>/<bucket>/zrelay/clips/<camera>/` shows a `.zrec` and a `.json` (and no dot-named `.json.part`).
4. `mc cp <alias>/<bucket>/zrelay/clips/<camera>/YYYY/MM/DD/<name>.zrec <alias>/<bucket>/zrelay/clips/<camera>/YYYY/MM/DD/<name>.json /tmp/` then `zdecrypt --key FILE:<path-to-master-key> --in /tmp/<name>.zrec --sidecar /tmp/<name>.json --out /tmp/<name>.mp4` and play `/tmp/<name>.mp4` (plaintext on a trusted machine only; delete afterwards).
5. Stop MinIO for 2 minutes (`ssh <nas> 'docker stop minio'` or the Synology Container Manager), keep triggering the camera, and watch `docker compose -f deploy/compose.yaml logs zrelay | grep "storage breaker"` for `state=open`. Start MinIO again and watch for `state=half_open` then `state=closed` and `startup recovery done` (when recovery had not succeeded yet). `zrelay_storage_breaker_state` is not exported until Task 30; the log lines are the signal until then.
6. `docker compose -f deploy/compose.yaml stop zrelay` (or `kill -9` the container's process with `docker kill -s KILL zrelay-zrelay-1` while a clip is recording) and restart: `mc ls --incomplete --recursive <alias>/<bucket>/zrelay/clips/` must be empty after the recovery sweep and the interrupted clip must have an `incomplete` sidecar.
**Expected result:** a clip with `.zrec` and `.json` appears within about `post_roll_seconds` of the motion ending; `zdecrypt` verifies it and the MP4 plays; while MinIO is down the breaker opens, clips buffer in memory (up to `egress_backlog_mb`) and arrive after MinIO returns, with no staging sidecar left; `docker stop` returns within `shutdown_grace_seconds` (compose `stop_grace_period` is 40 s); after a kill -9 the interrupted clip is listed with `status: incomplete`, `end_reason: recovered` and its video opens with `zdecrypt --allow-incomplete`.
**If the result differs, update:** `internal/relay/app/app.go` (`start`, `shutdown`, `recoverLoop`, `recoveryRetryEvery`) with `app_test.go`; the camera wiring (`startCamera`: ring cap, queue sizes, `OnState` into coverage) and design §3/§6.5; `recorder.WithBacklogPolicy` and relay-spec §4.5 (resume at 50% of the backlog limit); `stop_grace_period` in `deploy/compose.yaml`; when a real C220 does not become `ready`, `TapoRules()` and design §4.3.
**Status:** not run

## Task 29 (review fixes) — `zrelay run` recovery, memory and shutdown
This section supplements the Task 29 entry above (which stays as written); where they differ, this one is current.
**Automated in this task:** review fixes to `internal/relay/app`. Startup recovery now measures candidate ages against the process start time on every retry (`TestRecoveryRetryNeverTouchesThisProcessesClips`: a 5-minute clock jump with a clip in flight and a failing then recovering scan must not commit the upload or delete its staging sidecar). The live-view wiring is tested (`TestRunServesLiveWithBearerAuth`: 401 without a bearer, 404 for an unknown camera with a valid one, port closed after `Run` returns). The ONVIF-unsubscribe wait also ends when the supervisor's event loop is not running (`TestDoneSource*`, `TestRunShutdownWithONVIFDownIsPrompt`). The storage-down shutdown test asserts the spec bound (`took <= shutdown_grace_seconds`, plus 50 ms of scheduling slack). The egress `ForceFinalize` -> `recorder.WithBacklogPolicy` wiring is covered in CI (`TestRunForceFinalizesThroughTheBacklogPolicy`). `TestChaos` now asserts the specified metric, `/memory/classes/total:bytes` <= `max_memory_mb`; it failed at 277.6 MiB (3-minute outage) until `app.Run` set `debug.SetGCPercent(20)` (`TestRunTightensTheGCPercent`; the memory limit alone let garbage fill the heap up to the limit). `make chaos` with the full 10-minute outage then passed (632 s): `total:bytes` peaked at 241.3 MiB of 256 MiB (total minus released 220.6 MiB), the breaker opened 1 minute into the outage and closed after it, 54 `backlog` drops were recorded in coverage, a clip was committed after the outage and shutdown stayed within the grace. `make vet test` and `make integration` pass.
**Manual steps:**
1. On the Mac mini, with the real C220 and the Synology MinIO, run the Task 29 manual steps above with `egress_backlog_mb` and `max_memory_mb` as deployed.
2. While recording, watch the container's memory: `docker stats --no-stream zrelay-zrelay-1` during a 10-minute MinIO outage with continuous triggers.
3. `docker compose -f deploy/compose.yaml logs zrelay | grep -c "backlog full"` after the outage to see how often the backlog policy fired.
**Expected result:** container memory stays under `max_memory_mb`; the CPU cost of the lower GC percent is not noticeable (`docker stats` CPU within a few percent of before); clips recorded in the outage arrive after MinIO returns, except those dropped as `backlog`.
**If the result differs, update:** `gcPercent` in `internal/relay/app/app.go` (higher if the CPU cost is visible, lower if memory is near the limit) with `TestRunTightensTheGCPercent` and `TestChaos`; the memory budget formula in design §8.1 and `config.Budget` if the live heap is larger than budgeted.
**Status:** not run

## Task 29 (chaos fix) — Breaker opens reliably during an outage while the backlog evicts
This section supersedes the chaos results of "Task 29" (270.0 MiB peak, "breaker opened 1 minute in") and the chaos result of "Task 29 (review fixes)"; a reviewer could not reproduce them (`TestChaos` failed twice at "timed out waiting for the breaker to open").
**Automated in this task:** Root cause: at 3 x 4 Mbps with a 128 MiB backlog the accountant evicts the oldest finalized clip about every 10 to 30 s, and that is the clip a drain worker is uploading, so eviction cancelled each request before the 30 s `s3ResponseTimeout` could fail it; `Breaker.Failure` was never reported. Fix in `internal/relay/egress/worker.go`: `Egress` tracks when each worker's current storage request started (`job.pendingSince`), and when the backlog policy cancels a clip whose request has been pending for at least `stallReport` (5 s) it reports a transient `Breaker.Failure`; a Write cancelled by eviction or shutdown is no longer reported as a storage failure by itself. Tests (`internal/relay/egress/stall_test.go`): `TestEvictionCancelsStalledRequestAndReportsBreakerFailure` (staging PutSmall and part upload, stalled 6 s versus 1 s) and `TestStalledEvictionsOpenTheBreaker`; both failed before the fix. `make chaos` with the full 10-minute outage, twice in a row, both passed. Run 1 (636 s): breaker opened 1 m 0 s into the outage and closed after it, 54 `backlog` drops in coverage, a clip committed after the outage, shutdown within the grace, `/memory/classes/total:bytes` peaked at 237.1 MiB of 256 MiB (total minus released 222.0 MiB). Run 2 (633 s): breaker opened 1 m 0 s in, 54 `backlog` drops, `total:bytes` peaked at 233.5 MiB (total minus released 224.5 MiB). Review fixes to tests: `TestRunForceFinalizesThroughTheBacklogPolicy` now sets max clip and trigger timeout to one hour and asserts two `egress: clip dropped ... reason=backlog` logs, which only the real force-finalize, suppress, resume cycle produces (it fails with the force send removed); `TestRunShutdownWithEventSourceInBackoffIsPrompt` uses a seam (`newEventSource`) to give the supervisor an event source that returns early, so it is asleep in its backoff at shutdown (it fails 5 of 5 with the `doneSource` idle-finish disabled; `TestRunShutdownWithONVIFDownIsPrompt` cannot, because the real `PullPoint.Run` only returns when its context ends). Not done: reserving part of the shutdown grace for the ONVIF unsubscribe (optional in the review); with a 30 s grace and storage down the unsubscribe step can still log "gave up waiting".
**Manual steps:**
1. On the Mac mini with the Synology MinIO, run the Task 29 manual step 5 (stop MinIO) for 10 minutes while the camera triggers continuously: `docker compose -f deploy/compose.yaml logs -f zrelay | grep -E "storage breaker|clip dropped"`.
2. Watch memory with `docker stats --no-stream zrelay-zrelay-1` during the outage.
**Expected result:** `storage breaker state=open` appears within about a minute of stopping MinIO even while `clip dropped ... reason=backlog` lines are being logged; `state=closed` follows after MinIO returns; container memory stays under `max_memory_mb`.
**If the result differs, update:** `stallReport` and `noteStall` in `internal/relay/egress/worker.go` with `stall_test.go`; `s3ResponseTimeout` in `internal/relay/app/app.go`; `TestChaos` thresholds in `internal/relay/app/chaos_integration_test.go`.

## Task 40 — Live view grid
**Automated in this task:** `web/src/lib/live/{whep.ts,tile.ts,LiveTile.svelte}` and `web/src/routes/live/+page.svelte`, with the Live and Key views in `web/src/App.svelte`. Vitest with a fake `RTCPeerConnection` and a fake worker client: recvonly video+audio transceivers, the offer sent only after ICE gathering completes, the answer applied, close() issuing `live.close`; hiding the tab closes the session and showing it reopens (a session that resolves after the tab was hidden is closed at once); 503 shows `limit` and keeps retrying; reconnect delays 1,2,4,8,16,30,30 s and reset after a good connection; unmute only after an audio track exists. Playwright `web/e2e/live.spec.ts` (Chromium): builds the SPA, runs the real `zview-server` over a `local` store with its live upstream set to the real `internal/relay/live` server fed by the in-process RTSP fixture (`internal/testutil/cmd/livefixture`, all ports ephemeral on 127.0.0.1), imports the throwaway key through the onboarding form, and asserts the tile reaches `live` with `video.videoWidth == 640`. Result: passed. WebKit, phones, real G.711 audio and a real camera were not exercised.
**Manual steps:**
1. On the deployed zview HTTPS URL, import the key, open Live from the top bar, and confirm each camera tile reaches the red "Live" badge on an iPhone (Safari and Home Screen app) and Android Chrome.
2. With a camera that sends G.711 audio (the Tapo C220), confirm an "Unmute" button appears on its tile only, tap it, hear audio, tap "Mute".
3. Switch to another tab or app for 30 s and return: the tile reconnects by itself. On the relay host, `curl -s http://127.0.0.1:9464/metrics | grep zrelay_live_viewers` drops to 0 while hidden.
4. Set `live.max_viewers: 1` in `deploy/zrelay.yaml`, restart zrelay, open Live on two devices: the second shows "Viewer limit reached" and connects after the first leaves.
5. `docker compose -f deploy/compose.yaml restart zrelay` while a tile is live: it shows "Reconnecting…" and recovers with 1, 2, 4 s delays.
**Expected result:** tiles reach `live` over Tailscale with a single ICE UDP port; hidden tabs release their viewer slot; the limit state is shown and recovers; unmute exists only for cameras that send audio; reconnect delays are bounded by 30 s.
**If the result differs, update:** no media over Tailscale: `live.advertise_ip` / `live.ice_udp_port` in `deploy/zrelay.yaml` and the `gatheringComplete` timeout in `web/src/lib/live/whep.ts`; iOS autoplay: `startWhep` (`playsInline`, muted, `play()`) and `LiveTile.svelte`; backoff or visibility behaviour: `createTile` in `web/src/lib/live/tile.ts` with `tile.test.ts`; audio codec negotiation: `internal/relay/live` (Task 11) and design §5.1.
**Status:** not run

## Task 30 — Metrics, /healthz, runbook, soak
**Automated in this task:** `internal/relay/metrics` (every relay-spec §4.12 metric plus the design §11 additions, the Go and process collectors the soak reads; `TestAllMetricsRegistered`, counters, scrape-time sources, the ONVIF `events.PullMetrics` adapter, a cap of 64 distinct unmatched-topic series), `internal/relay/httpapi` (`/healthz`, `/metrics`, GET only, default listen `127.0.0.1:9464`; `TestHealthz`, `TestHTTPListenDefaultLoopback`), the egress `UploadMetrics` hook (`internal/relay/egress/metrics_test.go`: bytes counted once even across a resume, errors by class, resumes), and the wiring in `internal/relay/app` (`TestRunServesMetrics`, `TestRunCountsClipAndUpload`, `TestRunMetricsShowStorageOutage` against the in-process RTSP and ONVIF fixtures; `TestHealthErr*` with a fake clock: unhealthy only once the breaker has been out of closed for more than 5 minutes, or while shutting down). `deploy/minio-relay-policy.json` and `deploy/minio-zview-policy.json` are applied to real MinIO users in Docker by `go test -tags integration ./deploy/`: `TestRelayPolicyDeniesArchiveDelete` (DeleteObject on `zrelay/clips/c/2026/01/01/x.json` is AccessDenied and the object survives; on `.x.json.part` it succeeds; writes outside the root are denied), `TestRelayPolicyWorksWithTheS3Driver` (the real S3 driver with the relay user: HeadBucket health check, staging sidecar, multipart upload and commit, recovery listings, staging delete; archive deletes fail), `TestZviewPolicyIsReadOnlyOnMinIO`. The policy tests found two MinIO limits that shaped the policy files (no `s3:prefix` condition on `ListBucketMultipartUploads`; HeadBucket has no prefix key, so a `Null` statement allows it, which also lets both users list key names bucket-wide). `deploy/runbook.md` covers design §8.3. Nothing in this entry was run against the Synology MinIO or the Mac mini.
**Manual steps:**
1. On the Synology MinIO, follow `deploy/runbook.md` sections 1 to 3 with your real names: `mc mb <alias>/cctv`, `mc version info <alias>/cctv`, `mc admin policy create <alias> zrelay-relay deploy/minio-relay-policy.json`, the same for `zrelay-zview`, `mc admin user add` and `mc admin policy attach` for both users, then the two `mc ilm rule add` lines.
2. Verify the relay user cannot delete an archive key, from any host that reaches MinIO: `mc alias set relay https://nas.<tailnet>.ts.net:9000 zrelay <relay-secret>`, then `echo '{}' | mc pipe relay/cctv/zrelay/clips/probe/2026/01/01/x.json`, `mc rm relay/cctv/zrelay/clips/probe/2026/01/01/x.json` (must fail with Access Denied), `echo '{}' | mc pipe relay/cctv/zrelay/clips/probe/2026/01/01/.x.json.part`, `mc rm relay/cctv/zrelay/clips/probe/2026/01/01/.x.json.part` (must succeed). Clean up with the admin alias: `mc rm <alias>/cctv/zrelay/clips/probe/2026/01/01/x.json`.
3. Deploy (`deploy/deploy.sh`) and on the Mac mini run `curl -fsS http://127.0.0.1:9464/healthz` and `curl -fsS http://127.0.0.1:9464/metrics | grep -E '^(zrelay_|process_resident_memory_bytes|go_goroutines)'`. Confirm `zrelay_camera_rtsp_up{camera="..."} 1` and `zrelay_camera_events_up{camera="..."} 1` for the real C220.
4. Trigger motion in front of the camera, wait for the clip, then re-run the `curl ... /metrics` line: `zrelay_clips_total{end_reason="post_roll_elapsed"}` and `zrelay_upload_bytes_total` increased, `zrelay_ring_bytes` is non-zero, `zrelay_gop_max_seconds` matches the camera's I-frame interval.
5. Open a live view on a phone and start `ffplay rtsp://<user>:<pass>@<tailscale-ip>:8554/<camera-id>`: `zrelay_live_viewers{camera}` and `zrelay_restream_clients{camera}` become 1 while open.
6. Stop MinIO for 6 minutes (`docker stop` on the NAS, or block port 9000 with the Synology firewall) while the camera triggers. Within about a minute `zrelay_storage_breaker_state` becomes 2 and `zrelay_storage_breaker_transitions_total{to="open"}` increases; `/healthz` stays 200 for the first 5 minutes and answers 503 afterwards (`curl -i http://127.0.0.1:9464/healthz`). Start MinIO again: the state returns to 0, `/healthz` is 200 and the queued clips arrive.
7. **Soak (72 hours):** follow `deploy/runbook.md` section 12. Start the sampler (`~/zrelay-soak/sample.sh`, every 900 s it records `process_resident_memory_bytes`, `go_goroutines`, `docker inspect` restart count and start time to `~/zrelay-soak/soak.csv`), keep the stack running with `read_only: true`, keep triggering motion and keep one live view open for an hour a day. After 72 hours, run the summary commands in section 12 and fill in the table under "Soak results" in `deploy/runbook.md`.
**Expected result:** steps 1 and 2: the relay user deletes only dot-named staging objects. Step 3: `/healthz` returns `ok`; the camera series are 1. Step 4: counters move as described. Step 5: both viewer gauges read 1 while a session is open. Step 6: the breaker opens within about a minute, `/healthz` flips to 503 only after more than 5 minutes of outage and recovers by itself. Step 7: zero container restarts and a single start time over the 72 hours (a write to the read-only root would have crashed the process), maximum RSS below `max_memory_mb` with no steady climb (median of the last 6 hours within 10% of hours 6 to 12), goroutines flat (last-6-hour maximum within 20 of the first day), no `read-only file system` log lines.
**If the result differs, update:** a policy action MinIO on the Synology rejects or an unexpected AccessDenied: `deploy/minio-relay-policy.json` / `deploy/minio-zview-policy.json`, the "MinIO quirks" paragraph of runbook section 2, and `TestRelayPolicyWorksWithTheS3Driver` (a MinIO release that restricts `HeadBucket` or the prefix conditions differently); a camera series stuck at 0 or a `PullMetrics` count that does not move: the adapter wiring in `startCamera` and `OnState` in `internal/relay/app/app.go`; `/healthz` timing: `healthBreakerOpen` and `healthErr` in `internal/relay/app/app.go` with `TestHealthErr*`; RSS growth or a goroutine climb in the soak: the component named by `go_goroutines` and the memory budget in design §8.1 and `config.Budget`, then `gcPercent` in `internal/relay/app/app.go` (with `TestRunTightensTheGCPercent`); any restart: the container exit code and logs, which show the failing write.
## Task 36 — Playback engine and demo page
**Automated in this task:** `web/src/lib/player/{buffer,mse,blob,engine,advance,status}.ts` and the demo page `web/src/routes/demo/+page.svelte` (`#/demo?clip=<id>`). Vitest: the buffer-window, eviction, seek and failure-time planners as pure functions; the engine against a fake MediaSource/ManagedMediaSource/SourceBuffer and fake worker (capability order, sequential play, ahead/behind windows, QuotaExceededError retry once, `endstreaming`/`startstreaming`, seek inside and outside the buffer including the intra-chunk offset and a stale read dropped, tampered/incomplete/unknown-key/mismatch states, Blob size limit and URL LRU, auto-advance series-first across midnight); the demo page in jsdom. Test fixtures: a real 34.3 s clip recorded through `app.Run` (fake RTSP camera and ONVIF trigger, `s3` driver against a throwaway MinIO container from `storagetest.StartMinIO`, so the video went through the S3 multipart commit; the committed objects are copied out with GETs into the `local` driver's layout) with the Contract §4.5 vector key, plus a tampered and a truncated variant, under `testdata/clips/` (regenerate with the command in `testdata/clips/README.md`). Playwright `web/e2e/playback.spec.ts` on Chromium and WebKit (Playwright builds, Chromium 156 and WebKit 27.2 on macOS) against the real `zview-server`: the clip plays and stays verified, seeks to 20 s within 2 s, a flipped bit in chunk 18 shows `tampered` at 16 s and playback never passes it, the truncated clip plays and shows `incomplete`, a phone profile (touch, 390 px) seeks near the end of the clip, and with MediaSource removed the Blob fallback plays and seeks. WebKit used ManagedMediaSource. Review fixes: a backward seek outside the buffer no longer evicts its own fragment (the buffer window follows the seek target while the seek settles; engine unit test and a phone-profile backward seek in the e2e), a cached tampered or incomplete Blob resolves `load()` with its outcome, and seeks below the failure point of a tampered clip still fetch. Result: passed. Not exercised: a 300 s clip, Safari proper, iPhone, real `deviceMemory`.
**Manual steps:**
1. Record a 300 s clip on the real camera: on the relay host set `defaults.max_clip_seconds: 300` and `defaults.post_roll_seconds: 5` in `deploy/zrelay.yaml`, restart zrelay, and keep the camera's motion trigger active (walk in front of it) for about 5 minutes. Note the clip id from `GET /api/v1/clips` on the zview URL (or the file name in MinIO).
2. On the deployed zview HTTPS URL, import the key (`#/key`), then open `https://<zview-host>/#/demo?clip=<clip id>` on Chrome desktop, Safari macOS and an iPhone (Safari, and the Home Screen app).
3. On each: confirm the green "Verified" line, that video plays from 0:00, drag the native seek bar to about 150 s and to about 290 s (each must resume in about 2 s), let it play to the end, and tick "Autoplay next" to see it move to the next clip if one exists.
4. Record the engine and profile the page chose: `document.querySelector('[data-testid=demo]').dataset` in the console (`engine` is `mms`, `mse` or `blob`; `profile` is `mobile` or `desktop`). The iPhone should show `mms` and `mobile`, Safari macOS `mms` or `mse` and `desktop`.
5. While playing and after two seeks, read peak JS heap (Chrome: DevTools Performance monitor; Safari: Web Inspector Timelines) and the buffered span (`v=document.querySelector('[data-testid=player]'); [...Array(v.buffered.length).keys()].map(i=>[v.buffered.start(i),v.buffered.end(i)])`). Write the numbers into `docs/runbook-notes/playback-memory.md`.
6. Negative checks on the iPhone: serve a copy of `testdata/clips/tamper` (then `testdata/clips/incomplete`) with a scratch `zview-server` on the tailnet (`storage.driver: local`, `local.base_dir: <copy>`, `storage.root: zrelay`, `api_token_sha256` from `testdata/zrec/v1/vectors.json` `derived.k_api_sha256_hex`), import the manifest's `master_key`, and open `#/demo?clip=<clip_id from testdata/clips/manifest.json>`. It must show "failed verification at ~00:16" and "Recording was interrupted".
**Expected result:** all three browsers play the 300 s clip and seek in about 2 s; the iPhone uses ManagedMediaSource and the mobile window (30 s ahead, 15 s behind), so its buffered span stays near 45 s and the plaintext in memory stays at or under 32 MiB at 3 Mbps; desktop stays at or under 64 MiB (90 s ahead, 30 s behind). The iPhone resumes streaming after the screen locks or the app is backgrounded (ManagedMediaSource `startstreaming`). The tamper and truncated fixtures show the states of ui-spec §4.4.
**If the result differs, update:** window sizes in `WINDOWS` and the profile rule in `chooseProfile`/`detectProfile` (`web/src/lib/player/buffer.ts`, ui-spec §4.3.2 table) with `buffer.test.ts`; MMS handling (`openSink`, the `startstreaming`/`endstreaming` handlers and the initial `streaming = true`) in `web/src/lib/player/mse.ts` with `engine.test.ts`; the seek ordering (playhead is moved only after the fragment is appended, because WebKit otherwise settles on the nearest buffered time) in `PlayerImpl.seek` in `web/src/lib/player/engine.ts`; the Blob limits in `BLOB_MAX`/`BLOB_LRU`; `playback-memory.md` with the measured numbers.
**Status:** not run

## Task 36 (review fix) — Native seek bar scrubbing and overlapping seeks
**Automated in this task:** `PlayerImpl` in `web/src/lib/player/engine.ts` now routes the `<video>` element's own `seeking` events through the seek planner: a scrub (native seek bar, `currentTime = x`) to an unbuffered time aborts the parser, fetches the `seek_index` chunk for that time, evicts what is ahead of it and stops at the profile's ahead window; the tampered clamp applies to it. The engine's own `currentTime` assignment plans as buffered and does not re-enter, and a `seeking` event for the fragment a fetch seek is already loading (the browser clamps the playhead to the buffer end until the fragment completes) is not treated as a new scrub. Every `seek()` takes a token, so a fetch seek that is still settling no longer overrides a later seek. Vitest `engine.test.ts` (backward and forward scrubs, scrub inside the buffer, no re-entry, tampered clamp without looping, scrub before the first append, overlapping seeks) and `web/e2e/playback.spec.ts` (phone profile: a direct `currentTime` write back to 3 s after playing to the end, on Chromium and WebKit) passed. Not exercised: Safari proper, iPhone, real touch scrubbing of the native control.
**Manual steps:**
1. Follow Task 36 step 3 on an iPhone (Safari) and on Safari macOS with the 300 s clip, but scrub with the native seek bar: drag from 0 s to about 150 s, then to about 290 s, then back to about 10 s, without touching the page's Seek field.
2. After each drag, read `v=document.querySelector('[data-testid=player]'); [...Array(v.buffered.length).keys()].map(i=>[v.buffered.start(i),v.buffered.end(i)])` (Web Inspector console, or Safari remote inspector for the iPhone) and watch the Network panel for `Range` requests to the clip.
3. Drag the native seek bar again while the previous drag is still loading (two quick drags), and confirm playback lands at the second position.
**Expected result:** each drag resumes within about 2 s; the first `Range` request after a drag is for the chunk containing the target (not a sequence from the old position); `buffered` holds the target and stops about 30 s (mobile) or 90 s (desktop) ahead of it, never stretching from the old position to the target; no request loop appears when the drag lands near the end of the clip; the second of two quick drags wins.
**If the result differs, update:** `PlayerImpl.#nativeSeek`, `#onTime`/`#fetching` and `seek()` in `web/src/lib/player/engine.ts` (for example the end-of-buffer clamp rule if Safari reports a different `currentTime`), with the matching cases in `web/src/lib/player/engine.test.ts` and the phone-profile test in `web/e2e/playback.spec.ts`; the seek ordering note in Task 36 above.
**Status:** not run

## Task 36 (review fix 2) — A seek inside the buffer supersedes a settling fetch seek
**Automated in this task:** `PlayerImpl` in `web/src/lib/player/engine.ts`: when a seek lands inside the buffer (through `seek()` or the native `seeking` event in `#nativeSeek`) while an earlier fetch seek is still settling, the new `#resumeBuffered` bumps the seek token (the settling seek never moves the playhead), bumps the generation (its fill stops), clears the settling state, resets the parser, removes what lies beyond the ahead window and starts a sequential fill from the fragment at the end of the buffered range holding the target (new pure planner `planResume` in `web/src/lib/player/buffer.ts`). Vitest: `buffer.test.ts` (`planResume`), `engine.test.ts` native double scrub (second drag wins, `[0,30]` kept, no append or read from chunk 70 on) and direct `seek(70_000)` then `seek(5000)` (playing on to 29 s buffers contiguously about 30 s ahead, no reads at or above 70). `make vet test`, `make web` (421 tests) and `make web-e2e` (Chromium and WebKit, 20 passed, 2 skipped) passed. Not exercised: Safari proper, iPhone, real touch scrubbing.
**Manual steps:**
1. On an iPhone (Safari) and Safari macOS with the 300 s clip from Task 36 step 1, open `https://<zview-host>/#/demo?clip=<clip id>` and let it buffer from 0 s.
2. Drag the native seek bar to about 200 s and, before video appears (throttle with Web Inspector Network conditions or do it immediately), drag it back to about 5 s. Repeat with the second drag ending at a point already buffered.
3. After each pair, read `v=document.querySelector('[data-testid=player]'); [...Array(v.buffered.length).keys()].map(i=>[v.buffered.start(i),v.buffered.end(i)])` and watch the Network panel for `Range` requests; let it play for 40 s.
**Expected result:** playback lands at the second position (about 5 s); `buffered` stays one range around it, never gaining a second range near 200 s; after the second drag no `Range` request goes to chunks near 200 s; playback continues past the old buffer end without stalling and `buffered` stops about 30 s (mobile) or 90 s (desktop) ahead of the playhead.
**If the result differs, update:** `PlayerImpl.#resumeBuffered`, `#nativeSeek` and `seek()` in `web/src/lib/player/engine.ts`, `planResume` and its slack in `web/src/lib/player/buffer.ts`, with the matching cases in `buffer.test.ts` and `engine.test.ts` (`overlapping seeks`).
**Status:** not run

## Task 36 (review fix 3) — One seek path: resume from the fragment holding the range end, re-aim the fill, trim on fragment starts
**Automated in this task:** `web/src/lib/player/engine.ts` now has one seek path, `PlayerImpl.#move(tMs, native)`, used by `seek()` and by the `<video>` `seeking` event. One token (`#seekTok`) makes the latest move win, one generation (`#gen`) stops stale fills, and every position (fetch start, resume point) comes from `planSeek`/`planResume` over `seek_index`. Fixes: `planResume` resumes from the fragment holding the end of the buffered range (the last `seek_index` entry at or before the end, 0.1 s slack) instead of the next one, and is null only when the stream has ended and no range follows; `feedsRange` re-aims the fill (abort, trim, resume) when a move lands in a range the running fill is not carrying, not only while a fetch seek is settling; `alignToFragments` makes the ahead trim start on a fragment start so no fragment is left cut; the resume point is planned after the trim; queued `SourceBuffer.remove` calls are visible to `buffered()` (`Sink.buffered` in `mse.ts`) and a stale fill stops issuing removals; a native scrub inside the fragment of a still-settling seek retargets instead of being swallowed; `Sink.ended` added. Tests: `buffer.test.ts` (`planResume` mid-fragment, last fragment, fixture mid-chunk offsets, `feedsRange`, `alignToFragments`); `engine.test.ts` double scrub with a fragment every 4 chunks (native and direct), a table of seek sequences and 66 seeded random sequences (forward/backward, buffered/unbuffered, `seek()`/native scrub, reads held so a seek arrives while another settles) over a fake clip whose fragment layout is the committed fixture's `seek_index` (2 s fragments at mid-chunk offsets, 64 KiB chunks) with a SourceBuffer that parses fragments like the real one (`web/test-support/player-fakes.ts`); the invariants checked are: latest seek wins, playhead ends at the latest target, target buffered and contiguous to the ahead window, no parser violations, no stale reads, no buffered range past the playhead's. A 6000-seed run of the random sequences passed (`SEEK_SEEDS=6000 pnpm vitest run src/lib/player/engine.test.ts`). `make vet test`, `make integration`, `make web` (541 tests) and `make web-e2e` (Chromium and WebKit, 20 passed, 2 skipped) passed. Not exercised: Safari proper, iPhone, real touch scrubbing, real MediaSource removal semantics at a mid-GOP cut.
**Manual steps:**
1. On an iPhone (Safari) and Safari macOS with the 300 s clip from Task 36 step 1, open `https://<zview-host>/#/demo?clip=<clip id>` and let it buffer from 0 s (do not play).
2. Drag the native seek bar to about 200 s and, before video appears (throttle with Web Inspector Network conditions), drag back to about 5 s. Let it play for 40 s. Repeat with the first drag to about 120 s and the second to about 20 s.
3. Play to about 29 s, then scrub to 150 s, wait for it to settle, scrub to about 25 s (inside what remains of the old buffer) and let it play for 40 s.
4. After each step read `v=document.querySelector('[data-testid=player]'); [...Array(v.buffered.length).keys()].map(i=>[v.buffered.start(i),v.buffered.end(i)])` and watch the Network panel for `Range` requests to the clip.
5. Let the 300 s clip play to its end with autoplay-next off and confirm it reaches `ended`.
**Expected result:** playback lands at the latest position and continues past 30 s without stalling; `buffered` is one contiguous range around the playhead (no hole of a fraction of a second at the point where the old window was trimmed) stopping about 30 s (mobile) or 90 s (desktop) ahead; after the second drag no `Range` requests go towards the far position or the end of the clip; the first request after a resume is for the chunk holding the fragment that contains the end of the buffered range (a few hundred KiB of overlap re-read at most); the clip reaches `ended`.
**If the result differs, update:** `PlayerImpl.#move`, `#resumeBuffered`, `#evictAhead`, `#evictBehind` in `web/src/lib/player/engine.ts`; `planResume` and its `END_SLACK`, `feedsRange`, `alignToFragments` in `web/src/lib/player/buffer.ts` (for example the slack if Safari reports range ends further from the fragment start, or `alignToFragments` if a mid-GOP `remove` there leaves an undecodable tail); `MseSink.buffered`/`remove`/`ended` in `web/src/lib/player/mse.ts`; with the matching cases in `buffer.test.ts`, `engine.test.ts` (sequences and regression seeds) and the fake parser in `web/test-support/player-fakes.ts`.
**Status:** not run

## Task 36 (review fix 4) — The playhead follows a seek only once its whole fragment is buffered
**Automated in this task:** the Safari/iPhone engine flaked on a backward seek after the clip had ended: the fetch seek set `currentTime` after the first appended chunk although the target's 2 s fragment spans several chunks, so WebKit settled on the end of the partial range and later reported the end of the clip; the still-running fill then trimmed the target and refilled towards the end. `PlayerImpl.#move` in `web/src/lib/player/engine.ts` now keeps the seek's fill running (regardless of the window) until the target's whole fragment has been appended (`appendedTo >= fragmentEnd`; the buffered ranges are not consulted, because WebKit reports a part-appended fragment as buffered time; the fill also resolves when it ends first), and only then assigns `currentTime` [corrected in "review fix 5": the first version of this fix also accepted `isBuffered(target)`, which WebKit satisfies after the first chunk]. `#seekHead` (the window head) stays at the target after the assignment until a `seeked` or `timeupdate` reports a time inside the target's buffered range and that range reaches the end of the target's fragment (`playheadSettled` and `fragmentComplete` in `buffer.ts`); a time outside it without a `seeking` event, and the `seeking` raised by the engine's own assignment, are not scrubs. Tests first (red, then green): `buffer.test.ts` `playheadSettled` (4 cases); `engine.test.ts` over the fixture layout: a fetch seek whose fragment spans chunks leaves `currentTime` alone until buffered, a seek after the clip ended with a partial first chunk survives `currentTime` jumping to the duration without `seeking` (target never evicted, no read beyond 30 s ahead of 2 s, `currentTime` ends at 2), and the window follows the playhead again once it reports a time in range. Results: `make vet test` passed; `pnpm lint`, `pnpm check`, `pnpm test` (548 tests) passed; the random seek sequences passed with `SEEK_SEEDS=1500`; `pnpm exec playwright test e2e/playback.spec.ts --project=webkit -g 'phone profile' --repeat-each=30 --workers=1` 30 of 30 passed (previously 1 in 6 to 1 in 24 failed); full `make web-e2e` (Chromium and WebKit) 20 passed, 2 skipped. Not exercised: Safari proper, iPhone, real touch scrubbing.
**Manual steps:**
1. On an iPhone (Safari) and Safari macOS with the 300 s clip from Task 36 step 1, open `https://<zview-host>/#/demo?clip=<clip id>` and play it to its end (`v.ended === true`).
2. Seek back to about 2 s with the page's Seek field, then with the native seek bar. Repeat ten times, alternating.
3. After each seek read `v=document.querySelector('[data-testid=player]'); [v.currentTime, ...[...Array(v.buffered.length).keys()].map(i=>[v.buffered.start(i),v.buffered.end(i)])]` and watch the Network panel for `Range` requests.
**Expected result:** `currentTime` is about 2 s (never the clip end) after each seek and stays there when paused; `buffered` is one range starting at about 2 s and ending about 30 s (mobile) or 90 s (desktop) ahead; no `Range` request goes towards the end of the clip after the seek; no second `ended` event.
**If the result differs, update:** `PlayerImpl.#move`, `#confirmSeek`, `#fill` (the `seek.done` rule) in `web/src/lib/player/engine.ts` and `playheadSettled` in `web/src/lib/player/buffer.ts` (for example the 0.25 s start slack if Safari reports a time further before the target), with the matching cases in `buffer.test.ts` and `engine.test.ts` (`seek sequences over the fixture layout`).
**Status:** not run

## Task 36 (review fix 5) — The playhead follows a seek only once its fragment is fully appended (WebKit partial ranges)
**Automated in this task:** the previous fix still assigned `currentTime` after the first chunk of a multi-chunk fragment on WebKit, because the seek's `done` rule also accepted `isBuffered(target)` and WebKit reports a part-appended fragment as buffered time. Now `PlayerImpl.#move` (`web/src/lib/player/engine.ts`) resolves the seek's fill only when `appendedTo >= fragmentEnd` (every byte of the target's fragment appended; the fill's `finally` still resolves it when the clip ends first, as for the last fragment), so `currentTime` is never assigned while the fragment is partial. `#confirmSeek` ends the seek (returns the window to the browser's playhead) only when `playheadSettled` holds and the new `fragmentComplete` (`buffer.ts`: the range holding the target reaches the next fragment start, or the stream has ended for the last fragment), so a clip-end report after an early in-range `timeupdate` cannot move the window. Test support: `FakeSourceBuffer` has an opt-in WebKit mode (`installMedia({webkit: true})`; a part-appended fragment adds `[fragStart, fragStart + proportion]` buffered time per append and `abort()` keeps it). Tests first (red, then green): `buffer.test.ts` `fragmentComplete` (5 cases, failing before the function existed); `engine.test.ts` the two fixture-layout seek tests and the window-follows-playhead test now run in both modes (the WebKit-mode seek tests failed against the old engine: `currentTime` was assigned after the first chunk), and every row of the seek-sequence table and every random seed runs in both modes. Results: `make web` (lint, check, 655 tests) passed; `SEEK_SEEDS=1500 pnpm test src/lib/player/engine.test.ts` 3136 passed; `pnpm exec playwright test e2e/playback.spec.ts --project=webkit -g 'phone profile' --repeat-each=30 --workers=1` 30 of 30 passed; `make web-e2e` 20 passed, 2 skipped; `make vet test` passed. Not exercised: Safari proper, iPhone, real touch scrubbing.
**Manual steps:** the same as "review fix 4" (ten backward seeks to about 2 s after the clip has ended, alternating the page's Seek field and the native seek bar, on an iPhone and Safari macOS with the 300 s clip). Additionally, in the Web Inspector timeline, check that `video.currentTime` is not assigned until the `Range` request for the last chunk of the target's fragment has completed.
**Expected result:** as in "review fix 4": `currentTime` about 2 s (never the clip end) after each seek and while paused; one `buffered` range starting at about 2 s and ending about 30 s (mobile) or 90 s (desktop) ahead; no `Range` request towards the clip end after the seek; no second `ended` event.
**If the result differs, update:** `PlayerImpl.#move` (the `done` rule), `PlayerImpl.#confirmSeek` in `web/src/lib/player/engine.ts`, `playheadSettled`/`fragmentComplete` (the 0.25 s start slack and 0.1 s end slack) in `web/src/lib/player/buffer.ts`, and the WebKit mode of `FakeSourceBuffer` in `web/test-support/player-fakes.ts` if Safari reports partial ranges differently, with the matching cases in `buffer.test.ts` and `engine.test.ts` (`seek sequences over the fixture layout`).
**Status:** not run

## Task 39 — Timeline dashboard
**Automated in this task:** `web/src/routes/+page.svelte` (the timeline page, now the default view of `App.svelte`; `#/timeline`) with `Header`, `EventList` (virtualized, 56 px rows), `Player` (wraps `createPlayer`, owns the Export button and `ExportDialog`), `IntegrityBadge`, `StatusDot` and `ExportDialog` in `web/src/lib/components/`; `web/src/lib/export.ts` (`exportFilename`, `canExport`, `exportClip`: `showSaveFilePicker` streaming, Blob fallback within `BLOB_MAX`, `navigator.share` on iOS, aborted file on a failed chunk); `relayStatus.ts` (red when `updated_at` is older than 3 min or a camera's `rtsp` is `down`, amber for degraded cameras, a down event stream or a storage breaker that is not `closed`, grey when `/api/v1/status` cannot be read); `shortcuts.ts` (Space, Left/Right ±10 s, `J`/`K`, `[`/`]`, `T`); `dashboard/data.ts` (one local day of `/api/v1/clips` and per-camera `/api/v1/coverage`). `Timeline.svelte` gained `stepZoom`, `jumpToNow` and `selectedClipId`. Tests first (red, then green): filename in WIB, export disabled when tampered, status dot red at 4 min, `J`/`K` selection, list virtualization, the unencrypted-copy wording, picker/Blob/share paths, filters and day queries (`export.test.ts`, `relayStatus.test.ts`, `shortcuts.test.ts`, `dashboard/data.test.ts`, `components/components.test.ts`, `routes/page.test.ts`). `make web` (lint, check, 746 tests), `make web-e2e` (Chromium and WebKit, 28 passed, 2 skipped, including the new `e2e/dashboard.spec.ts`: desktop layout, 400-clip list window, `J`/`K`, red dot, phone menu, bottom sheet and pinned player) and `make vet test` passed. Not exercised: a real key and server, the real file picker, `navigator.share`, a real iPhone or Safari.
**Manual steps:**
1. Run the stack from the Task 36 live check (a real zview-server with at least three clips from today, one `incomplete`) and open `https://<zview-host>/` on desktop Chrome with the key imported. Pick a camera, a date and a trigger in the header; confirm the list and timeline follow.
2. Press `K`, `K`, `J`, `Space`, `ArrowRight`, `ArrowLeft`, `]`, `[`, `T` with the focus on the page body.
3. Select a verified clip, press Export, read the dialog, press Save, choose a folder. Repeat in Safari macOS (Blob download) and on an iPhone (share sheet) with a clip under 48 MiB. Repeat with an `incomplete` clip.
4. Open a tampered clip (flip one ciphertext byte in MinIO with `mc cp` of a modified copy into a scratch bucket served by a test zview) and look at the Export button.
5. On an iPhone, open the page, open the Menu, open the Events sheet, scroll the page and watch the player.
6. Stop the relay for 4 minutes (`docker stop zrelay`), reload; then cut the camera's RTSP and wait a minute; then block MinIO from the relay (`iptables` on the Synology or `docker network disconnect`) and wait for the breaker to open. Hover the dot each time.
**Expected result:** list and timeline show the chosen day in the local zone (UTC+7 for WIB) newest first; `K`/`J` select the next/previous clip in time and the player loads it, `Space` toggles playback, arrows move ±10 s, `[`/`]` change the timeline zoom, `T` centres on now; the dialog says "This saves an **unencrypted** copy on this device." and the file is named `<camera_id>_<local YYYYMMDD-HHMMSS>.mp4` (with `-incomplete` before `.mp4` for an interrupted clip), plays in VLC, and playback of the clip resumes where it was after saving; Export is disabled with a title for the tampered clip; on the phone the header folds into Menu, the player stays pinned and the events open as a bottom sheet; the dot is red after 4 minutes without a status update and when the camera is down, amber with the breaker state in the tooltip when MinIO is unreachable (a relay that cannot reach storage cannot publish `storage`, so expect red/stale after 3 minutes there), and the tooltip shows `since`.
**If the result differs, update:** `exportClip`/`browserExportEnv` in `web/src/lib/export.ts` (for example the iOS detection, or `canShare` when Safari refuses `video/mp4` files) with `export.test.ts`; `relayHealth` in `web/src/lib/relayStatus.ts` (which camera states count as red, the breaker state names `closed`/`open`/`half_open`) with `relayStatus.test.ts`; `commandFor` in `web/src/lib/shortcuts.ts`; the sticky/bottom-sheet classes in `web/src/routes/+page.svelte` and `e2e/dashboard.spec.ts`; `Player.svelte` `confirmExport` if reopening the clip after an export is not needed or loses the position; ui-spec §4.7/§4.8 if the intended behaviour changes.
**Status:** not run

## Task 39 review fix — export of an incomplete clip, shortcuts behind the export dialog
**Automated in this task:** `exportClip` (`web/src/lib/export.ts`) now stops at the first `ErrTruncated` of an `incomplete` clip and saves the verified chunks (picker: file closed; Blob: built from the gathered chunks) under the `-incomplete` name; `ErrAuth` and every other error still abort the file, and a truncation of a clip not marked incomplete, or before any verified byte, still fails (`export.test.ts`, with a `FakeClient` that throws an `ErrTruncated`-named error on the last chunk). The page ignores all shortcuts while an `[aria-modal="true"]` dialog is open, and `ExportDialog` traps Tab (`routes/page.test.ts`, `components/components.test.ts`). New tests drive `Timeline.stepZoom`, `jumpToNow` and `selectedClipId` (`timeline/Timeline.test.ts`), `Player.togglePlay` and `skip` (`components.test.ts`), the page's Space, arrows, `[`, `]`, `T` (also from another day) and the transport buttons (`routes/page.test.ts`), `browserExportEnv` in jsdom (`exportEnv.test.ts`) and the `App.svelte` routes (`App.test.ts`). `make vet test` and `make web` (lint, check, 808 tests) and `make web-e2e` (28 passed, 2 skipped) passed.
**Manual steps:**
1. With the stack of the Task 36 live check, find a clip whose sidecar says `status: incomplete` (stop the relay mid-clip with `docker stop zrelay`, or use one produced by the Task 24 chaos run). Open `https://<zview-host>/` in desktop Chrome with the key imported, select it, press Export and Save.
2. Repeat in Safari macOS (Blob download) and on an iPhone (share sheet).
3. Play both saved files in VLC and compare their length with the "Recording was interrupted" point in zview.
4. Open the export dialog on a verified clip, press Tab and Shift+Tab repeatedly, then `J`, `K`, `Space`, `ArrowLeft`, `ArrowRight`, `]`, `T`, then click the dark backdrop and press the same keys.
**Expected result:** the file is `<camera_id>_<local YYYYMMDD-HHMMSS>-incomplete.mp4`, is not empty, plays in VLC up to the interruption, and the dialog closes with no error; focus cycles between Cancel and Save and never leaves the dialog; with the dialog open (also after a backdrop click) none of the keys changes the selected clip, playback or timeline zoom.
**If the result differs, update:** `exportClip` in `web/src/lib/export.ts` (the `ErrTruncated` handling, for example if the real worker reports the truncation as a different error name or only after the last good chunk) with the `FakeClient` in `export.test.ts`; `trapTab` in `web/src/lib/components/ExportDialog.svelte` and the modal guard in `onkeydown` of `web/src/routes/+page.svelte` with their tests; ui-spec §4.8 if the intended behaviour changes.
**Status:** not run

## Task 41 — PWA and end-to-end
**Automated in this task:** `web/src/sw.ts` (precaches the hashed shell only; `fetch` handler answers only same-origin GETs of precached `/assets/*` files, never `/api` or `/live`, never writes data to Cache Storage; `skipWaiting` only on a `SKIP_WAITING` message; activation drops older `zview-shell-*` caches), `web/vite-sw-plugin.ts` (compiles it to `/sw.js` with the build's hashed files baked in), `web/src/lib/pwa/` (`shell.ts`, `update.ts`, `register.ts`, `UpdatePrompt.svelte`: "New version available — reload", button held while a video plays, reload only for a takeover the page asked for), `web/static/manifest.webmanifest` and icons (`web/scripts/gen-icons.py`), manifest and icon links in `index.html`, and `zview-server` now serves `.webmanifest` as `application/manifest+json`. `livefixture` gained `--key ENV:NAME` (a ref, never a literal) so live video can share the vector key of the fixture clips. Tests first (red, then green): `sw.test.ts`, `lib/pwa/{shell,update,UpdatePrompt}.test.ts`, Go `TestAppServesManifestWithItsMediaType`, `TestLiveFixtureKeyRef`, `TestLiveFixtureKeyRefRejected`, and `web/e2e/firstrun.spec.ts` on Chromium and WebKit: a fresh profile pastes the Contract §4.5 vector key, the day view lists the fixture clip, it plays, seeks and exports (Chromium: stubbed `showSaveFilePicker`, full plaintext size and `ftyp` checked; WebKit: Blob download and file name), the Live tab tile reaches `live` with 640 px video on Chromium; zero `securitypolicyviolation` events (the policy is Report-Only); an instrumented `Worker` wrapper records every message both ways and shows the master key, its hex, `K_api` and the raw key bytes appear nowhere except the one outgoing `key.import`; after playback `caches.keys()` holds only `/assets/<name>-<hash>` entries, no `/api` or `/live`, and no `/api` response came from the service worker; the manifest, icons, theme colour and `sw.js` (`Cache-Control: no-cache`) are served. Not exercised: a real iPhone or Android install, the update prompt against a real second deploy, WebKit live video (Playwright's WebKit cannot complete loopback ICE; the tile is only checked to exist and not to show the viewer limit), Lighthouse.
**Manual steps:**
1. Deploy the SPA and `zview-server` behind `tailscale serve` HTTPS (design D18). On an Android phone open `https://<zview-host>/` in Chrome, import the key, then use the menu "Install app". On an iPhone open it in Safari, Share, "Add to Home Screen".
2. Launch from the home screen icon; check the icon, the splash colour and that there is no browser chrome. Open the Timeline, play a clip, open Live.
3. In desktop Chrome DevTools, Application tab: Manifest shows no errors and an installable icon set; Service Workers shows `sw.js` activated; Cache Storage lists only `zview-shell-<hash>` with `/assets/...` entries. Network tab: `/api/v1/*` and `/live/*` requests are not marked "(ServiceWorker)". Console: no `Content-Security-Policy-Report-Only` violation messages while using Timeline, Live and Export.
4. Update flow: rebuild the SPA with a visible change (`cd web && pnpm build`, redeploy `app_dir`), reload the installed app twice. While a clip plays confirm the banner "New version available — reload" appears with its button disabled and "Pause the video to update."; pause, press Reload, confirm the new version loads and the old cache `zview-shell-<old hash>` is gone.
5. Run Lighthouse (DevTools, Mobile, categories PWA and Performance) against the deployed URL.
6. Airplane mode: reopen the installed app; the shell loads but the Timeline shows "Could not load" (data is never cached, by design).
**Expected result:** installable on both phones with the zview icon; Cache Storage never contains an `/api` or `/live` URL, a clip, a sidecar or a key; no CSP report-only violations; the update banner never lets the new version take over while a clip plays and takes over only after the Reload click; Lighthouse PWA installability checks pass; offline shows an error for data, not stale recordings.
**If the result differs, update:** `web/static/manifest.webmanifest` and `web/scripts/gen-icons.py` (for example a maskable icon rejected by Lighthouse or iOS ignoring the icon: `apple-touch-icon.png` and the `index.html` links) with the manifest test in `e2e/firstrun.spec.ts`; `installServiceWorker` in `web/src/sw.ts` and the `HASHED` pattern in `web/src/lib/pwa/shell.ts` if a real build names files differently, with `sw.test.ts` and `shell.test.ts`; `createUpdater`/`UpdatePrompt.svelte` if a browser reports a waiting worker differently (for example iOS standalone mode), with `update.test.ts` and `UpdatePrompt.test.ts`; the Report-Only policy in `internal/zview/app.go` (`contentSecurityPolicy`) for any violation seen, with `app_test.go`; ui-spec §4.9 if the intended behaviour changes.
**Status:** not run

## Task 42 — Enforce CSP and finish hardening
**Automated in this task:** `internal/zview/app.go` now sends the ui-spec §2.4 policy (with `connect-src 'self'`, design §7.1) as the enforcing `Content-Security-Policy` header on every response (app shell, `sw.js`, `/healthz`, `/api`, 401, 404, 405) and no longer sends `Content-Security-Policy-Report-Only`. Tests first (red, then green): `TestCSPEnforced` (exact header value, Report-Only absent) and `TestCSPEnforcedOnMethodNotAllowed` in `internal/zview/csp_test.go`; `app_test.go` assertions moved from the Report-Only header to the enforcing one. `web/e2e/firstrun.spec.ts` now also asserts the shell carries the enforcing header, and its existing zero-`securitypolicyviolation` checks now prove nothing is blocked. `pnpm exec playwright test` passes on Chromium and WebKit (34 passed, 2 skipped, the WebKit live-video cases) against the real zview-server with the policy enforced. `docs/family-onboarding.md` (install Tailscale, open the URL, Add to Home Screen, paste or scan QR) was written against the actual labels in `Onboarding.svelte`. Not run: Lighthouse. A local `pnpm dlx lighthouse` run against a locally started zview-server was not permitted in the implementing session (it downloads and executes a remote package), so no score exists.
**Manual steps:**
1. On a machine with Chrome, against the deployed URL (the `tailscale serve` HTTPS host of zview-server, from a device on the tailnet): `pnpm dlx lighthouse https://mini.<tailnet>.ts.net/ --only-categories=best-practices,performance,accessibility --view` (current Lighthouse has no PWA category; use the installability audit in step 2). Note the scores.
2. In Chrome DevTools, Application tab: Manifest shows "Installable" with no errors, and Service Workers shows `sw.js` activated with no errors in the console.
3. Console, while using Timeline (play and seek a clip), Live and Export: no `Refused to ...` / `Content Security Policy` messages.
4. Walk a family member through `docs/family-onboarding.md` on a real iPhone and Android phone and note every step that did not match the screen.
**Expected result:** zero CSP violations in the console in normal use; manifest installable; service worker activates without errors; Lighthouse best-practices has no failing CSP or HTTPS audit; the onboarding guide matches what the phones show. Record the Lighthouse scores here.
**If the result differs, update:** a CSP violation means a directive in `contentSecurityPolicy` in `internal/zview/app.go` plus `csp_test.go` (`wantCSP`) and `app_test.go`, and ui-spec §2.4; prefer fixing the SPA over widening the policy. An installability failure: `web/static/manifest.webmanifest`, `web/scripts/gen-icons.py`, `web/index.html`. Wrong onboarding wording: `docs/family-onboarding.md` and the labels in `web/src/routes/onboarding/Onboarding.svelte`.
**Status:** not run

## Task 33 (final review fixes) — per-user 401 limit behind tailscale serve and S3 response timeout
**Automated in this task:** `internal/zview/auth.go` now counts failed authentications per `Tailscale-User-Login` when the peer is loopback or private (the `tailscale serve` or Docker-bridge proxy), else per peer address (tests `TestAuthFailureLimitIsPerTailscaleUserBehindProxy`, `TestTailscaleUserHeaderIgnoredFromPublicPeer`); `Onboarding.svelte` treats a 429 from the recent-recordings check like a 401 ("The server did not accept this key", test in `Onboarding.test.ts`); `zview-server` passes `s3.NewHTTPClient(30 s)` (shared with the relay) to the s3 driver (`TestOpenStorageS3GivesUpOnSilentServer`). Not exercised: a real `tailscale serve`, a real half-open MinIO.
**Manual steps:**
1. Behind the deployed `tailscale serve`, from one device send 12 requests with a wrong key: `for i in $(seq 12); do curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer wrong' https://<zview-host>/api/v1/status; done`. Expect 401 x10 then 429.
2. Immediately from a device of a different tailnet user (or with the real key) repeat one wrong-key request and one valid request.
3. On the NAS, `docker pause` the MinIO container, open the Timeline for today from two phones, wait 40 s, then `docker unpause`.
**Expected result:** step 2: a different user's wrong key still gets 401 (devices of the same Tailscale user share one budget, by design), the valid key always gets 200; step 3: both phones show a load error within about 35 s instead of spinning, and the Timeline loads after unpause.
**If the result differs, update:** `clientKey`/`proxyPeer` in `internal/zview/auth.go` with `auth_test.go` (for example if `tailscale serve` sends the header from a public-looking address, or Docker maps the peer to an address `proxyPeer` rejects); `s3ResponseTimeout` in `cmd/zview-server/main.go`.
**Status:** not run
