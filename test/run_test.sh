#!/bin/bash

# WebSocket 文件传输测试运行脚本

set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  WebSocket 文件传输测试${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""

# 检查Go是否安装
if ! command -v go &> /dev/null; then
    echo -e "${RED}错误: 未找到Go环境，请先安装Go 1.21+${NC}"
    exit 1
fi

echo -e "${GREEN}✓ Go环境检查通过${NC}"

# 进入测试目录
cd "$(dirname "$0")"

# 安装依赖
echo -e "\n${YELLOW}→ 安装依赖...${NC}"
if [ ! -f "go.sum" ]; then
    go mod download
    echo -e "${GREEN}✓ 依赖安装完成${NC}"
else
    echo -e "${GREEN}✓ 依赖已存在${NC}"
fi

# 检查服务器是否运行（公网测试时跳过）
# echo -e "\n${YELLOW}→ 检查服务器状态...${NC}"
# SERVER_URL="http://ecs.letshare.fun/health"
# 
# if curl -s -f "$SERVER_URL" > /dev/null 2>&1; then
#     echo -e "${GREEN}✓ 服务器正在运行${NC}"
# else
#     echo -e "${RED}✗ 服务器未运行${NC}"
#     echo -e "${YELLOW}请先启动服务器:${NC}"
#     echo -e "  cd /root/cloud"
#     echo -e "  MODE=local go run cmd/server/main.go"
#     exit 1
# fi
echo -e "\n${GREEN}✓ 使用公网服务器进行测试${NC}"

# 运行测试
echo -e "\n${BLUE}========================================${NC}"
echo -e "${BLUE}  开始测试${NC}"
echo -e "${BLUE}========================================${NC}\n"

# 默认参数
SERVER_WS="${SERVER_WS:-wss://ecs.letshare.fun}"
SECRET="${SECRET:-sever_auth_123}"
ROOM="${ROOM:-test-room}"
SIZE="${SIZE:-102400}"
CHUNK="${CHUNK:-8192}"

# 运行测试
go run websocket_file_transfer.go \
    -server "$SERVER_WS" \
    -secret "$SECRET" \
    -room "$ROOM" \
    -size "$SIZE" \
    -chunk "$CHUNK"

EXIT_CODE=$?

if [ $EXIT_CODE -eq 0 ]; then
    echo -e "\n${GREEN}========================================${NC}"
    echo -e "${GREEN}  ✓✓✓ 测试通过！${NC}"
    echo -e "${GREEN}========================================${NC}"
else
    echo -e "\n${RED}========================================${NC}"
    echo -e "${RED}  ✗✗✗ 测试失败！${NC}"
    echo -e "${RED}========================================${NC}"
fi

exit $EXIT_CODE

