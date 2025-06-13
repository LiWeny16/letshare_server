package service

import (
	"fmt"
	"letshare-server/internal/model"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type JWTService struct {
	secret          string
	expirationHours int
}

func NewJWTService(secret string, expirationHours int) *JWTService {
	return &JWTService{
		secret:          secret,
		expirationHours: expirationHours,
	}
}

// GenerateToken 生成JWT token
func (j *JWTService) GenerateToken(userID, userType, roomID string) (string, error) {
	now := time.Now()
	claims := &model.JWTClaims{
		UserID:    userID,
		UserType:  userType,
		RoomID:    roomID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Duration(j.expirationHours) * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":   claims.UserID,
		"user_type": claims.UserType,
		"room_id":   claims.RoomID,
		"iat":       claims.IssuedAt,
		"exp":       claims.ExpiresAt,
	})

	return token.SignedString([]byte(j.secret))
}

// ValidateToken 验证JWT token
func (j *JWTService) ValidateToken(tokenString string) (*model.JWTClaims, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(j.secret), nil
	})

	if err != nil {
		return nil, fmt.Errorf("token解析失败: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("token无效")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("claims解析失败")
	}

	// 检查过期时间
	if exp, ok := claims["exp"].(float64); ok {
		if time.Now().Unix() > int64(exp) {
			return nil, fmt.Errorf("token已过期")
		}
	}

	// 提取claims
	jwtClaims := &model.JWTClaims{}
	
	if userID, ok := claims["user_id"].(string); ok {
		jwtClaims.UserID = userID
	}
	
	if userType, ok := claims["user_type"].(string); ok {
		jwtClaims.UserType = userType
	}
	
	if roomID, ok := claims["room_id"].(string); ok {
		jwtClaims.RoomID = roomID
	}
	
	if iat, ok := claims["iat"].(float64); ok {
		jwtClaims.IssuedAt = int64(iat)
	}
	
	if exp, ok := claims["exp"].(float64); ok {
		jwtClaims.ExpiresAt = int64(exp)
	}

	// 基本验证
	if jwtClaims.UserID == "" {
		return nil, fmt.Errorf("token中缺少user_id")
	}

	return jwtClaims, nil
}

// RefreshToken 刷新token（可选功能）
func (j *JWTService) RefreshToken(tokenString string) (string, error) {
	claims, err := j.ValidateToken(tokenString)
	if err != nil {
		return "", err
	}

	// 生成新token
	return j.GenerateToken(claims.UserID, claims.UserType, claims.RoomID)
} 