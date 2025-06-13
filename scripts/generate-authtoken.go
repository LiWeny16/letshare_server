package main

import (
	"fmt"
	"letshare-server/internal/service"
	"log"
	"os"
	
	"github.com/joho/godotenv"
)

func main() {
	fmt.Println("=== LetShare AuthToken 生成器 ===")
	
	// 加载.env文件
	if err := godotenv.Load(); err != nil {
		fmt.Printf("警告: 无法加载.env文件: %v\n", err)
		fmt.Println("将使用系统环境变量或默认值")
	} else {
		fmt.Println("✅ 成功加载.env文件")
	}
	
	// 检查环境变量
	secretKey := os.Getenv("SERVER_AUTH_SECRET")
	if secretKey == "" {
		fmt.Println("注意: 未设置 SERVER_AUTH_SECRET 环境变量，使用默认密钥 'sever_auth_123'")
		secretKey = "sever_auth_123"
	} else {
		fmt.Printf("使用环境变量密钥: %s\n", secretKey)
	}
	
	// 创建认证服务
	authService := service.NewAuthService()
	
	// 生成token
	token, err := authService.GenerateAuthToken()
	if err != nil {
		log.Fatalf("生成AuthToken失败: %v", err)
	}
	
	fmt.Printf("\n生成的AuthToken:\n")
	fmt.Printf("================================\n")
	fmt.Printf("%s\n", token)
	fmt.Printf("================================\n")
	
	// 验证生成的token
	if err := authService.ValidateAuthToken(token); err != nil {
		log.Fatalf("验证生成的token失败: %v", err)
	}
	
	fmt.Printf("\n✅ Token验证成功！\n")
	fmt.Printf("\n使用方法:\n")
	fmt.Printf("1. 复制上面的AuthToken到前端配置\n")
	fmt.Printf("2. WebSocket连接URL示例:\n")
	fmt.Printf("   ws://localhost:8080/ws?token=%s\n", token)
	fmt.Printf("   wss://your-domain.com/ws?token=%s\n", token)
	fmt.Printf("\n注意: 请妥善保管此token，它等同于服务器访问凭证！\n")
} 