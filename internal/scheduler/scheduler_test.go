package scheduler

import (
	"testing"
	"time"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/pool"
	"catpaw2api/internal/upstream"
)

func TestValidateRenewedIdentity(t *testing.T) {
	acct := &pool.Account{Name: "u1", UID: "u1", UserName: "old"}
	if err := validateRenewedIdentity(acct, &upstream.UserInfo{UserID: "u1", UserName: "new"}); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if err := validateRenewedIdentity(acct, &upstream.UserInfo{UserID: "u2"}); err == nil {
		t.Fatal("mismatched identity must be rejected")
	}
	if err := validateRenewedIdentity(acct, &upstream.UserInfo{}); err == nil {
		t.Fatal("missing identity must be rejected")
	}
	if err := validateRenewedIdentity(nil, &upstream.UserInfo{UserID: "u1"}); err == nil {
		t.Fatal("nil account must be rejected")
	}
}

func TestInstallRenewedAuthPreservesBalanceAndClearsCooldown(t *testing.T) {
	oldAuth := auth.New("u1", "old-name", "tok-old")
	p, err := pool.New([]*auth.Auth{oldAuth}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.SetBalance("u1", 123)
	p.Cooldown("u1", pool.CoolPlan, time.Hour, "balance exhausted")
	if p.Healthy("u1") {
		t.Fatal("fixture must start cooling")
	}

	s := New(Config{Pool: p})
	newAuth := auth.New("u1", "new-name", "tok-new")
	updated := s.installRenewedAuth(newAuth)
	if updated == nil {
		t.Fatal("renewed account not installed")
	}
	if got := p.Get("u1").Auth.Token(); got != "tok-new" {
		t.Fatalf("token=%q", got)
	}
	if !p.Healthy("u1") {
		t.Fatal("renewal should clear stale cooldown/disabled state")
	}
	list := p.List()
	if len(list) != 1 {
		t.Fatalf("list=%v", list)
	}
	if got := list[0]["balance"].(int64); got != 123 {
		t.Fatalf("balance changed during renewal: got=%d want=123", got)
	}
	if got := list[0]["nickname"].(string); got != "new-name" {
		t.Fatalf("nickname=%q", got)
	}
}
