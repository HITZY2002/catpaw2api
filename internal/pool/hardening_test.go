package pool

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"catpaw2api/internal/auth"
	"catpaw2api/internal/upstream"
)

func TestValidateTransientFailureDoesNotDisableAccount(t *testing.T) {
	oldGateway := upstream.GatewayHost
	defer func() { upstream.GatewayHost = oldGateway }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("temporary upstream failure"))
	}))
	defer srv.Close()
	upstream.GatewayHost = srv.URL

	p := newTestPool(t, "a")
	acct := p.Get("a")
	ok, err := p.Validate(acct)
	if err == nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if p.IsDisabled("a") {
		t.Fatal("transient 5xx must not permanently disable account")
	}
	if !p.Healthy("a") {
		t.Fatal("account must remain eligible after validation transport failure")
	}
}

func TestValidateUnauthorizedDisablesAndPersists(t *testing.T) {
	oldGateway := upstream.GatewayHost
	defer func() { upstream.GatewayHost = oldGateway }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	upstream.GatewayHost = srv.URL

	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	a := &auth.Auth{UID: "a", AccessToken: "tok-a"}
	p, err := New([]*auth.Auth{a}, Config{}, state)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := p.Validate(p.Get("a"))
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !p.IsDisabled("a") {
		t.Fatal("401 must disable account")
	}

	p2, err := New([]*auth.Auth{{UID: "a", AccessToken: "tok-a"}}, Config{}, state)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.IsDisabled("a") {
		t.Fatal("disabled state was not persisted")
	}
}

func TestSetBalancePersistsImmediately(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	auths := []*auth.Auth{{UID: "a", AccessToken: "tok-a"}}
	p, err := New(auths, Config{}, state)
	if err != nil {
		t.Fatal(err)
	}
	p.SetBalance("a", 456)

	p2, err := New([]*auth.Auth{{UID: "a", AccessToken: "tok-a"}}, Config{}, state)
	if err != nil {
		t.Fatal(err)
	}
	got := p2.List()[0]["balance"].(int64)
	if got != 456 {
		t.Fatalf("balance=%d want=456", got)
	}
}

func TestAddAccountReplacesTokenWithoutCorruptingBalance(t *testing.T) {
	p := newTestPool(t, "a")
	p.SetBalance("a", 789)
	p.Cooldown("a", CoolErr, time.Hour, "temporary")
	p.Disable("a", "old token invalid")

	newAuth := auth.New("a", "fresh-user", "tok-new")
	got := p.AddAccount(newAuth)
	if got == nil {
		t.Fatal("updated account is nil")
	}
	if got.Auth.Token() != "tok-new" {
		t.Fatalf("token=%q", got.Auth.Token())
	}
	if !p.Healthy("a") {
		t.Fatal("fresh login should clear stale disabled/cooldown state")
	}
	list := p.List()
	if list[0]["balance"].(int64) != 789 {
		t.Fatalf("balance=%v", list[0]["balance"])
	}
	if list[0]["err_count"].(int) != 0 {
		t.Fatalf("err_count=%v", list[0]["err_count"])
	}
}

func TestAccountsReturnsSliceSnapshot(t *testing.T) {
	p := newTestPool(t, "a", "b")
	accounts := p.Accounts()
	accounts[0] = nil
	fresh := p.Accounts()
	if fresh[0] == nil {
		t.Fatal("mutating returned slice must not mutate pool membership")
	}
}
