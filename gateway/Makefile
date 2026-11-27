.PHONY: test car

test:
	go test ./...

car:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o out/agw ./cmd/agw
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o out/camhelper ./cmd/camhelper
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o out/summonhelper ./cmd/summonhelper

.PHONY: interop
interop:
	mkdir -p out
	swiftc -O interop/swift/GatewayCrypto.swift interop/swift/main.swift -o out/interop
	out/interop selftest testdata/vectors.json
	out/interop seal testdata/vectors.json testdata/cryptokit_arm.json
	go test ./internal/session -run CryptoKit -count=1
	out/interop opens2c testdata/go_s2c.json testdata/cryptokit_arm.json
