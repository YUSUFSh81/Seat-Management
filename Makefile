.PHONY: run test burst

burst:
	go run ./cmd/burst -url $(BASE_URL)