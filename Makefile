.PHONY: build amd64 arm clean

build:
	go build -o radio-gaga main.go

amd64:
	GOOS=linux GOARCH=amd64 go build -o radio-gaga-amd64 main.go

arm:
	GOOS=linux GOARCH=arm go build -o radio-gaga-arm main.go

clean:
	rm -f radio-gaga radio-gaga-amd64 radio-gaga-arm
