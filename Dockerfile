# syntax=docker/dockerfile:1
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/cuijianzhuang/polaris/internal/panel.Version=${VERSION}" -o /out/polaris ./cmd/panel \
 && for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOARCH=$arch go build -trimpath -ldflags "-s -w -X github.com/cuijianzhuang/polaris/internal/agent.Version=${VERSION}" -o /out/downloads/polaris-agent-linux-$arch ./cmd/agent; \
    done

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 polaris
COPY --from=build /out/polaris /usr/local/bin/polaris
COPY --from=build /out/downloads /opt/polaris/downloads
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV POLARIS_DATA_DIR=/data POLARIS_LISTEN=:8080 POLARIS_AGENT_LISTEN=:9443
VOLUME /data
EXPOSE 8080 9443
USER polaris
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
