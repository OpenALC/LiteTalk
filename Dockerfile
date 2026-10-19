# LiteTalk 服务端镜像（#27）：多阶段构建，最终 CGO_ENABLED=0 静态二进制
#（SQLite 走纯 Go 的 modernc 驱动，无 cgo 依赖）。保持单二进制卖点：不塞 MySQL/Redis/coturn。
# 发版镜像请显式传版本号：docker build --build-arg VERSION=vX.Y.Z -t litetalk-server:vX.Y.Z .
# VERSION 默认 local（非 dev）：生产守卫（JWT/SFU IP/存储降级）全部生效；
# 只有显式传 VERSION=dev 才回到开发模式，仅限本地自测。
FROM golang:1.27 AS build
WORKDIR /src
# 先只拷依赖清单：server/ 源码变更不使 go mod download 层失效。
# client 仅需 go.mod/go.sum：server 的联调测试通过 replace ../client 引用真实
# 客户端 Tracker（#32），模块图加载需要它；构建服务端二进制不编译客户端源码。
COPY server/go.mod server/go.sum ./server/
COPY client/go.mod client/go.sum ./client/
WORKDIR /src/server
RUN go mod download
COPY server/ ./
ARG VERSION=local
ARG TARGETARCH=amd64
# /out/data 供最终镜像创建 /data：命名卷首次创建时继承镜像内目录属主（65532）。
RUN mkdir -p /out/data && CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/litetalk-server ./cmd/server

# nonroot（uid 65532）：绑定 80/443 需要 compose 里的
# net.ipv4.ip_unprivileged_port_start=0（或自行 --cap-add / 反代终结 TLS）。
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/litetalk-server /litetalk-server
# /data 属主设为 65532：compose 的命名卷首次创建会继承它，非 root 进程可直接写入
# （SQLite / 自动生成的 jwt.secret）。bind mount 不受镜像属主影响，宿主机目录
# 需要先 chown 65532:65532。
COPY --from=build --chown=65532:65532 /out/data /data
ENV LITETALK_DATA_DIR=/data \
    LITETALK_STORE=sqlite
VOLUME /data
# 80/443：ACME 挑战与 TLS（或纯 HTTP API 时 8077）
# 7845：SFU UDP 媒体 + ICE-TCP；3478：内置 TURN UDP+TCP（中继段 49152-49200 见 compose）
EXPOSE 80 443 8077 7845/udp 7845/tcp 3478/udp 3478/tcp
ENTRYPOINT ["/litetalk-server"]
