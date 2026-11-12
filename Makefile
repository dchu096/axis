GIT_HEAD = $(shell git rev-parse HEAD | head -c8)

build:
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -gcflags "all=-trimpath=$(pwd)" -o build/axis_linux_amd64 -v axis.go

debug:
	go build -ldflags="-X github.com/0x7d8/wings/system.Version=$(GIT_HEAD)" -o axis
	sudo ./axis --debug --ignore-certificate-errors --config config.yml --pprof --pprof-block-rate 1

# Runs a remotely debuggable session for Axis allowing an IDE to connect and target
# different breakpoints.
rmdebug:
	go build -gcflags "all=-N -l" -ldflags="-X github.com/0x7d8/wings/system.Version=$(GIT_HEAD)" -race -o axis
	sudo dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./axis -- --debug --ignore-certificate-errors --config config.yml

cross-build: clean build compress

clean:
	rm -rf build/axis_* build/wings_*

.PHONY: all build compress clean
