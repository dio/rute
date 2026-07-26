.PHONY: fmt test race vet check

fmt:
	gofmt -w ./cmd ./internal ./oauthpkce ./providers

test:
	CGO_ENABLED=0 go test ./...

race:
	CGO_ENABLED=0 go test -race ./...

vet:
	CGO_ENABLED=0 go vet ./...

check: vet test race
