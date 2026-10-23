LDFLAGS := -s -w

.PHONY: test vet fmt check build-bench ffmpeg clean

# A test that hangs fails after a minute instead of ten; the suite takes seconds.
test:
	go test -timeout 60s ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

check: fmt vet test

# The benchmark against Lavalink, for a Raspberry Pi 5: see BENCHMARKS.md.
build-bench:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/voicebench-linux-arm64 ./cmd/voicebench

# A lean, static ffmpeg for a Raspberry Pi 5; TARGET=x86_64-linux-musl for
# amd64. See scripts/build-ffmpeg.sh.
ffmpeg:
	scripts/build-ffmpeg.sh

clean:
	rm -rf bin build
