# 沙箱管理服务镜像
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sandbox-server .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/sandbox-server /sandbox-server
EXPOSE 8090
ENTRYPOINT ["/sandbox-server"]
