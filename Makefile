.PHONY: test vet integration vectors web web-e2e

# Unit tests with the race detector.
test:
	go test -race ./...

# go vet plus staticcheck (pinned as a Go tool dependency in go.mod).
vet:
	go vet ./... && go tool staticcheck ./...

# Integration-tagged tests (MinIO and other fixtures run in Docker).
integration:
	go test -race -tags integration ./...

# Regenerate the cross-language zrec vectors (Contract §4.5).
vectors:
	go run ./pkg/zrec/internal/genvectors -out testdata/zrec/v1

# SPA install, lint, type check and unit tests.
web:
	cd web && pnpm install --frozen-lockfile && pnpm lint && pnpm check && pnpm test

# SPA browser tests (Chromium and WebKit); needs `pnpm exec playwright install chromium webkit`.
web-e2e:
	cd web && pnpm exec playwright test
