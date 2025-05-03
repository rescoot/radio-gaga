VERSION := $(shell git describe --always --tags --dirty=-$(shell hostname)-$(shell date -u +%Y%m%d-%H%M%S))
LDFLAGS := -X main.version=$(VERSION)
BUILDFLAGS := -tags netgo,osusergo
MAIN_PATH := ./cmd/radio-gaga
OUTPUT_NAME := radio-gaga

.PHONY: build amd64 arm arm-debug dist clean install

dev: build
build:
	go build -ldflags "$(LDFLAGS)" -o $(OUTPUT_NAME) $(MAIN_PATH)

amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o $(OUTPUT_NAME)-amd64 $(MAIN_PATH)

arm:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o $(OUTPUT_NAME)-arm $(MAIN_PATH)

dist:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o $(OUTPUT_NAME)-arm-dist $(MAIN_PATH)

arm-debug:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -gcflags="all=-N -l" $(BUILDFLAGS) -o $(OUTPUT_NAME)-arm-debug $(MAIN_PATH)

clean:
	rm -f $(OUTPUT_NAME) $(OUTPUT_NAME)-amd64 $(OUTPUT_NAME)-arm $(OUTPUT_NAME)-arm-dist $(OUTPUT_NAME)-arm-debug