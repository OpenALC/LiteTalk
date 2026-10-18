# LiteTalk 服务端镜像（#27）：多阶段构建，最终 CGO_ENABLED=0 静态二进制
#（SQLite 走纯 Go 的 modernc 驱动，无 cgo 依赖）。保持单二进制卖点：不塞 MySQL/Redis/coturn。
# 发版镜像请传版本号：docker build --build-arg VERSION=vX.Y.Z -t litetalk-server:vX.Y.Z .
# VERSION=dev 时生产守卫（JWT/SFU IP）不生效，仅限本地自测。
FROM golang:1.27 AS build
WORKDIR /src
# 先只拷依赖清单：server/ 源码变更不使 go mod download 层失效
COPY server/go.mod server/go.sum ./server/
WORKDIR /src/server
RUN go mod download
COPY server/ ./
ARG VERSION=dev
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/litetalk-server ./cmd/server

# nonroot（uid 65532）：绑定 80/443 需要 compose 里的
# net.ipv4.ip_unprivileged_port_start=0（或自行 --cap-add / 反代终结 TLS）。
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/litetalk-server /litetalk-server
ENV LITETALK_DATA_DIR=/data \
    LITETALK_STORE=sqlite
VOLUME /data
# 80/443：ACME 挑战与 TLS（或纯 HTTP API 时 8077）
# 7845：SFU UDP 媒体 + ICE-TCP；3478：内置 TURN UDP+TCP（中继段 49152-49200 见 compose）
EXPOSE 80 443 8077 7845/udp 7845/tcp 3478/udp 3478/tcp
ENTRYPOINT ["/litetalk-server"]
