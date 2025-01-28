VERSION := $(shell git describe --always --dirty=-$(shell hostname)-$(shell date -u +%Y%m%d-%H%M%S))
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build amd64 arm clean

build:
	go build -ldflags "$(LDFLAGS)" -o radio-gaga main.go

amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o radio-gaga-amd64 main.go

arm:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o radio-gaga-arm main.go

clean:
	rm -f radio-gaga radio-gaga-amd64 radio-gaga-arm
