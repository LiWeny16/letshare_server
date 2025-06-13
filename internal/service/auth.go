package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

type AuthService struct {
	secretKey string
}

func NewAuthService() *AuthService {
	// 从环境变量获取密钥，默认值为 "sever_auth_123"
	secretKey := os.Getenv("SERVER_AUTH_SECRET")
	if secretKey == "" {
		secretKey = "sever_auth_123"
	}
	
	return &AuthService{
		secretKey: secretKey,
	}
}

// GenerateAuthToken 生成基于密钥的固定认证token
func (a *AuthService) GenerateAuthToken() (string, error) {
	// 使用SHA256哈希生成固定的token
	hash := sha256.Sum256([]byte(a.secretKey))
	return hex.EncodeToString(hash[:]), nil
}

// ValidateAuthToken 验证认证token
func (a *AuthService) ValidateAuthToken(token string) error {
	if token == "" {
		return fmt.Errorf("token不能为空")
	}
	
	// 生成期望的token
	expectedToken, err := a.GenerateAuthToken()
	if err != nil {
		return fmt.Errorf("生成期望token失败: %w", err)
	}
	
	// 直接比较token
	if token != expectedToken {
		return fmt.Errorf("token验证失败")
	}
	
	return nil
}