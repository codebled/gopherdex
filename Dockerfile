# syntax=docker/dockerfile:1

# Gopherdex registry server. The SQLite driver is pure Go, so the binaries
# are static and the runtime image has no shell or package manager.
FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=""
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X github.com/parthiban-sivakumar/gopherdex/internal/version.Version=${VERSION}" \
        -o /out/gopherdexd ./cmd/gopherdexd && \
    go build -trimpath -ldflags "-s -w -X github.com/parthiban-sivakumar/gopherdex/internal/version.Version=${VERSION}" \
        -o /out/gopherdex ./cmd/gopherdex && \
    mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gopherdexd /out/gopherdex /usr/local/bin/
COPY --from=build --chown=nonroot:nonroot /out/data /data
# Every flag can be set as GOPHERDEX_<FLAG>; see deploy/gopherdex.env.example.
ENV GOPHERDEX_ADDR=:8080 \
    GOPHERDEX_DB=/data/gopherdex.db \
    GOPHERDEX_BLOBS=/data/blobs
VOLUME /data
EXPOSE 8080
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/gopherdexd", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/gopherdexd"]
