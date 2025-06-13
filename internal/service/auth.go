package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
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

// GenerateAuthToken 生成加密的认证token
func (a *AuthService) GenerateAuthToken() (string, error) {
	// 使用密钥作为明文进行AES加密
	plaintext := []byte(a.secretKey)
	
	// 创建AES加密器（使用密钥的前32字节作为AES密钥）
	key := make([]byte, 32)
	copy(key, []byte(a.secretKey))
	if len(a.secretKey) < 32 {
		// 如果密钥长度不够，用密钥重复填充
		for i := len(a.secretKey); i < 32; i++ {
			key[i] = key[i%len(a.secretKey)]
		}
	}
	
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("创建AES加密器失败: %w", err)
	}
	
	// 使用GCM模式
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建GCM失败: %w", err)
	}
	
	// 生成随机nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成nonce失败: %w", err)
	}
	
	// 加密
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	
	// 转换为十六进制字符串
	return hex.EncodeToString(ciphertext), nil
}

// ValidateAuthToken 验证认证token
func (a *AuthService) ValidateAuthToken(token string) error {
	if token == "" {
		return fmt.Errorf("token不能为空")
	}
	
	// 从十六进制字符串解码
	ciphertext, err := hex.DecodeString(token)
	if err != nil {
		return fmt.Errorf("token格式无效: %w", err)
	}
	
	// 创建AES解密器
	key := make([]byte, 32)
	copy(key, []byte(a.secretKey))
	if len(a.secretKey) < 32 {
		// 如果密钥长度不够，用密钥重复填充
		for i := len(a.secretKey); i < 32; i++ {
			key[i] = key[i%len(a.secretKey)]
		}
	}
	
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("创建AES解密器失败: %w", err)
	}
	
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("创建GCM失败: %w", err)
	}
	
	// 检查密文长度
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return fmt.Errorf("token长度无效")
	}
	
	// 提取nonce和密文
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	
	// 解密
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return fmt.Errorf("token解密失败: %w", err)
	}
	
	// 验证解密后的内容是否与密钥匹配
	if string(plaintext) != a.secretKey {
		return fmt.Errorf("token验证失败")
	}
	
	return nil
} 