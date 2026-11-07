.PHONY: test vet integration chaos vectors web web-e2e demo

# Unit tests with the race detector.
test:
	go test -race ./...

# go vet plus staticcheck (pinned as a Go tool dependency in go.mod).
vet:
	go vet ./... && go tool staticcheck ./...

# Integration-tagged tests (MinIO and other fixtures run in Docker).
integration:
	go test -race -tags integration ./...

# Chaos run (design §9): three 4 Mbps cameras, MinIO black-holed for ten real
# minutes, memory bounded. Takes about 15 minutes; not part of CI. No -race:
# the detector's shadow memory would distort the memory assertion.
chaos:
	go test -tags chaos -run TestChaos -count=1 -timeout 40m -v ./internal/relay/app

# Regenerate the cross-language zrec vectors (Contract §4.5).
vectors:
	go run ./pkg/zrec/internal/genvectors -out testdata/zrec/v1

# SPA install, lint, type check and unit tests.
web:
	cd web && pnpm install --frozen-lockfile && pnpm lint && pnpm check && pnpm test

# SPA browser tests (Chromium and WebKit); needs `pnpm exec playwright install chromium webkit`.
web-e2e:
	cd web && pnpm exec playwright test

# Local demo without a camera or a NAS: a fake Tapo camera plus ready-made
# configs and a throwaway key in .demo/ (README "Try it locally").
demo:
	go run ./internal/testutil/cmd/demo --dir .demo
