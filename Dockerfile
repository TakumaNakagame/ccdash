# Container image for the hub (`ccdash hub serve`). Devices keep using the
# release binaries; only the portal runs in a container.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/takumanakagame/ccmanage/internal/buildinfo.Version=${VERSION}" \
      -o /out/ccdash ./cmd/ccdash

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ccdash /usr/local/bin/ccdash
ENV CCDASH_HUB_LISTEN=:8080 \
    CCDASH_HUB_DATA_DIR=/data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ccdash", "hub", "serve"]
