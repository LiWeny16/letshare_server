#!/bin/bash

# LetShare 服务器启动脚本

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 检查是否安装了Go
check_go() {
    if ! command -v go &> /dev/null; then
        echo -e "${RED}错误: Go 未安装${NC}"
        echo "请安装 Go 1.21 或更高版本"
        exit 1
    fi
    
    GO_VERSION=$(go version | cut -d' ' -f3 | cut -d'o' -f2)
    echo -e "${GREEN}Go 版本: $GO_VERSION${NC}"
}

# 检查是否安装了Docker
check_docker() {
    if ! command -v docker &> /dev/null; then
        echo -e "${YELLOW}警告: Docker 未安装，无法使用容器化部署${NC}"
        return 1
    fi
    echo -e "${GREEN}Docker 已安装${NC}"
    return 0
}

# 显示帮助信息
show_help() {
    echo "LetShare 服务器启动脚本"
    echo ""
    echo "用法: $0 [选项]"
    echo ""
    echo "选项:"
    echo "  dev, local     - 本地开发模式"
    echo "  prod           - 生产模式"
    echo "  docker-dev     - Docker 开发模式"
    echo "  docker-prod    - Docker 生产模式"
    echo "  build          - 仅构建，不运行"
    echo "  clean          - 清理构建文件和Docker容器"
    echo "  test          - 运行测试"
    echo "  help, -h       - 显示此帮助信息"
    echo ""
    echo "环境变量:"
    echo "  PORT          - 服务端口 (默认: 8080)"
    echo "  JWT_SECRET    - JWT密钥"
    echo "  LOG_LEVEL     - 日志级别 (debug, info, warn, error)"
}

# 本地开发模式
start_local() {
    echo -e "${GREEN}启动本地开发模式...${NC}"
    check_go
    
    export MODE=local
    export LETSHARE_SERVER_PORT=${PORT:-8080}
    export LETSHARE_JWT_SECRET=${JWT_SECRET:-"letshare-jwt-secret-key-2024-local"}
    export LETSHARE_LOG_LEVEL=${LOG_LEVEL:-"debug"}
    
    echo -e "${YELLOW}正在安装依赖...${NC}"
    go mod tidy
    
    echo -e "${GREEN}启动服务器 (端口: ${LETSHARE_SERVER_PORT})...${NC}"
    go run cmd/server/main.go
}

# 生产模式
start_prod() {
    echo -e "${GREEN}启动生产模式...${NC}"
    check_go
    
    export MODE=production
    export LETSHARE_SERVER_PORT=${PORT:-8080}
    export LETSHARE_JWT_SECRET=${JWT_SECRET:-"letshare-jwt-secret-key-2024-production"}
    export LETSHARE_LOG_LEVEL=${LOG_LEVEL:-"info"}
    
    echo -e "${YELLOW}正在构建...${NC}"
    go build -o bin/letshare-server cmd/server/main.go
    
    echo -e "${GREEN}启动服务器 (端口: ${LETSHARE_SERVER_PORT})...${NC}"
    ./bin/letshare-server
}

# Docker开发模式
start_docker_dev() {
    echo -e "${GREEN}启动Docker开发模式...${NC}"
    if ! check_docker; then
        exit 1
    fi
    
    export MODE=local
    docker-compose up --build
}

# Docker生产模式
start_docker_prod() {
    echo -e "${GREEN}启动Docker生产模式...${NC}"
    if ! check_docker; then
        exit 1
    fi
    
    export MODE=production
    docker-compose up -d --build
    
    echo -e "${GREEN}服务已在后台启动${NC}"
    echo "查看日志: docker-compose logs -f letshare-server"
    echo "停止服务: docker-compose down"
}

# 构建
build_only() {
    echo -e "${GREEN}构建服务器...${NC}"
    check_go
    
    go mod tidy
    mkdir -p bin
    go build -o bin/letshare-server cmd/server/main.go
    
    echo -e "${GREEN}构建完成: bin/letshare-server${NC}"
}

# 清理
clean() {
    echo -e "${YELLOW}清理构建文件和Docker容器...${NC}"
    
    # 清理二进制文件
    rm -rf bin/
    rm -rf logs/
    
    # 清理Docker容器
    if check_docker; then
        docker-compose down -v 2>/dev/null || true
        docker system prune -f 2>/dev/null || true
    fi
    
    echo -e "${GREEN}清理完成${NC}"
}

# 运行测试
run_tests() {
    echo -e "${GREEN}运行测试...${NC}"
    check_go
    
    go mod tidy
    go test -v ./...
}

# 主逻辑
case "${1:-local}" in
    "dev"|"local")
        start_local
        ;;
    "prod"|"production")
        start_prod
        ;;
    "docker-dev")
        start_docker_dev
        ;;
    "docker-prod")
        start_docker_prod
        ;;
    "build")
        build_only
        ;;
    "clean")
        clean
        ;;
    "test")
        run_tests
        ;;
    "help"|"-h"|"--help")
        show_help
        ;;
    *)
        echo -e "${RED}错误: 未知选项 '$1'${NC}"
        show_help
        exit 1
        ;;
esac 