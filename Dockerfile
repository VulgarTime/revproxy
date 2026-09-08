# ---------- 构建阶段 ----------
FROM golang:1.21-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/revproxy .

# ---------- 运行阶段 ----------
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S revproxy 2>/dev/null || true \
    && adduser -S -G revproxy revproxy 2>/dev/null || true
COPY --from=builder /out/revproxy /usr/local/bin/revproxy
RUN mkdir -p /data && chown -R 65534:65534 /data
VOLUME ["/data"]
EXPOSE 8080
ENV REVPROXY_DATA=/data \
    TZ=Asia/Shanghai
USER 65534:65534
ENTRYPOINT ["/usr/local/bin/revproxy"]
CMD ["-data", "/data", "-addr", ":8080"]
