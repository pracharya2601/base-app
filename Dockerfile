# PocketBase (framework mode) + Litestream, in one image.
# Litestream supervises PocketBase so it can restore-on-boot and stream the WAL.

# ---- frontend stage: build the Svelte admin SPA (served at /admin) ----
FROM node:22-alpine AS frontend
WORKDIR /fe
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend/ ./
# Output straight to /spa; the Go stage embeds it into internal/adminui/spa.
RUN npm run build -- --outDir /spa --emptyOutDir

# ---- build stage: compile our custom PocketBase from main.go ----
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
COPY internal/ ./internal/
# Overwrite the committed SPA build with a fresh one from the frontend stage.
COPY --from=frontend /spa ./internal/adminui/spa
RUN go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -o /pb/pocketbase .

# ---- runtime stage ----
FROM alpine:3.20
ARG LITESTREAM_VERSION=0.5.12
ARG TARGETARCH
RUN apk add --no-cache ca-certificates wget

# Install Litestream (release assets use x86_64 / arm64 naming).
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) LS_ARCH=x86_64 ;; \
      arm64) LS_ARCH=arm64 ;; \
      *)     LS_ARCH="$TARGETARCH" ;; \
    esac; \
    wget -qO /tmp/ls.tgz "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-${LITESTREAM_VERSION}-linux-${LS_ARCH}.tar.gz"; \
    tar -C /usr/local/bin -xzf /tmp/ls.tgz litestream; \
    rm /tmp/ls.tgz; \
    litestream version

WORKDIR /pb
COPY --from=build /pb/pocketbase /pb/pocketbase
COPY litestream.yml /etc/litestream.yml
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

VOLUME /pb/pb_data
EXPOSE 8090

ENTRYPOINT ["/entrypoint.sh"]
