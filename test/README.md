# WebSocket 文件传输测试

这是一个用于测试 LetShare WebSocket 服务器文件传输功能的测试脚本。

## 功能特性

- ✅ 模拟发送方和接收方两个客户端
- ✅ 完整的文件传输流程测试（请求、接受、传输、完成）
- ✅ 支持分块传输
- ✅ 实时进度显示
- ✅ 数据完整性校验（SHA256）
- ✅ 传输速度统计

## 使用方法

### 1. 安装依赖

```bash
cd /root/cloud/test
go mod download
```

### 2. 启动服务器

确保 LetShare WebSocket 服务器正在运行：

```bash
cd /root/cloud
MODE=local go run cmd/server/main.go
```

### 3. 运行测试

基本测试（使用默认参数）：

```bash
cd /root/cloud/test
go run websocket_file_transfer_test.go
```

### 4. 自定义参数

```bash
go run websocket_file_transfer_test.go \
  -server "ws://localhost:8080/ws" \
  -secret "sever_auth_123" \
  -room "test-room" \
  -size 102400 \
  -chunk 8192
```

## 参数说明

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-server` | WebSocket服务器地址 | `ws://localhost:8080/ws` |
| `-secret` | 认证密钥 | `sever_auth_123` |
| `-room` | 测试房间名称 | `test-room` |
| `-size` | 测试文件大小（字节） | `102400` (100KB) |
| `-chunk` | 数据块大小（字节） | `8192` (8KB) |

## 测试场景示例

### 小文件测试 (10KB)

```bash
go run websocket_file_transfer_test.go -size 10240 -chunk 1024
```

### 中等文件测试 (1MB)

```bash
go run websocket_file_transfer_test.go -size 1048576 -chunk 16384
```

### 大文件测试 (10MB)

```bash
go run websocket_file_transfer_test.go -size 10485760 -chunk 32768
```

### 远程服务器测试

```bash
go run websocket_file_transfer_test.go \
  -server "wss://your-server.com/ws" \
  -secret "your-secret-key"
```

## 测试流程

测试脚本会自动执行以下步骤：

1. **连接建立**
   - 创建发送方客户端
   - 创建接收方客户端
   - 两者都连接到WebSocket服务器

2. **房间订阅**
   - 发送方订阅测试房间
   - 接收方订阅测试房间

3. **文件传输请求**
   - 生成随机测试数据
   - 计算原始数据校验和
   - 发送方发起文件传输请求

4. **接受传输**
   - 接收方自动接受传输请求

5. **数据传输**
   - 发送方按块发送文件数据
   - 服务器转发数据到接收方
   - 实时显示传输进度

6. **传输完成**
   - 发送方通知传输结束
   - 计算传输速度

7. **数据验证**
   - 计算接收数据校验和
   - 对比原始数据和接收数据
   - 验证数据完整性

## 输出示例

```
========== 开始WebSocket文件传输测试 ==========
服务器: ws://localhost:8080/ws
房间: test-room
文件大小: 100.00 KB
块大小: 8.00 KB
发送方ID: sender-abc123
接收方ID: receiver-xyz789
===============================================

→ 连接发送方...
✓ 发送方连接成功

→ 连接接收方...
✓ 接收方连接成功

→ 订阅房间...
[sender-abc123] ✓ 已订阅房间: test-room
[receiver-xyz789] ✓ 已订阅房间: test-room

→ 生成测试文件 (100.00 KB)...
✓ 文件已生成，校验和: 8f3a4b2c1d5e6f7a...

========== 开始文件传输 ==========
[sender-abc123] → 发送文件传输请求: test-file.bin (100.00 KB, 13 块)
[receiver-xyz789] ← 收到文件传输请求: test-file.bin (100.00 KB, 13 块)
[receiver-xyz789] 接受文件传输: transfer-id-123
[sender-abc123] ✓ 接收方已接受传输: transfer-id-123

→ 发送数据块...
[sender-abc123] → 发送数据块 1/13 (大小: 8192 字节)
[receiver-xyz789] → 收到数据块 (大小: 8192 字节, 总计: 8192 字节)
[sender-abc123] 传输进度: 7.69% (1/13 块)
...
✓ 所有数据块已发送 (13 块)
传输耗时: 1.2s
传输速度: 83.33 KB/s

========== 验证数据完整性 ==========
原始数据大小: 102400 字节
接收数据大小: 102400 字节
原始校验和: 8f3a4b2c1d5e6f7a...
接收校验和: 8f3a4b2c1d5e6f7a...

✓✓✓ 测试通过！数据完整性验证成功！✓✓✓
成功通过WebSocket传输了 100.00 KB 数据

========== 测试完成 ==========
```

## 错误排查

### 连接失败

```
创建发送方失败: dial tcp [::1]:8080: connect: connection refused
```

**解决方法**：确保WebSocket服务器正在运行

### 认证失败

```
[sender-123] ✗ 错误: [401] token验证失败
```

**解决方法**：检查 `-secret` 参数是否与服务器配置匹配

### 传输超时

```
[sender-123] WebSocket连接错误: i/o timeout
```

**解决方法**：
- 检查网络连接
- 增大块大小（减少传输次数）
- 调整传输延迟参数

### 数据校验失败

```
✗✗✗ 测试失败！数据校验和不匹配！✗✗✗
```

**解决方法**：
- 检查服务器日志
- 减小文件大小或块大小进行测试
- 检查网络稳定性

## 技术细节

### 消息格式

测试脚本实现了完整的文件传输协议：

1. **文本消息（JSON）**：控制消息
   - 订阅房间
   - 文件传输请求
   - 接受/拒绝传输
   - 开始/结束传输

2. **二进制消息**：文件数据
   - 前256字节：JSON元数据（块索引、大小等）
   - 剩余部分：实际文件数据

### 数据完整性

- 使用 SHA256 计算校验和
- 发送前后对比校验和
- 确保数据传输无损

### 并发安全

- 使用 `sync.Mutex` 保护共享数据
- 使用 `sync.WaitGroup` 等待goroutine完成
- 使用 channel 进行协程间通信

## 扩展测试

### 并发传输测试

可以修改脚本支持多个发送方同时传输：

```bash
# 在不同终端运行多个测试实例
go run websocket_file_transfer_test.go -room "room1" &
go run websocket_file_transfer_test.go -room "room2" &
go run websocket_file_transfer_test.go -room "room3" &
```

### 压力测试

```bash
# 持续运行测试
while true; do
  go run websocket_file_transfer_test.go -size 1048576
  sleep 1
done
```

### 网络质量测试

在不同网络条件下测试：
- 本地网络：低延迟
- 远程服务器：高延迟
- 移动网络：不稳定连接

## 常见问题

**Q: 为什么接收到的数据大小不对？**

A: 检查二进制消息的解析逻辑，确保正确跳过元数据部分。

**Q: 如何调试消息流？**

A: 测试脚本已包含详细的日志输出，查看每条消息的类型和内容。

**Q: 支持哪些文件类型？**

A: 测试脚本使用随机二进制数据，可以传输任何文件类型。

**Q: 最大文件大小限制？**

A: 取决于服务器配置的 `maxFileSize` 参数（默认100MB）。

## 许可证

与 LetShare 项目相同。

