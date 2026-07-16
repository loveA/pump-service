# ================= 启用现代 BuildKit 的高级解析器 =================
# syntax=docker/dockerfile:1

# ================= 第一阶段：编译阶段 =================
FROM golang:1.23-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git

# 1. 缓存 Go modules 依赖
COPY go.mod ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 安装 garble 混淆工具
RUN go install mvdan.cc/garble@v0.13.0

# 复制所有源码
COPY . .

# 2. 编译时挂载 Go 编译缓存和依赖缓存，Garble 同样会受益于增量编译
# 使用 garble 代替 go build 进行混淆编译
# -literals: 混淆字符串字面量（极其重要，加密你代码里的 URL、命令行参数等硬编码字符串）
# -tiny: 进一步缩减体积，移除文件名、行号等调试所需的位置信息
# -ldflags="-s -w": 依然保留，用于剔除符号表和 DWARF 信息
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 GOOS=linux garble -literals -tiny build -ldflags="-s -w" -o pump-server main.go


# ================= 第二阶段：运行阶段 =================
FROM alpine:3.19

# 3. 安装运行时必需的依赖
RUN apk add --no-cache \
    ffmpeg \
    util-linux \
    ca-certificates

WORKDIR /app

# 4. 从 builder 阶段把“混淆后”的二进制程序拷贝过来
COPY --from=builder /build/pump-server .

# 创建视频存储目录
RUN mkdir -p ./records

# 5. 声明服务监听的端口
EXPOSE 19999

# 6. 启动程序
ENTRYPOINT ["./pump-server"]
