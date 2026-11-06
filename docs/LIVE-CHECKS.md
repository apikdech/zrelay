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
**Automated in this task:** `internal/relay/egress` (`Egress`, `Run`, `Shutdown`, `BacklogBytes`, drain workers) is tested against a scriptable fake `storage.Writer` and the fake clock: `TestCommitOrdering` (staging Put, object Commit, final Put, staging Delete; no sidecar before Commit), `TestResumeAfterTransientWriteError` (lagging `DurableOffset`, object bytes identical to the stream), `TestBreakerOpenParksWorkersAndBacklogGrows` and `...DrainsAfterClose`, `TestEvictionAbortsByOwner` (Abort on the worker goroutine that called Create), `TestWorkersLimit`, `TestShutdownWithOpenBreakerExitsWithinGrace` (30 s fake grace, both clips aborted, `OnDrop(...,"shutdown")` twice), plus healthy-drain shutdown, recorder abandon and force-finalize forwarding. All pass with `-race`. Nothing ran against a real MinIO.
**Manual steps:**
1. Start the stack against the Synology MinIO with a small backlog: `egress_backlog_mb: 16`, `shutdown_grace_seconds: 30`, `upload_workers: 2`, one camera recording continuously (`docker compose up -d zrelay`).
2. Block MinIO from the Mac mini: on the Synology, `iptables -I INPUT -p tcp --dport 9000 -j DROP` (or stop the MinIO container). Wait until `curl -s 127.0.0.1:9464/metrics | grep zrelay_storage_breaker_state` shows `2`.
3. While it is down, watch the backlog grow (`storage.backlog_bytes` in `status/relay.json` once the status publisher reports `BacklogBytes`), then send `docker stop -t 60 zrelay` and time it: `time docker stop -t 60 zrelay`.
4. Restore MinIO, restart zrelay, and run `mc ls --recursive <alias>/<bucket>/clips/` to look for stray `.part` objects and `mc admin ... incomplete` multipart uploads.
5. Repeat steps 1 and 2, but restore MinIO before stopping; confirm the clips appear in `clips/<camera>/...` with `.zrec` and `.json`, and the dot-named staging sidecars are gone.
**Expected result:** the container exits within about `shutdown_grace_seconds` (not 60 s) with one log line `egress: clip dropped ... reason=shutdown` per buffered clip and `zrelay_clips_dropped_total{reason="shutdown"}` incremented accordingly; after recovery, clips buffered while MinIO was down but not dropped commit with final sidecars and no leftover staging sidecar; a backlog over 16 MiB logs `reason=backlog` drops, oldest finalized clip first.
**If the result differs, update:** `Egress.Shutdown` and `Egress.abort` (`internal/relay/egress/egress.go`, `worker.go`: the real-time `abortTimeout`, which bounds `Abort` against an unreachable MinIO), the S3 driver's `Abort`/`Resume` context handling (`pkg/storage/s3`), and design §6.3/§6.4 plus `TestShutdownWithOpenBreakerExitsWithinGrace`.
**Status:** not run
