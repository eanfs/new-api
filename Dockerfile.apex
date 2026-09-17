# Apex fork 构建文件 —— 基于 rc.24 官方 Dockerfile,把基础镜像/依赖源抽成 ARG。
# 上游 Dockerfile 的 base 按 digest 钉在 docker.io;内网构建机无法访问 docker.io,
# 故 Apex 自建镜像时用内网镜像源覆盖。
#   默认值 = 上游 Dockerfile 的三个 digest 钉定 ref,plain `docker build -f Dockerfile.apex .`
#   与上游一致、可复现。无法访问 docker.io 的环境必须用 --build-arg 覆盖这三个 ARG 为内网镜像
#   ref;一旦覆盖,该镜像自身的 digest 由调用方负责钉定(本文件只钉上游 docker.io 的 digest)。
#   传这三个 --build-arg 的构建脚本不在本仓库内,无法核实它是否传全;内网构建前请自行确认,
#   否则会因拉取被墙的 docker.io 而构建失败。
ARG BUN_IMAGE=oven/bun:1.4.0@sha256:5ff609364c049b54eb0ff560ec96319729a972078ef2c755d758f0c6ef89c2d6
ARG GOLANG_IMAGE=golang:1.26.1-alpine@sha256:2389ebfa5b7f43eeafbd6be0c3700cc46690ef842ad962f6c5bd6be49ed82039
ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:f06537653ac770703bc45b4b113475bd402f451e85223f0f2837acbf89ab020a

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
# 仓库里的 VERSION 是空文件,由构建流程在构建前写入;为空时 -X 会静默写入空版本,
# 使镜像 tag 与 /api/status 的 version 不一致,故这里直接失败而不是产出空版本镜像。
RUN test -s VERSION || { echo "ERROR: VERSION is empty; the image version and /api/status version would be blank" >&2; exit 1; }; \
    go build -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'" -o new-api

FROM ${DEBIAN_IMAGE}
# apt 源也走镜像源:部分网络下从 deb.debian.org 拉取大文件极慢或半挂。
# 默认值 deb.debian.org 时不替换(官方源可用);用 --build-arg APT_MIRROR_HOST=<镜像域名> 覆盖以加速。
# 安全:APT_MIRROR_HOST 会被插进 sed 脚本,必须先按白名单校验。GNU sed 的 e 命令能把
# "x|g;e <cmd> #" 这类值变成真正的 shell 执行(以 root 在构建期跑),故只放行
# [A-Za-z0-9.-](同时排除 sed 的 s 分隔符 |、特殊字符 & \、以及全部 shell 元字符)。
# 校验/改写/校验三步都不静默吞错:失败即非零退出,避免"替换没生效 → 撞被墙的
# deb.debian.org → 表现为网络超时"这种最难查的失败。
# bookworm-slim 只有 /etc/apt/sources.list.d/debian.sources(没有 /etc/apt/sources.list),
# 故只改写该文件,并显式校验它存在 —— 不靠 sed 对不存在的路径报错。
ARG APT_MIRROR_HOST=deb.debian.org
RUN set -eu; \
    case "${APT_MIRROR_HOST}" in \
      *[!A-Za-z0-9.-]*|'') \
        printf 'ERROR: APT_MIRROR_HOST must match [A-Za-z0-9.-]+ (got %s); refusing to build\n' "${APT_MIRROR_HOST}" >&2; \
        exit 1; \
        ;; \
    esac; \
    if [ "${APT_MIRROR_HOST}" = "deb.debian.org" ]; then \
      echo "APT_MIRROR_HOST is the default deb.debian.org; Debian sources left as shipped"; \
    else \
      sources=/etc/apt/sources.list.d/debian.sources; \
      if [ ! -f "${sources}" ]; then \
        echo "ERROR: ${sources} not found; cannot point APT at ${APT_MIRROR_HOST}" >&2; \
        exit 1; \
      fi; \
      if ! grep -qF -- "deb.debian.org" "${sources}"; then \
        echo "ERROR: ${sources} has no deb.debian.org entry to rewrite; unexpected base image layout" >&2; \
        exit 1; \
      fi; \
      sed -i "s|deb.debian.org|${APT_MIRROR_HOST}|g" "${sources}"; \
      if ! grep -qF -- "${APT_MIRROR_HOST}" "${sources}"; then \
        echo "ERROR: rewriting deb.debian.org to ${APT_MIRROR_HOST} in ${sources} took no effect" >&2; \
        exit 1; \
      fi; \
    fi
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata libasan8 wget \
    && rm -rf /var/lib/apt/lists/* \
    && update-ca-certificates

COPY --from=builder2 /build/new-api /
COPY LICENSE NOTICE THIRD-PARTY-LICENSES.md /licenses/
EXPOSE 3000
WORKDIR /data
ENTRYPOINT ["/new-api"]
