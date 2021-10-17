.PHONY:all build clean run check cover lint docker help sms_gateway
BUILD_NAME:=bin/sms_gateway
BUILD_VERSION := 1.0
SOURCE=apps/main.go

all: deps build
deps:
	go mod tidy
build:
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-w' -o ${BUILD_NAME} ${SOURCE}
