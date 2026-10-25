# FRPLink 🚀

基于 [frp](https://github.com/fatedier/frp)（v0.71.1）客户端内核的**多服务器穿透管理器**，Go 编写，单文件即可运行。

- **多服务器管理**：同时连接任意多个 frps，每个服务器独立的启停/状态/穿透规则
- **图形界面**：双击运行自动打开浏览器管理页（中文界面，无外部依赖、免安装）
- **分享码一键导入**：服务端生成一串配置文本 `FRPLINK1.xxx`，客户端粘贴即可添加对应服务器 —— 无需手填地址、端口、令牌
- **失败自动重连**：断线/服务端重启后自动退避重试，错误实时显示
- **7 种穿透类型**：tcp / udp / http / https / stcp / xtcp / sudp，支持加密、压缩

## 快速开始

```
frplink.exe                 # 启动并自动打开管理界面
```

程序默认监听 `127.0.0.1` 随机端口，每次启动生成随机访问密钥（含在打印的 URL 里），只允许本机访问。

### 界面说明

| 页面 | 功能 |
|---|---|
| 服务器 | 竖排列表：备注名、本地地址:端口、服务器地址:端口、协议、实时上行/下行速度、启动/停止（绿/红）、编辑；右上角「添加」弹出菜单（手动添加 / 粘贴分享码导入） |
| 生成分享码 | 填写 frps 连接信息（可带预置穿透规则、有效期）生成分享码，供他人导入 |
| 运行日志 | 所有 frp 客户端连接的实时日志 |

## 分享码（核心功能）

服务端管理员把 frps 的连接信息（地址、端口、令牌、可选的预置穿透规则）编码为一行文本：

```
FRPLINK1.eyJ2IjoxLCJuYW1lIjoi...
```

使用者只需在「服务器」页点 **📥 粘贴分享码导入** → 粘贴 → 解析预览 → 导入（可选立即启动）。也可以用命令行：

```
frplink import "FRPLINK1.eyJ2IjoxLC..."
```

### 服务端如何生成分享码

**方式一：直接读取 frps 配置**（推荐，在 frps 所在机器上运行）：

```
frplink gencode -c frps.toml \
    -name "公司中转节点" \
    -note "有效期至 2027-01，勿外传" \
    -expire 2027-01-31 \
    -proxy tcp:127.0.0.1:22:6022 \
    -proxy http:127.0.0.1:8080:nav.example.com
```

自动从 `frps.toml`/`frps.ini`/`frps.yaml` 读取 `bindAddr`、`bindPort`、`auth.token`（bind 为 0.0.0.0 时需 `-addr` 指定对外地址）。

**方式二：在 GUI「生成分享码」页填写表单生成**（参数完全相同）。

**方式三：不读配置，手工指定**：

```
frplink gencode -addr frps.example.com -port 7000 -token 你的令牌 -name 节点一
```

`-proxy` 格式为 `类型:本地IP:本地端口:第四段`，第四段含义：tcp/udp → 远程端口；http/https → 逗号分隔的域名；stcp/xtcp/sudp → 共享密钥。可重复指定多条。

> ⚠️ **分享码包含认证令牌，等同于密码**，请只通过可信渠道发送。支持设置过期时间，过期后客户端拒绝导入。

## 配套服务端 frplink-server（frps 向导）

服务端程序 `frplink-server.exe` 零配置启动，三步完成：

```
> frplink-server.exe
[1/3] 首次运行，已自动生成随机认证令牌
      令牌: 090f77ced6a4a4e2e8c3e34d
[2/3] 设置监听参数
      监听地址 (回车=0.0.0.0):
      监听端口 (回车=7000):
[3/3] 客户端连接用的对外地址
      本机网卡: 192.168.1.5, ...
      对外 IP/域名 (回车=127.0.0.1):
配置已保存: frps.auto.toml
✅ frps 已启动，输出分享码:
FRPLINK1.eyJ2IjoxLCJhZGRyIjoi...
```

把输出的分享码发给使用者即可（客户端粘贴导入）。首次生成的配置会存为 `frps.auto.toml`，下次运行 `frplink-server -c frps.auto.toml` 免交互启动；也兼容任意现有 frps 配置（ini/toml/yaml/json）。

> 构建服务端需加 `noweb` 标签（frp 官方源码包未附带管理页静态资源）：
> `go build -tags noweb -o frplink-server.exe ./cmd/frplink-server`

## 命令一览

```
frplink                     启动图形界面
  -addr 127.0.0.1:7800      指定监听地址（默认随机端口）
  -data <目录>               指定数据目录（默认 %AppData%/frplink）
  -no-browser                不自动打开浏览器
frplink run                 无界面运行（启动所有勾选"随程序启动"的服务器）
frplink import <分享码>      命令行导入分享码
frplink gencode [...]        生成分享码（见上文）
frplink list                 列出已配置的服务器
frplink export -all          导出全部/单个服务器的分享码 (-id 前缀匹配)
frplink version              版本信息
```

## 编译

要求 Go 1.23+，无需 CGO：

```
go build -trimpath -ldflags "-s -w" -o frplink.exe .
```

交叉编译（例如在任意平台编 Windows 版）：

```
GOOS=windows GOARCH=amd64 go build -o frplink.exe .
```

运行测试：

```
go test ./internal/...                       # 单元测试
go test -tags integration ./internal/runner  # 集成测试（会自动编译 frps 并做真实穿透验证）
```

## 项目结构

```
frplink/
├── main.go                    入口 + CLI 子命令（GUI/run/import/gencode/list/export）
├── internal/
│   ├── model/                 数据模型：服务器条目、穿透规则、校验
│   ├── sharecode/             分享码编解码（FRPLINK1.<base64url(JSON)>）
│   ├── store/                 多服务器配置持久化（JSON，原子写入）
│   ├── runner/                frp 客户端封装：每服务器一个连接，启停/重试/状态
│   ├── server/                本地 Web GUI + REST API（含访问密钥鉴权）
│   │   └── web/index.html     前端单页（内嵌，无外部依赖）
│   └── logbuf/                日志环形缓冲（GUI 日志页数据源）
└── dist/                      编译产物
```

## 安全说明

- 管理界面仅监听回环地址，API 需携带本次启动的随机密钥（浏览器 URL 中的 `?k=`）
- 分享码 = 令牌 + 连接信息，泄露等同于泄露 frps 令牌，请谨慎分发；支持 `expire` 过期控制
- 本地配置文件 `servers.json` 中令牌为明文，请保护好运行账户

## frps 兼容性

基于 frp v0.71.1 客户端内核，支持 tcp/websocket/wss/quic/kcp 连接协议与 TLS（v0.50+ 默认开启，与大多数 frps 版本兼容）。
