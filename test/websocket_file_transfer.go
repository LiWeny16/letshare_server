package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// 消息类型常量
const (
	MessageTypeSubscribe            = "subscribe"
	MessageTypePublish              = "publish"
	MessageTypeMessage              = "message"
	MessageTypeFileTransferRequest  = "file:transfer:request"
	MessageTypeFileTransferAccept   = "file:transfer:accept"
	MessageTypeFileTransferReject   = "file:transfer:reject"
	MessageTypeFileTransferStart    = "file:transfer:start"
	MessageTypeFileTransferChunk    = "file:transfer:chunk"
	MessageTypeFileTransferEnd      = "file:transfer:end"
	MessageTypeFileTransferProgress = "file:transfer:progress"
)

// WebSocketMessage 表示WebSocket消息
type WebSocketMessage struct {
	Type      string          `json:"type"`
	Channel   string          `json:"channel,omitempty"`
	Event     string          `json:"event,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp int64           `json:"timestamp,omitempty"`
	Error     *ErrorInfo      `json:"error,omitempty"`
}

// ErrorInfo 错误信息
type ErrorInfo struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// FileTransferRequest 文件传输请求
type FileTransferRequest struct {
	TransferID  string `json:"transfer_id"`
	FileName    string `json:"file_name"`
	FileSize    int64  `json:"file_size"`
	FileType    string `json:"file_type"`
	ChunkSize   int    `json:"chunk_size"`
	TotalChunks int    `json:"total_chunks"`
	FromUserID  string `json:"from_user_id"`
	ToUserID    string `json:"to_user_id"`
	RoomName    string `json:"room_name"`
}

// FileTransferChunk 文件数据块元数据
type FileTransferChunk struct {
	TransferID  string `json:"transfer_id"`
	ChunkIndex  int    `json:"chunk_index"`
	ChunkSize   int    `json:"chunk_size"`
	TotalChunks int    `json:"total_chunks"`
}

// FileTransferProgress 传输进度
type FileTransferProgress struct {
	TransferID       string  `json:"transfer_id"`
	ChunkIndex       int     `json:"chunk_index"`
	TotalChunks      int     `json:"total_chunks"`
	BytesTransferred int64   `json:"bytes_transferred"`
	TotalBytes       int64   `json:"total_bytes"`
	Percentage       float64 `json:"percentage"`
}

// Client WebSocket客户端
type Client struct {
	conn           *websocket.Conn
	userID         string
	roomName       string
	receivedChunks [][]byte
	receivedData   []byte
	mu             sync.Mutex
	done           chan struct{}
	wg             sync.WaitGroup
}

// NewClient 创建客户端
func NewClient(serverURL, authToken, userID, roomName string) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("解析服务器URL失败: %w", err)
	}

	q := u.Query()
	q.Set("token", authToken)
	q.Set("userId", userID)
	u.RawQuery = q.Encode()

	log.Printf("[%s] 连接到服务器: %s", userID, u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("连接WebSocket失败: %w", err)
	}

	client := &Client{
		conn:           conn,
		userID:         userID,
		roomName:       roomName,
		receivedChunks: make([][]byte, 0),
		receivedData:   make([]byte, 0),
		done:           make(chan struct{}),
	}

	// 启动消息接收goroutine
	client.wg.Add(1)
	go client.readMessages()

	return client, nil
}

// readMessages 读取消息
func (c *Client) readMessages() {
	defer c.wg.Done()

	for {
		select {
		case <-c.done:
			return
		default:
			messageType, message, err := c.conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("[%s] WebSocket连接错误: %v", c.userID, err)
				}
				return
			}

			switch messageType {
			case websocket.TextMessage:
				c.handleTextMessage(message)
			case websocket.BinaryMessage:
				c.handleBinaryMessage(message)
			}
		}
	}
}

// handleTextMessage 处理文本消息
func (c *Client) handleTextMessage(data []byte) {
	var msg WebSocketMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("[%s] 解析消息失败: %v", c.userID, err)
		return
	}

	switch msg.Type {
	case "subscribed":
		log.Printf("[%s] ✓ 已订阅房间: %s", c.userID, msg.Channel)

	case MessageTypeFileTransferRequest:
		var req FileTransferRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			log.Printf("[%s] 解析传输请求失败: %v", c.userID, err)
			return
		}
		log.Printf("[%s] ← 收到文件传输请求: %s (%.2f KB, %d 块)",
			c.userID, req.FileName, float64(req.FileSize)/1024, req.TotalChunks)

		// 自动接受文件传输
		c.acceptFileTransfer(req.TransferID)

	case MessageTypeFileTransferAccept:
		var data map[string]interface{}
		json.Unmarshal(msg.Data, &data)
		log.Printf("[%s] ✓ 接收方已接受传输: %s", c.userID, data["transfer_id"])

	case MessageTypeFileTransferStart:
		var data map[string]interface{}
		json.Unmarshal(msg.Data, &data)
		log.Printf("[%s] → 开始接收文件: %s", c.userID, data["file_name"])

	case MessageTypeFileTransferChunk:
		var chunk FileTransferChunk
		if err := json.Unmarshal(msg.Data, &chunk); err != nil {
			log.Printf("[%s] 解析块元数据失败: %v", c.userID, err)
			return
		}
		// 元数据已接收，等待二进制数据
		log.Printf("[%s] → 收到块元数据 %d/%d", c.userID, chunk.ChunkIndex+1, chunk.TotalChunks)

	case MessageTypeFileTransferProgress:
		var progress FileTransferProgress
		if err := json.Unmarshal(msg.Data, &progress); err != nil {
			log.Printf("[%s] 解析进度失败: %v", c.userID, err)
			return
		}
		log.Printf("[%s] 传输进度: %.2f%% (%d/%d 块)",
			c.userID, progress.Percentage, progress.ChunkIndex+1, progress.TotalChunks)

	case MessageTypeFileTransferEnd:
		var data map[string]interface{}
		json.Unmarshal(msg.Data, &data)
		log.Printf("[%s] ✓ 文件传输完成: %s (%.2f KB)",
			c.userID, data["file_name"], float64(data["file_size"].(float64))/1024)

	case "error":
		if msg.Error != nil {
			log.Printf("[%s] ✗ 错误: [%d] %s", c.userID, msg.Error.Code, msg.Error.Message)
		}

	default:
		log.Printf("[%s] 收到消息: type=%s, channel=%s", c.userID, msg.Type, msg.Channel)
	}
}

// handleBinaryMessage 处理二进制消息
func (c *Client) handleBinaryMessage(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 打印前32字节用于调试
	preview := data
	if len(data) > 32 {
		preview = data[:32]
	}
	log.Printf("[%s] ← 收到二进制消息 (大小: %d 字节), 前32字节: %x", c.userID, len(data), preview)

	// 服务器已经处理了元数据，转发给接收方的二进制消息只包含实际文件数据
	// 所以这里直接保存收到的数据即可
	chunk := make([]byte, len(data))
	copy(chunk, data)
	c.receivedChunks = append(c.receivedChunks, chunk)
	c.receivedData = append(c.receivedData, data...)

	log.Printf("[%s]   总计接收: %d 字节 (块数: %d)",
		c.userID, len(c.receivedData), len(c.receivedChunks))
}

// Subscribe 订阅房间
func (c *Client) Subscribe(roomName string) error {
	msg := WebSocketMessage{
		Type:    MessageTypeSubscribe,
		Channel: roomName,
		Event:   "signal:all",
	}

	log.Printf("[%s] 订阅房间: %s", c.userID, roomName)
	return c.conn.WriteJSON(msg)
}

// acceptFileTransfer 接受文件传输
func (c *Client) acceptFileTransfer(transferID string) error {
	data := map[string]interface{}{
		"transfer_id": transferID,
	}
	dataBytes, _ := json.Marshal(data)

	msg := WebSocketMessage{
		Type:    MessageTypeFileTransferAccept,
		Channel: c.roomName,
		Data:    dataBytes,
	}

	log.Printf("[%s] 接受文件传输: %s", c.userID, transferID)
	return c.conn.WriteJSON(msg)
}

// SendFileTransferRequest 发送文件传输请求
func (c *Client) SendFileTransferRequest(toUserID, fileName string, fileSize int64, chunkSize int) (string, error) {
	transferID := uuid.New().String()
	totalChunks := int(fileSize / int64(chunkSize))
	if fileSize%int64(chunkSize) != 0 {
		totalChunks++
	}

	request := FileTransferRequest{
		TransferID:  transferID,
		FileName:    fileName,
		FileSize:    fileSize,
		FileType:    "application/octet-stream",
		ChunkSize:   chunkSize,
		TotalChunks: totalChunks,
		FromUserID:  c.userID,
		ToUserID:    toUserID,
		RoomName:    c.roomName,
	}

	dataBytes, _ := json.Marshal(request)
	msg := WebSocketMessage{
		Type:    MessageTypeFileTransferRequest,
		Channel: c.roomName,
		Data:    dataBytes,
	}

	log.Printf("[%s] → 发送文件传输请求: %s (%.2f KB, %d 块)",
		c.userID, fileName, float64(fileSize)/1024, totalChunks)

	if err := c.conn.WriteJSON(msg); err != nil {
		return "", err
	}

	return transferID, nil
}

// StartFileTransfer 开始文件传输
func (c *Client) StartFileTransfer(transferID string, fileName string, fileSize int64, totalChunks int) error {
	data := map[string]interface{}{
		"transfer_id":  transferID,
		"file_name":    fileName,
		"file_size":    fileSize,
		"total_chunks": totalChunks,
	}
	dataBytes, _ := json.Marshal(data)

	msg := WebSocketMessage{
		Type:    MessageTypeFileTransferStart,
		Channel: c.roomName,
		Data:    dataBytes,
	}

	log.Printf("[%s] → 开始文件传输: %s", c.userID, transferID)
	return c.conn.WriteJSON(msg)
}

// SendFileChunk 发送文件数据块
func (c *Client) SendFileChunk(transferID string, chunkIndex int, totalChunks int, data []byte) error {
	// 创建元数据
	chunkMeta := FileTransferChunk{
		TransferID:  transferID,
		ChunkIndex:  chunkIndex,
		ChunkSize:   len(data),
		TotalChunks: totalChunks,
	}

	metaJSON, err := json.Marshal(chunkMeta)
	if err != nil {
		return err
	}

	// 组合元数据和数据 (元数据 + 数据)
	// 元数据填充到256字节
	metaPadded := make([]byte, 256)
	copy(metaPadded, metaJSON)

	// 合并元数据和实际数据
	message := append(metaPadded, data...)

	// 打印数据的前32字节用于调试
	preview := data
	if len(data) > 32 {
		preview = data[:32]
	}
	log.Printf("[%s] → 发送数据块 %d/%d (数据大小: %d 字节, 总消息: %d 字节), 数据前32字节: %x",
		c.userID, chunkIndex+1, totalChunks, len(data), len(message), preview)

	return c.conn.WriteMessage(websocket.BinaryMessage, message)
}

// EndFileTransfer 结束文件传输
func (c *Client) EndFileTransfer(transferID string, fileName string, fileSize int64) error {
	data := map[string]interface{}{
		"transfer_id": transferID,
		"file_name":   fileName,
		"file_size":   fileSize,
	}
	dataBytes, _ := json.Marshal(data)

	msg := WebSocketMessage{
		Type:    MessageTypeFileTransferEnd,
		Channel: c.roomName,
		Data:    dataBytes,
	}

	log.Printf("[%s] → 文件传输结束通知: %s", c.userID, transferID)
	return c.conn.WriteJSON(msg)
}

// Close 关闭连接
func (c *Client) Close() {
	close(c.done)
	c.conn.Close()
	c.wg.Wait()
}

// GetReceivedData 获取接收到的数据
func (c *Client) GetReceivedData() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.receivedData
}

// generateAuthToken 生成认证token
func generateAuthToken(secret string) string {
	hash := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(hash[:])
}

// generateTestFile 生成测试文件数据
func generateTestFile(size int) []byte {
	data := make([]byte, size)
	rand.Read(data)
	return data
}

// calculateChecksum 计算数据校验和
func calculateChecksum(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func main() {
	// 命令行参数
	serverURL := flag.String("server", "ws://localhost:8080/ws", "WebSocket服务器URL")
	authSecret := flag.String("secret", "sever_auth_123", "认证密钥")
	roomName := flag.String("room", "test-room", "测试房间名称")
	fileSize := flag.Int("size", 102400, "测试文件大小(字节)")
	chunkSize := flag.Int("chunk", 8192, "数据块大小(字节)")
	flag.Parse()

	// 生成认证token
	authToken := generateAuthToken(*authSecret)
	log.Printf("认证Token: %s", authToken)

	// 设置中断信号处理
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// 创建发送方和接收方
	senderUserID := "sender-" + uuid.New().String()[:8]
	receiverUserID := "receiver-" + uuid.New().String()[:8]

	log.Printf("\n========== 开始WebSocket文件传输测试 ==========")
	log.Printf("服务器: %s", *serverURL)
	log.Printf("房间: %s", *roomName)
	log.Printf("文件大小: %.2f KB", float64(*fileSize)/1024)
	log.Printf("块大小: %.2f KB", float64(*chunkSize)/1024)
	log.Printf("发送方ID: %s", senderUserID)
	log.Printf("接收方ID: %s", receiverUserID)
	log.Printf("===============================================\n")

	// 连接发送方
	log.Println("→ 连接发送方...")
	sender, err := NewClient(*serverURL, authToken, senderUserID, *roomName)
	if err != nil {
		log.Fatalf("创建发送方失败: %v", err)
	}
	defer sender.Close()
	log.Println("✓ 发送方连接成功")

	// 连接接收方
	log.Println("\n→ 连接接收方...")
	receiver, err := NewClient(*serverURL, authToken, receiverUserID, *roomName)
	if err != nil {
		log.Fatalf("创建接收方失败: %v", err)
	}
	defer receiver.Close()
	log.Println("✓ 接收方连接成功")

	// 等待连接稳定
	time.Sleep(1 * time.Second)

	// 订阅房间
	log.Println("\n→ 订阅房间...")
	if err := sender.Subscribe(*roomName); err != nil {
		log.Fatalf("发送方订阅房间失败: %v", err)
	}
	if err := receiver.Subscribe(*roomName); err != nil {
		log.Fatalf("接收方订阅房间失败: %v", err)
	}
	time.Sleep(1 * time.Second)

	// 生成测试文件
	log.Printf("\n→ 生成测试文件 (%.2f KB)...", float64(*fileSize)/1024)
	testData := generateTestFile(*fileSize)
	originalChecksum := calculateChecksum(testData)
	log.Printf("✓ 文件已生成，校验和: %s", originalChecksum[:16]+"...")

	// 发送文件传输请求
	log.Println("\n========== 开始文件传输 ==========")
	transferID, err := sender.SendFileTransferRequest(receiverUserID, "test-file.bin", int64(*fileSize), *chunkSize)
	if err != nil {
		log.Fatalf("发送传输请求失败: %v", err)
	}

	// 等待接收方接受
	time.Sleep(2 * time.Second)

	// 开始传输
	totalChunks := *fileSize / *chunkSize
	if *fileSize%*chunkSize != 0 {
		totalChunks++
	}

	if err := sender.StartFileTransfer(transferID, "test-file.bin", int64(*fileSize), totalChunks); err != nil {
		log.Fatalf("开始传输失败: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// 发送数据块
	log.Println("\n→ 发送数据块...")
	startTime := time.Now()
	reader := bytes.NewReader(testData)
	chunkIndex := 0

	for {
		chunk := make([]byte, *chunkSize)
		n, err := reader.Read(chunk)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("读取数据失败: %v", err)
		}

		// 发送实际读取的数据
		if err := sender.SendFileChunk(transferID, chunkIndex, totalChunks, chunk[:n]); err != nil {
			log.Fatalf("发送数据块失败: %v", err)
		}

		chunkIndex++
		time.Sleep(50 * time.Millisecond) // 模拟真实传输延迟
	}

	duration := time.Since(startTime)
	speed := float64(*fileSize) / duration.Seconds() / 1024 // KB/s

	log.Printf("✓ 所有数据块已发送 (%d 块)", chunkIndex)
	log.Printf("传输耗时: %v", duration)
	log.Printf("传输速度: %.2f KB/s", speed)

	// 结束传输
	time.Sleep(500 * time.Millisecond)
	if err := sender.EndFileTransfer(transferID, "test-file.bin", int64(*fileSize)); err != nil {
		log.Fatalf("结束传输失败: %v", err)
	}

	// 等待接收完成
	log.Println("\n→ 等待接收完成...")
	time.Sleep(2 * time.Second)

	// 验证接收到的数据
	log.Println("\n========== 验证数据完整性 ==========")
	receivedData := receiver.GetReceivedData()
	receivedChecksum := calculateChecksum(receivedData)

	log.Printf("原始数据大小: %d 字节", len(testData))
	log.Printf("接收数据大小: %d 字节", len(receivedData))
	log.Printf("原始校验和: %s", originalChecksum[:32]+"...")
	log.Printf("接收校验和: %s", receivedChecksum[:32]+"...")

	if originalChecksum == receivedChecksum {
		log.Println("\n✓✓✓ 测试通过！数据完整性验证成功！✓✓✓")
		log.Printf("成功通过WebSocket传输了 %.2f KB 数据", float64(*fileSize)/1024)
	} else {
		log.Println("\n✗✗✗ 测试失败！数据校验和不匹配！✗✗✗")
		log.Printf("预期: %s", originalChecksum[:32]+"...")
		log.Printf("实际: %s", receivedChecksum[:32]+"...")
		os.Exit(1)
	}

	log.Println("\n========== 测试完成 ==========")
	log.Println("按 Ctrl+C 退出")

	// 等待中断信号
	<-interrupt
	log.Println("\n正在关闭连接...")
}

