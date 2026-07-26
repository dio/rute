GORELEASER ?= goreleaser

.PHONY: fmt test race vet check vuln release-check snapshot

fmt:
	gofmt -w ./cmd ./internal ./oauthpkce ./providers

test:
	CGO_ENABLED=0 go test ./...

race:
	CGO_ENABLED=0 go test -race ./...

vet:
	CGO_ENABLED=0 go vet ./...

check: vet test race

vuln:
	govulncheck ./...

release-check:
	$(GORELEASER) check

snapshot:
	$(GORELEASER) release --snapshot --clean
