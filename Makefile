VERSION := $(shell git describe --always --dirty=-$(shell hostname)-$(shell date -u +%Y%m%d-%H%M%S))
LDFLAGS := -X main.version=$(VERSION)
BUILDFLAGS := -tags netgo,osusergo

.PHONY: build amd64 arm arm-debug dist clean

dev: build
build:
	go build -ldflags "$(LDFLAGS)" -o radio-gaga main.go

amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o radio-gaga-amd64 main.go

arm:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o radio-gaga-arm main.go

dist:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o radio-gaga-arm-dist main.go

arm-debug:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -gcflags="all=-N -l" $(BUILDFLAGS) -o radio-gaga-arm-debug main.go

clean:
	rm -f radio-gaga radio-gaga-amd64 radio-gaga-arm
