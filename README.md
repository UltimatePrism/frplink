# FRPLink

基于 [frp](https://github.com/fatedier/frp) (v0.71.1) 内核的多服务器内网穿透工具，Go 编写，单文件免安装。客户端内置 Web 管理界面，服务端为零配置交互式向导。

## 功能

- 多服务器管理：每节点独立连接、启停、实时上/下行速率、累计流量（持久化）、拖拽排序
- 分享码：服务端生成一串文本，客户端粘贴即可添加服务器（地址/端口/令牌/预置映射一应俱全）
- 连接测试、连接超时自动停止（默认 10 秒，可在设置中调整）、断线自动重连
- 传输协议 tcp / websocket / wss / quic / kcp + TLS；穿透类型 tcp / udp / http / https / stcp / xtcp / sudp

## 快速开始

每个发布包内含 `frplink`（客户端）与 `frplink-server`（服务端）。

### Windows

1. 下载 `frplink-vX.Y.Z-windows-amd64.zip` 解压
2. 服务端（公网机器）：双击 `frplink-server.exe` —— **自动生成随机认证令牌**，按提示回车启动 frps，**并输出一个分享码**
3. 客户端（内网机器）：双击 `frplink.exe`，浏览器自动打开管理页 → 添加 → 粘贴分享码 → 导入并启动

### Linux

```bash
tar xzf frplink-vX.Y.Z-linux-*.tar.gz && chmod +x frplink frplink-server

# 服务端（公网机器）：自动生成 token，回车启动并输出分享码
./frplink-server
#   配置自动保存为 frps.auto.toml，之后可免交互启动：
./frplink-server -c frps.auto.toml

# 客户端（内网机器）：打印管理页地址（无桌面时手动访问，或用 run 无界面运行）
./frplink
./frplink run
```

> 服务端兼容已有 frps 配置：`./frplink-server -c frps.toml`，同样输出分享码。

## 命令

```
frplink                   启动图形界面（-addr 监听地址 / -data 数据目录 / -no-browser）
frplink run               无界面运行（启动所有"随程序启动"的节点）
frplink import <分享码>    命令行导入
frplink gencode -c frps.toml   从 frps 配置生成分享码（支持 -proxy 预置映射、-expire 有效期）
frplink list / export     列出节点 / 导出分享码
frplink-server            服务端向导；-c 指定配置文件
```

## 编译

```
go build -trimpath -ldflags "-s -w" .                      # 客户端
go build -trimpath -ldflags "-s -w" -tags noweb ./cmd/frplink-server   # 服务端（noweb 跳过 frp 未随源码发布的管理页资源）
go test ./...                                              # 单元测试
go test -tags integration ./internal/runner                # 集成测试（自动编译 frps 做真实穿透验证）
```

## 安全说明

- 管理界面仅监听 127.0.0.1，每次启动生成随机访问密钥
- 分享码包含认证令牌，等同于密码，请通过可信渠道发送，建议设置有效期
