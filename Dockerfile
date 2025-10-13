FROM golang:1.25 AS build-env

ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH

RUN apt-get install -yq --no-install-recommends git

# Copy source + vendor
COPY . /go/src/github.com/vincejv/phivolcs-eq-to-matrix
WORKDIR /go/src/github.com/vincejv/phivolcs-eq-to-matrix

# Compile go binaries
ENV GOPATH=/go
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GO111MODULE=on go build -v -a -ldflags "-s -w" -o /go/bin/phivolcs-eq-to-matrix .

# Build final image from alpine
FROM alpine:latest
RUN apk --update --no-cache add curl && rm -rf /var/cache/apk/*
COPY --from=build-env /go/bin/phivolcs-eq-to-matrix /usr/bin/phivolcs-eq-to-matrix

# Create a group and user
RUN addgroup -S phivolcs-eq-to-matrix && adduser -S phivolcs-eq-to-matrix -G phivolcs-eq-to-matrix
USER phivolcs-eq-to-matrix

ENTRYPOINT ["phivolcs-eq-to-matrix"]