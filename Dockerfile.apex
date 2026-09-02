# Apex fork 构建文件 —— 基于 rc.24 官方 Dockerfile,把基础镜像/依赖源抽成 ARG。
# 上游 Dockerfile 的 base 按 digest 钉在 docker.io;内网 arm64 构建机(172.16.12.183)
# docker.io 被墙,故 Apex 自建镜像时用镜像源覆盖(见 aws-deploy/scripts/build-newapi.sh)。
#   默认值保持与上游同 tag;去掉 digest 换镜像源时可用(ECR 侧已 IMMUTABLE + 生命周期)。
ARG BUN_IMAGE=oven/bun:1
ARG GOLANG_IMAGE=golang:1.26.1-alpine
ARG DEBIAN_IMAGE=debian:bookworm-slim

FROM ${BUN_IMAGE} AS builder
# bun 装 web 依赖;内网经 npmmirror 加速
ARG NPM_REGISTRY=https://registry.npmjs.org
ENV BUN_CONFIG_REGISTRY=${NPM_REGISTRY}
WORKDIR /build/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY ./web ./
COPY ./VERSION /build/VERSION
RUN DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION=$(cat /build/VERSION) bun run build

FROM ${GOLANG_IMAGE} AS builder2
ENV GO111MODULE=on CGO_ENABLED=0 GOWORK=off
# go 模块下载镜像源(proxy.golang.org 在内网可能不可达)
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

ARG TARGETOS
ARG TARGETARCH
ENV GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}
ENV GOEXPERIMENT=greenteagc

WORKDIR /build

ADD go.mod go.sum ./
# relaykit 是本地 replace 的目录模块;go mod download 需要其 go.mod 在场
ADD relaykit/go.mod ./relaykit/go.mod
RUN go mod download

COPY . .
COPY --from=builder /build/web/dist ./web/dist
# 运行版本 = 构建时 VERSION 内容(镜像 tag 后缀,须与 /api/status version 精确一致)
RUN go build -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'" -o new-api

FROM ${DEBIAN_IMAGE}
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata libasan8 wget \
    && rm -rf /var/lib/apt/lists/* \
    && update-ca-certificates

COPY --from=builder2 /build/new-api /
COPY LICENSE NOTICE THIRD-PARTY-LICENSES.md /licenses/
EXPOSE 3000
WORKDIR /data
ENTRYPOINT ["/new-api"]
