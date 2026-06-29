VERSION := 2.0.0
BINARY := coreclaw-mcp-server

.PHONY: build test lint release clean deploy nginx

build:
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) .

test:
	go test -v -race ./...

lint:
	go vet ./...

release: clean
	mkdir -p dist
	GOOS=darwin  GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-darwin-amd64 .
	GOOS=darwin  GOARCH=arm64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-darwin-arm64 .
	GOOS=linux   GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-linux-amd64 .
	GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o dist/$(BINARY)-windows-amd64.exe .

clean:
	rm -f $(BINARY)
	rm -rf dist/

# 自动发布到 Ubuntu。服务器和目录等信息从 .env 读取。
# 覆盖部署目录：make deploy DIR=/opt/coreclaw-mcp-server
# 跳过重新编译：make deploy ARGS=--skip-build
deploy:
	./scripts/deploy.sh $(if $(DIR),--dir $(DIR)) $(ARGS)

# 部署 nginx 反向代理配置到 Ubuntu。
# make nginx                            # 按 .env 默认值
# make nginx ARGS="--server mcp.x.io"   # 覆盖 server_name
# make nginx ARGS=--dry-run             # 只打印生成的 conf
nginx:
	./scripts/setup-nginx.sh $(ARGS)
