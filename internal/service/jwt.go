package service

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ProClaims JWT claims for PRO identity
type ProClaims struct {
	IsPro bool `json:"is_pro"`
	jwt.RegisteredClaims
}

// JWTService handles PRO token generation and validation
type JWTService struct {
	secret          []byte
	expirationHours int
}

func NewJWTService(secret string, expirationHours int) *JWTService {
	return &JWTService{
		secret:          []byte(secret),
		expirationHours: expirationHours,
	}
}

// GenerateProToken issues a JWT for a user who has activated PRO with a valid invite code.
func (j *JWTService) GenerateProToken(userID string) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(time.Duration(j.expirationHours) * time.Hour)

	claims := ProClaims{
		IsPro: true,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString(j.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to sign JWT: %w", err)
	}

	return tokenString, expiresAt, nil
}

// ValidateToken parses and validates a PRO JWT, returning the claims on success.
func (j *JWTService) ValidateToken(tokenString string) (*ProClaims, error) {
	if tokenString == "" {
		return nil, fmt.Errorf("token is empty")
	}

	token, err := jwt.ParseWithClaims(tokenString, &ProClaims{}, func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != "HS256" {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return j.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(*ProClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	if !claims.IsPro {
		return nil, fmt.Errorf("token does not grant PRO status")
	}

	return claims, nil
}
