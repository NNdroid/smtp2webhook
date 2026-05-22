#!/usr/bin/env bash

# ==============================================================================
# 🚀 工业级 Go 多平台自动交叉编译流水线脚本
# ==============================================================================

# 发生任何错误时立即终止脚本执行
set -e

# 定义编译产物的输出目录
OUTPUT_DIR="bin"
# 定义你的程序名称
BINARY_NAME="smtp2webhook"

# 确保输出目录干净、存在
echo "🧹 正在清理旧的编译产物..."
rm -rf "${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"

echo "📦 开始对内置静态网页文件及依赖执行安全检查..."
# 检查当前目录下是否有 index.html 以免 go:embed 报错
if [ ! -f "index.html" ]; then
    echo "❌ 错误: 未在当前目录下找到 index.html，Go 无法将其编码进程序！"
    exit 1
fi

# ==============================================================================
# 🛠 核心编译函数
# 参数: 1:GOOS  2:GOARCH  3:扩展名(可选)
# ==============================================================================
build_target() {
    local os=$1
    local arch=$2
    local extension=$3
    local full_name="${BINARY_NAME}-${os}-${arch}${extension}"
    
    echo "🔨 正在编译: 目标系统=${os} | 目标架构=${arch} -> ${OUTPUT_DIR}/${full_name}"
    
    # 🌟 核心调优参数解析：
    # CGO_ENABLED=0: 纯静态编译，不依赖宿主机的 C 语言动态链接库，确保编译出的二进制文件在任何 Linux 发行版（如 Alpine、Ubuntu）上都能无缝运行。
    # -s: 移除符号表（Symbol Table）。
    # -w: 移除 DWARF 调试信息。
    # 这两个优化参数可以大幅缩减最终二进制文件约 30%~40% 的体积，并且能有效提升黑客反编译的难度。
    CGO_ENABLED=0 GOOS=${os} GOARCH=${arch} go build \
        -ldflags="-s -w" \
        -o "${OUTPUT_DIR}/${full_name}" main.go
}

echo "----------------------------------------------------------------"
echo "🌀 启动跨平台矩阵编译..."
echo "----------------------------------------------------------------"

# 1. 生产环境主力：Linux 矩阵
build_target "linux" "amd64" ""
build_target "linux" "arm64" ""

# 2. 苹果生态：macOS 矩阵（支持 Intel 芯片与 M1/M2/M3 Apple Silicon 芯片）
build_target "darwin" "amd64" ""
build_target "darwin" "arm64" ""

# 3. 微软生态：Windows 矩阵
build_target "windows" "amd64" ".exe"

echo "----------------------------------------------------------------"
echo "🎉 编译矩阵流水线全量执行完毕！"
echo "📊 以下是生成的单文件可执行程序列表："
echo "----------------------------------------------------------------"

# 高亮打印生成的文件列表及大小
ls -lh "${OUTPUT_DIR}"

echo "----------------------------------------------------------------"
echo "💡 提示：部署到 Linux 服务器时，别忘了执行 'chmod +x' 赋予执行权限。"