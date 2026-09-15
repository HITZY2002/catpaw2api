package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/pool"
)

func TestAdminEnablePreservesBalance(t *testing.T) {
	p, err := pool.New([]*auth.Auth{auth.New("u1", "alice", "tok")}, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	p.SetBalance("u1", 321)
	p.Disable("u1", "manual disable")

	h := NewHandler(Config{Pool: p, APIKey: "test-key"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/enable", strings.NewReader(`{"uid":"u1"}`))
	rec := httptest.NewRecorder()
	h.adminEnable(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	list := p.List()
	if len(list) != 1 {
		t.Fatalf("list=%v", list)
	}
	if list[0]["disabled"].(bool) {
		t.Fatalf("account should be enabled: %v", list[0])
	}
	if got := list[0]["balance"].(int64); got != 321 {
		t.Fatalf("balance=%d, want 321", got)
	}
}

func TestAdminEnableMissingAccountReturns404(t *testing.T) {
	p, err := pool.New(nil, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/enable", strings.NewReader(`{"uid":"missing"}`))
	rec := httptest.NewRecorder()
	h.adminEnable(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
