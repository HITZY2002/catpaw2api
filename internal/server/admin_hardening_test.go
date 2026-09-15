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

func TestAdminExplicitMissingAccountReturns404(t *testing.T) {
	p, err := pool.New(nil, pool.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Pool: p, APIKey: "test-key"})

	cases := []struct {
		name string
		path string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{name: "enable", path: "/admin/api/accounts/enable", fn: h.adminEnable},
		{name: "disable", path: "/admin/api/accounts/disable", fn: h.adminDisable},
		{name: "clear cooldown", path: "/admin/api/accounts/clear-cooldown", fn: h.adminClearCooldown},
		{name: "unfreeze", path: "/admin/api/accounts/unfreeze", fn: h.adminUnfreeze},
		{name: "credits", path: "/admin/api/credits", fn: h.adminCredits},
		{name: "keepalive", path: "/admin/api/keepalive", fn: h.adminKeepalive},
		{name: "apply", path: "/admin/api/checkin", fn: h.adminApply},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"uid":"missing"}`))
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
