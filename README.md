# LetShare WebSocket 服务器

与前端 LetShare 应用配套的高性能 WebSocket 服务器，完全兼容 Ably 协议，支持高并发实时通信。

## 功能特性

- 🔥 **高性能**: 基于 Golang + Gin + gorilla/websocket，支持数千并发连接
- 🔒 **JWT 认证**: 简单安全的 JWT token 认证机制
- 🏠 **房间管理**: 完整的房间订阅/发布机制，兼容 Ably 格式
- 📊 **轻量监控**: 内置健康检查和性能监控
- 🚀 **Docker 部署**: 完整的容器化部署方案
- 📝 **智能日志**: 错误日志本地持久化，自动清理
- 🌐 **CORS 支持**: 完整的跨域配置

## 架构设计

### 目录结构

```
server/
├── cmd/server/          # 主程序入口
├── internal/
│   ├── config/         # 配置管理
│   ├── handler/        # HTTP/WebSocket 处理器
│   ├── middleware/     # 中间件
│   ├── model/          # 数据模型
│   └── service/        # 业务服务
├── pkg/
│   └── logger/         # 日志管理
├── configs/            # 配置文件
├── Dockerfile          # Docker 配置
└── docker-compose.yml  # Docker Compose 配置
```

### 技术栈

- **Go 1.21**: 最新 LTS 版本
- **Gin**: 高性能 Web 框架
- **gorilla/websocket**: 成熟的 WebSocket 库
- **JWT**: 轻量级认证
- **Logrus**: 结构化日志
- **Viper**: 配置管理

## 部署方式

### 1. Docker 部署（推荐）

**环境变量控制运行模式：**

```bash
# 本地调试模式
export MODE=local
docker-compose up

# 生产环境模式
export MODE=production
docker-compose up -d
```

**快速启动：**

```bash
# 构建并启动
docker-compose up --build

# 后台运行
docker-compose up -d
```

### 2. 本地开发

**安装 Go 1.21:**
```bash
# 下载并安装 Go 1.21
wget https://go.dev/dl/go1.21.6.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.21.6.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

**运行服务：**
```bash
# 本地调试模式
MODE=local go run cmd/server/main.go

# 生产模式
MODE=production go run cmd/server/main.go
```

## 配置说明

### 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `MODE` | 运行模式 (local/production) | `local` |
| `LETSHARE_SERVER_PORT` | 服务端口 | `8080` |
| `LETSHARE_JWT_SECRET` | JWT 密钥 | `letshare-jwt-secret-key-2024` |
| `LETSHARE_LOG_LEVEL` | 日志级别 | `info` |

### 配置文件

**本地调试 (configs/local.yaml):**
```yaml
server:
  port: "8080"
jwt:
  secret: "letshare-jwt-secret-key-2024-local"
  expiration_hours: 720
cors:
  allowed_origins:
    - "http://localhost:3000"
    - "http://localhost:5173"
    - "https://letshare.fun"
log:
  level: "debug"
  max_entries: 200
websocket:
  max_room_users: 50
```

**生产环境 (configs/production.yaml):**
```yaml
server:
  port: "8080"
jwt:
  secret: "letshare-jwt-secret-key-2024-production"
cors:
  allowed_origins:
    - "https://letshare.fun"
    - "https://www.letshare.fun"
    - "https://cdn.letshare.fun"
log:
  level: "info"
```

## WebSocket 协议

### 连接认证

```
wss://your-server.com/ws?token=your-jwt-token
```

### 消息格式

**订阅房间:**
```json
{
  "type": "subscribe",
  "channel": "room-name",
  "event": "signal:all"
}
```

**发布消息:**
```json
{
  "type": "publish",
  "channel": "room-name", 
  "event": "signal:all",
  "data": {
    "type": "discover",
    "from": "user-id",
    "userType": "desktop"
  }
}
```

**服务器响应:**
```json
{
  "type": "message",
  "channel": "room-name",
  "event": "signal:all",
  "data": { "type": "discover", "from": "other-user" },
  "timestamp": 1704067200000
}
```

## API 端点

### 健康检查
```bash
GET /health
```

### 监控指标
```bash
GET /metrics
```

返回服务器状态、内存使用、WebSocket 连接数等信息。

## 前端集成

在前端 `mobx.ts` 中配置自定义服务器：

```typescript
// 设置自定义服务器URL
settingsStore.update("customServerUrl", "wss://your-server.com");
settingsStore.update("customAuthToken", "your-jwt-token");
settingsStore.update("serverMode", "custom"); // 强制使用自定义服务器
```

## 性能优化

### 3M 内存 / 2核 CPU 建议配置

```yaml
websocket:
  max_room_users: 50        # 单房间最大用户数
log:
  max_entries: 200          # 错误日志保留条数
```

### 并发能力

- **理论并发**: 10,000+ WebSocket 连接
- **实际建议**: 在 2 核 3M 内存环境下，建议 1,000-2,000 并发连接
- **单房间限制**: 50 用户

## 监控和日志

### 错误日志

- 自动保存到 `logs/errors.log`
- 只记录警告和错误级别
- 自动清理，保留最新 200 条
- JSON 格式，便于分析

### 监控指标

访问 `/metrics` 端点获取：
- 连接数统计
- 消息吞吐量
- 内存使用情况
- 系统性能指标

## 安全说明

- JWT token 有效期 30 天
- 支持 CORS 域名白名单
- 非 root 用户运行
- 自动清理非活跃连接

## 故障排除

### 常见问题

1. **连接被拒绝**: 检查 CORS 配置和域名白名单
2. **JWT 验证失败**: 确认 token 格式和密钥正确
3. **房间已满**: 单房间限制 50 用户
4. **内存不足**: 调整日志保留数量和连接数限制

### 查看日志

```bash
# Docker 日志
docker logs letshare-server

# 错误日志文件
cat logs/errors.log
```

## 开发相关

### 生成 JWT Token

```go
// 示例：生成测试 token
jwtService := service.NewJWTService("your-secret", 720)
token, err := jwtService.GenerateToken("user123", "desktop", "room456")
```

### 房间名验证规则

- 长度：2-12 个字符
- 支持：中文、英文、数字、空格、下划线、中划线
- 正则：`[\u4e00-\u9fa5a-zA-Z0-9 _-]+`

## 更新日志

- **v1.0.0**: 初始版本，完整的 Ably 兼容实现
- 支持高并发 WebSocket 连接
- JWT 认证和房间管理
- Docker 容器化部署
