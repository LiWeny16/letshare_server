package handler

import "testing"

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
func TestTurnHMACSHA1Deterministic(t *testing.T) {
	secret := "s3cret"
	username := "1788090400:600"
	got1 := turnHMACSHA1(secret, username)
	got2 := turnHMACSHA1(secret, username)
	if got1 != got2 {
		t.Fatalf("相同输入应得到相同 HMAC，got %q vs %q", got1, got2)
	}
	if got1 == "" {
		t.Fatal("HMAC 不应为空")
	}
	// 不同 secret 应得到不同凭证
	if turnHMACSHA1("other-secret", username) == got1 {
		t.Fatal("不同 secret 应产生不同凭证")
	}
}