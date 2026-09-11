# syntax=docker/dockerfile:1

FROM node:20-bookworm-slim AS web

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm config set registry https://registry.npmmirror.com \
    && npm ci

COPY web/ ./
RUN npm run build


FROM golang:1.26.6-bookworm AS build

WORKDIR /src

# 与 go.mod 的 go 1.26.4 对齐；local 禁止再去拉 toolchain，避免 proxy.golang.org 中断
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOPROXY=https://goproxy.cn,direct
ARG BUILD_TAGS=fuse

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=web /src/internal/api/web /src/internal/api/web

RUN go build -tags "${BUILD_TAGS}" -trimpath -ldflags="-s -w" -o /out/litepan ./cmd/litepan


FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata fuse3 \
    && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf 2>/dev/null || true \
    && grep -q '^user_allow_other' /etc/fuse.conf || echo user_allow_other >> /etc/fuse.conf \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

RUN mkdir -p /app/data/log /app/strm /app/mounts

COPY --from=build /out/litepan /app/litepan

# 许可证随镜像分发。PolyForm Noncommercial 1.0.0 的 Notices 条款要求：任何从
# 你这里取得软件拷贝的人，都必须一并获得许可证条款，以及其中所有
# "Required Notice:" 开头的行（本项目为 Copyright Ponphil）。
# docker pull 即构成一次分发，所以镜像里必须带上它。
COPY LICENSE /app/LICENSE

ENV LITEPAN_DATA_DIR=/app/data \
    LITEPAN_STRM_DIR=/app/strm \
    LITEPAN_LISTEN=:5211 \
    LITEPAN_LOG_LEVEL=info \
    TZ=Asia/Shanghai

EXPOSE 5211 42069/tcp 42069/udp

VOLUME ["/app/data", "/app/strm", "/app/mounts"]

ENTRYPOINT ["/app/litepan"]
