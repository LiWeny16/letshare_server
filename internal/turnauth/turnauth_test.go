package turnauth

import (
	"crypto/md5"
	"fmt"
	"testing"
	"time"
)

func TestBuildUsernameFormat(t *testing.T) {
	exp := time.Unix(1700000000, 0)
	got := BuildUsername(exp)
	want := "1700000000:letshare"
	if got != want {
		t.Fatalf("BuildUsername = %q, want %q", got, want)
	}
}

// RoundTrip：Verify 返回的 key 必须与客户端侧推导（RFC 5389 §15.4 长期凭据）
// MD5(username:realm:credential) 完全一致 —— 这是浏览器能通过 pion 认证的契约。
func TestVerifyRoundTrip(t *testing.T) {
	secret := "s3cret"
	realm := "letshare.fun"
	now := time.Now()
	username := BuildUsername(now.Add(5 * time.Minute))
	credential := Credential(secret, username)

	gotKey, ok := Verify(secret, username, realm, now)
	if !ok {
		t.Fatal("Verify should accept valid username")
	}
	h := md5.New() //nolint:gosec
	fmt.Fprintf(h, "%s:%s:%s", username, realm, credential)
	wantKey := h.Sum(nil)
	if string(gotKey) != string(wantKey) {
		t.Fatalf("Verify key mismatch: got %x, want %x (client-side MD5 derivation)", gotKey, wantKey)
	}
}

func TestVerifyExpired(t *testing.T) {
	secret := "s3cret"
	now := time.Now()
	username := BuildUsername(now.Add(-time.Minute))
	if _, ok := Verify(secret, username, "letshare.fun", now); ok {
		t.Fatal("Verify should reject expired username")
	}
}

func TestVerifyWrongSecretProducesDifferentKey(t *testing.T) {
	username := "1700000000:letshare"
	k1, ok1 := Verify("secret-a", username, "letshare.fun", time.Unix(1699999000, 0))
	k2, ok2 := Verify("secret-b", username, "letshare.fun", time.Unix(1699999000, 0))
	if !ok1 || !ok2 {
		t.Fatal("both should be valid usernames")
	}
	if string(k1) == string(k2) {
		t.Fatal("different secrets must produce different keys")
	}
}

func TestVerifyMalformedUsername(t *testing.T) {
	if _, ok := Verify("s", "nocolon", "letshare.fun", time.Now()); ok {
		t.Fatal("Verify should reject username without colon")
	}
	if _, ok := Verify("s", "notanumber:user", "letshare.fun", time.Now()); ok {
		t.Fatal("Verify should reject non-numeric expiry")
	}
}
