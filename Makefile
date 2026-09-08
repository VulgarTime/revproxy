BINARY   := revproxy
VERSION  := 1.0.0
LDFLAGS  := -s -w -X main.Version=$(VERSION)
GOFLAGS  := -trimpath
DIST     := dist
PLATFORMS := linux/amd64 linux/arm64 linux/arm/v7 darwin/amd64 darwin/arm64 windows/amd64 freebsd/amd64

.PHONY: all build run dev test cross docker up down fmt vet clean install help

all: fmt vet build

## build: 编译当前平台二进制到 bin/
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o bin/$(BINARY) .

## run: 编译并前台运行（数据目录 ./data，开启 debug 日志）
run: build
	./bin/$(BINARY) -data ./data -addr :8080 -debug

## dev: 不编译直接跑（改代码后重新执行即可）
dev:
	go run . -data ./data -addr :8080 -debug

## test: 单元测试
test:
	go test ./... -race -count=1

## cross: 交叉编译到 dist/（含 Linux/Mac/Windows/FreeBSD）
cross:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$$(echo $$p | cut -d/ -f1); \
		arch=$$(echo $$p | cut -d/ -f2); \
		arm=$$(echo $$p | cut -d/ -f3 | tr -d 'v'); \
		out=$(DIST)/$(BINARY)-$$os-$$arch$${arm:+$$arm}; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "  → $$out"; \
		GOOS=$$os GOARCH=$$arch GOARM=$$arm CGO_ENABLED=0 \
			go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $$out . || exit 1; \
	done
	@ls -lh $(DIST)

## docker: 构建镜像
docker:
	docker build -t $(BINARY):$(VERSION) -t $(BINARY):latest .

## up/down: docker compose 启停
up:
	docker compose up -d --build
down:
	docker compose down

## install: 编译并安装到本机（含 systemd 服务）
install: build
	@sudo install -m 0755 bin/$(BINARY) /usr/local/bin/$(BINARY)
	@sudo install -d -m 0755 /etc/revproxy
	@[ -f /etc/revproxy/config.json ] || sudo install -m 0644 examples/config.example.json /etc/revproxy/config.json
	@sudo install -m 0644 deploy/$(BINARY).service /etc/systemd/system/$(BINARY).service
	@sudo systemctl daemon-reload
	@sudo systemctl enable --now $(BINARY)
	@sudo systemctl status $(BINARY) --no-pager || true

## uninstall: 卸载 systemd 服务与二进制
uninstall:
	@sudo systemctl stop $(BINARY) 2>/dev/null || true
	@sudo systemctl disable $(BINARY) 2>/dev/null || true
	@sudo rm -f /etc/systemd/system/$(BINARY).service /usr/local/bin/$(BINARY)
	@sudo systemctl daemon-reload

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -rf bin $(DIST)

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
