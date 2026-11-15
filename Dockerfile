# syntax=docker/dockerfile:1
#
# PairLink 多阶段构建：
#   build   : golang:1.24-alpine，CGO_ENABLED=0 静态编译（-trimpath -ldflags "-s -w"）
#   runtime : gcr.io/distroless/static-debian12:nonroot（无 shell、无包管理器、非 root）
#
# 选择 distroless 而不是 alpine 的理由：
#   - PairLink 是纯静态 Go 二进制，不需要 libc/包管理器；distroless 攻击面最小；
#   - 自带 CA 证书（agent/client 用 tls_ca 连公网/私有 CA 时需要）；
#   - 默认 nonroot（uid 65532），配合 compose 的 read_only + cap_drop: ALL 即可运行。
# 代价：镜像内没有 shell/curl，排障要走 docker logs 与宿主机端口检查；
#   因此**不提供镜像内 HEALTHCHECK**（见 deploy/README.md 的说明与替代命令）。
#
# 构建（在仓库根目录）：
#   docker build -t pairlink:v0.5.0 .
# 运行（示例，配置从宿主机只读挂载）：
#   docker run --rm -p 127.0.0.1:9000:9000 \
#     -v "$PWD/examples/docker/relay.ini:/etc/pairlink/relay.ini:ro" \
#     pairlink:v0.5.0 -relay -config /etc/pairlink/relay.ini
#
# 注意：EXPOSE 只是文档性声明（README 的默认生产端口），不会发布端口；
#       docker-compose.yml 使用 17000 段端口并显式 publish。

# ---------- build ----------
FROM golang:1.24-alpine AS build

WORKDIR /src

# 依赖层缓存：go.mod/go.sum 不变时无需重新下载模块。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 产出可放进 distroless/static 的静态二进制；
# -trimpath 去掉构建机路径；-s -w 去掉符号表/DWARF（体积更小，panic 栈仍可用）。
# 注意先建 /out：go build -o 不会自动创建父目录。
RUN mkdir -p /out && \
    CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/pairlink .

# ---------- runtime ----------
FROM gcr.io/distroless/static-debian12:nonroot

# 版本号与 internal/version.Version 保持一致（升级版本时同步修改）。
LABEL org.opencontainers.image.title="PairLink" \
      org.opencontainers.image.description="PairLink 配对式中继系统（relay / agent / client）" \
      org.opencontainers.image.version="v0.5.0" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/lsong98sh/pairlink"

COPY --from=build /out/pairlink /pairlink

# 基础镜像已带 nonroot 用户（uid/gid 65532）；显式声明便于审查。
USER nonroot:nonroot

# 文档性端口：隧道 7000、TCP 数据面 7001、原生 UDP 7002、管理页 9000。
EXPOSE 7000 7001 7002/udp 9000

STOPSIGNAL SIGTERM
ENTRYPOINT ["/pairlink"]
