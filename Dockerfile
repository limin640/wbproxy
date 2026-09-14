# 两阶段：构建 + 极简运行镜像
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /wbproxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /wbproxy /usr/local/bin/wbproxy
ENV LISTEN=:8080
EXPOSE 8080
ENTRYPOINT ["wbproxy"]
CMD ["-listen", ":8080"]
