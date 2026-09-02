# Multi-stage: cross-compiled static Go build -> distroless (includes CA roots for TLS).
# The build stage stays on the NATIVE builder ($BUILDPLATFORM) and cross-compiles to the
# target arch via GOARCH=$TARGETARCH, so the arm64 image is NOT compiled under slow QEMU.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG VERSION=dev
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -ldflags="-s -w -X main.Version=${VERSION}" -o /culipulse-agent .

FROM gcr.io/distroless/static-debian12
COPY --from=build /culipulse-agent /culipulse-agent
ENTRYPOINT ["/culipulse-agent"]
