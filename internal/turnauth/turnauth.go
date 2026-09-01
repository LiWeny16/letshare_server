// Package turnauth 提供 TURN 短效凭据的构造与校验（RFC 5766 use-auth-secret 模式）。
//
// handler（/api/turn-credentials 签发）与 turnserver（嵌入式 TURN 服务 AuthHandler）
// 共用本包，保证"签发的凭据一定能通过中继认证"这一单一事实源。
package turnauth

import (
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // RFC 5389 长期凭据 key 推导规定 MD5
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// UsernameSuffix 短效凭据 username 的用户标识部分（"<unix过期>:<suffix>"）。
const UsernameSuffix = "letshare"

// BuildUsername 构造短效凭据 username："<unix过期时间戳>:<suffix>"。
func BuildUsername(expiresAt time.Time) string {
	return strconv.FormatInt(expiresAt.Unix(), 10) + ":" + UsernameSuffix
}

// Credential 计算 Base64(HMAC-SHA1(secret, username))。
func Credential(secret, username string) string {
	return base64.StdEncoding.EncodeToString(hmacSHA1(secret, username))
}

// Verify 校验 username 并返回 TURN 长期凭据的 MESSAGE-INTEGRITY key：
//   - username 须形如 "<unix过期>:<suffix>" 且未过期
//   - 返回 MD5(username:realm:credential) 原始字节（RFC 5389 §15.4 长期凭据 key）
//
// 协议两侧对齐方式：客户端（Chrome/pion）用签发下发的 credential 字符串算
// MD5(username:realm:password) 作为 HMAC key；pion 服务端 AuthHandler 的返回值
// 就是这个 key（不做二次推导，见 pion internal/server/util.go authenticateRequest
// 对 stun.MessageIntegrity(ourKey).Check 的用法）。攻击者无 secret 无法构造
// 正确 credential，因此返回 key 本身安全。
func Verify(secret, username, realm string, now time.Time) ([]byte, bool) {
	parts := strings.SplitN(username, ":", 2)
	if len(parts) != 2 {
		return nil, false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || now.Unix() > expiry {
		return nil, false
	}
	h := md5.New() //nolint:gosec // RFC 5389 规定的长期凭据 key 推导
	fmt.Fprintf(h, "%s:%s:%s", username, realm, Credential(secret, username))
	return h.Sum(nil), true
}

func hmacSHA1(secret, username string) []byte {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	return mac.Sum(nil)
}
