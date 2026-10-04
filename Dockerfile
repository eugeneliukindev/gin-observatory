FROM golang:1.26 AS build
WORKDIR /src
# Modules in a layer of their own: a code change does not download them again.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
COPY web web
# No cgo — the SQLite driver is pure Go — so the binary runs on an image with nothing else in it.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /api ./cmd/api
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /api /api
COPY --from=build --chown=nonroot:nonroot /data /data
WORKDIR /data
EXPOSE 8000
ENTRYPOINT ["/api"]
