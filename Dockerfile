# 沙箱管理服务镜像
# 在构建机的原生架构上交叉编译，构建多架构镜像时不需要 QEMU 模拟 Go 编译
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/sandbox-server .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/sandbox-server /sandbox-server
EXPOSE 8090
ENTRYPOINT ["/sandbox-server"]
