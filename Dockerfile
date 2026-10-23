FROM golang:1.27.1-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY src ./src
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cloud-storage-migration ./src

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -g 65532 app \
    && adduser -D -H -u 65532 -G app app \
    && mkdir -p /state /work/temp-copy /run/credentials \
    && chown app:app /state /work/temp-copy \
    && chmod 0700 /state /work/temp-copy
COPY --from=build /out/cloud-storage-migration /usr/local/bin/cloud-storage-migration
ENV AUTH_DIR=/state \
    TEMP_COPY_DIR=/work/temp-copy \
    AUTH_INTERACTIVE=true \
    GOOGLE_OAUTH_PORT=8765 \
    GOOGLE_OAUTH_LISTEN_HOST=0.0.0.0
WORKDIR /work
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cloud-storage-migration"]
