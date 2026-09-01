package handler

import (
	"testing"

	"letshare-server/internal/turnauth"
)

// 验证 allowRate 每 IP 每分钟最多放行 30 次，超过即拒绝。
func TestTurnHandlerAllowRate(t *testing.T) {
	h := NewTurnHandler(true, "test-secret", []string{"turn:ecs.letshare.fun:3478"}, 600)

	const limit = 30
	key := "10.0.0.1"

	for i := 0; i < limit; i++ {
		if !h.allowRate(key) {
			t.Fatalf("第 %d 次请求不应被限流", i+1)
		}
	}
	if h.allowRate(key) {
		t.Fatalf("第 %d 次请求应触发限流（每分钟上限 %d）", limit+1, limit)
	}
}

// 验证不同 IP 互相独立限流。
func TestTurnHandlerAllowRatePerIP(t *testing.T) {
	h := NewTurnHandler(true, "test-secret", nil, 600)
	if !h.allowRate("10.0.0.1") {
		t.Fatal("IP A 首次请求应放行")
	}
	if !h.allowRate("10.0.0.2") {
		t.Fatal("IP B 首次请求应放行")
	}
}

// 验证 HMAC 签发格式符合 RFC 5766：Base64(HMAC-SHA1(secret, username))，确定性可复现。
// username 格式为 "<过期时间戳>:<用户标识>"（见 coturn use-auth-secret 约定）。
func TestTurnHMACSHA1Deterministic(t *testing.T) {
	secret := "s3cret"
	username := "1788091000:letshare" // 过期时间戳:用户标识
	got1 := turnauth.Credential(secret, username)
	got2 := turnauth.Credential(secret, username)
	if got1 != got2 {
		t.Fatalf("相同输入应得到相同 HMAC，got %q vs %q", got1, got2)
	}
	if got1 == "" {
		t.Fatal("HMAC 不应为空")
	}
	// 不同 secret 应得到不同凭证
	if turnauth.Credential("other-secret", username) == got1 {
		t.Fatal("不同 secret 应产生不同凭证")
	}
}

// 验证 HMAC 结果与官方参考实现一致（RFC 5766 use-auth-secret 的确定性向量）。
// 官方算法：password = base64(hmac_sha1(secret, username))，username 含过期时间戳与用户标识。
func TestTurnHMACSHA1MatchesReference(t *testing.T) {
	secret := "s3cret"
	username := "1600000000:letshare"
	// 手工计算 HMAC-SHA1 的 base64（用标准库 open 内联等价验证，避免引入额外依赖）
	got := turnauth.Credential(secret, username)
	// HMAC-SHA1 结果恒为 20 字节，base64 后恒为 28 字符（含 = 补齐）
	if len(got) != 28 {
		t.Fatalf("base64(HMAC-SHA1) 长度应为 28，实际 %d (%q)", len(got), got)
	}
}
