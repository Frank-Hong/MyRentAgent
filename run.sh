#!/bin/bash

# 跨平台 Go 项目编译运行脚本
# 支持 Windows (Git Bash/WSL) 和 Linux 系统

# 颜色定义（增强输出可读性）
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 脚本参数配置
PROJECT_NAME="my-rent-agent"  # 可修改为你的项目名称
BUILD_DIR="./bin"         # 编译输出目录
MAIN_FILE="./main.go"     # 项目入口文件路径（根据实际情况修改）

# 检查 Go 是否安装
check_go_installed() {
    if ! command -v go &> /dev/null; then
        echo -e "${RED}错误: 未检测到 Go 环境，请先安装 Go 并配置环境变量${NC}"
        exit 1
    fi
    echo -e "${GREEN}✅ Go 环境检测通过 (版本: $(go version))${NC}"
}

# 下载项目依赖
download_dependencies() {
    echo -e "${YELLOW}📥 开始下载项目依赖...${NC}"
    go mod download
    if [ $? -ne 0 ]; then
        echo -e "${RED}❌ 依赖下载失败，请检查 go.mod 文件或网络连接${NC}"
        exit 1
    fi
    echo -e "${GREEN}✅ 依赖下载完成${NC}"
}

# 编译项目（根据系统自动选择编译目标）
build_project() {
    # 创建编译输出目录
    mkdir -p ${BUILD_DIR}

    # 判断操作系统类型
    OS=$(uname -s)
    case "${OS}" in
        Linux*)
            TARGET_OS="linux"
            BIN_EXT=""
            ;;
        CYGWIN*|MINGW*|MSYS*)
            TARGET_OS="windows"
            BIN_EXT=".exe"
            ;;
        *)
            echo -e "${RED}❌ 不支持的操作系统: ${OS}${NC}"
            exit 1
            ;;
    esac

    echo -e "${YELLOW}🔨 开始编译项目 (目标系统: ${TARGET_OS})...${NC}"
    GOOS=${TARGET_OS} GOARCH=amd64 go build -o ${BUILD_DIR}/${PROJECT_NAME}${BIN_EXT} ${MAIN_FILE}

    if [ $? -ne 0 ]; then
        echo -e "${RED}❌ 项目编译失败${NC}"
        exit 1
    fi
    echo -e "${GREEN}✅ 项目编译完成，输出文件: ${BUILD_DIR}/${PROJECT_NAME}${BIN_EXT}${NC}"
}

# 运行项目
run_project() {
    echo -e "${YELLOW}🚀 开始运行项目...${NC}"
    ${BUILD_DIR}/${PROJECT_NAME}${BIN_EXT}

    if [ $? -ne 0 ]; then
        echo -e "${RED}❌ 项目运行出错${NC}"
        exit 1
    fi
}

# 主流程
main() {
    echo -e "${YELLOW}=====================================${NC}"
    echo -e "${YELLOW}      开始构建并运行 Go 项目        ${NC}"
    echo -e "${YELLOW}=====================================${NC}"

    check_go_installed
    download_dependencies
    build_project
    run_project

    echo -e "${GREEN}🎉 项目运行结束${NC}"
}

# 执行主流程
main