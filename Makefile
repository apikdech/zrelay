.PHONY: test vet integration vectors web

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

# SPA install, lint and unit tests.
web:
	cd web && pnpm install --frozen-lockfile && pnpm lint && pnpm test
