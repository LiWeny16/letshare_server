package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: go run generate-token.go <user_id> [user_type] [room_id]")
		fmt.Println("示例: go run generate-token.go user123 desktop room456")
		os.Exit(1)
	}

	userID := os.Args[1]
	userType := "desktop"
	roomID := ""

	if len(os.Args) > 2 {
		userType = os.Args[2]
	}
	if len(os.Args) > 3 {
		roomID = os.Args[3]
	}

	// 使用默认密钥
	secret := "letshare-jwt-secret-key-2024"
	if envSecret := os.Getenv("JWT_SECRET"); envSecret != "" {
		secret = envSecret
	}

	// 创建token
	now := time.Now()
	expirationHours := 720 // 30天

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":   userID,
		"user_type": userType,
		"room_id":   roomID,
		"iat":       now.Unix(),
		"exp":       now.Add(time.Duration(expirationHours) * time.Hour).Unix(),
	})

	tokenString, err := token.SignedString([]byte(secret))
	if err != nil {
		log.Fatalf("生成token失败: %v", err)
	}

	fmt.Printf("JWT Token 生成成功:\n")
	fmt.Printf("用户ID: %s\n", userID)
	fmt.Printf("用户类型: %s\n", userType)
	if roomID != "" {
		fmt.Printf("房间ID: %s\n", roomID)
	}
	fmt.Printf("有效期: %d 小时\n", expirationHours)
	fmt.Printf("过期时间: %s\n", now.Add(time.Duration(expirationHours)*time.Hour).Format("2006-01-02 15:04:05"))
	fmt.Printf("\nToken:\n%s\n", tokenString)
	fmt.Printf("\n连接URL示例:\n")
	fmt.Printf("ws://localhost:8080/ws?token=%s\n", tokenString)
	fmt.Printf("wss://your-domain.com/ws?token=%s\n", tokenString)
} 