package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"catpaw2api/internal/auth"
)

func TestAddAccountKeepsPublishedAuthPointerStable(t *testing.T) {
	p, err := New([]*auth.Auth{auth.New("u1", "alice", "tok-0")}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	acct := p.Get("u1")
	if acct == nil || acct.Auth == nil {
		t.Fatal("account/auth missing")
	}
	published := acct.Auth

	updated := p.AddAccount(auth.New("u1", "alice-new", "tok-1"))
	if updated != acct {
		t.Fatal("same UID should keep Account object stable")
	}
	if acct.Auth != published {
		t.Fatal("published Auth pointer must remain stable")
	}
	if got := acct.Auth.Token(); got != "tok-1" {
		t.Fatalf("token=%q, want tok-1", got)
	}
	if got := acct.Token(); got != "tok-1" {
		t.Fatalf("account token=%q, want tok-1", got)
	}
}

func TestCredentialHotReplacementConcurrentReads(t *testing.T) {
	p, err := New([]*auth.Auth{auth.New("u1", "alice", "tok-0")}, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	acct := p.Get("u1")
	if acct == nil || acct.Auth == nil {
		t.Fatal("account/auth missing")
	}
	published := acct.Auth

	const iterations = 500
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 1; i <= iterations; i++ {
			if got := p.AddAccount(auth.New("u1", "alice", fmt.Sprintf("tok-%d", i))); got == nil {
				t.Errorf("AddAccount returned nil at iteration %d", i)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations*3; i++ {
			// Deliberately cover both the legacy direct path and the preferred
			// Account accessor. The direct pointer read is safe because the pointer
			// is an invariant after publication; token contents are mutex-protected.
			_ = acct.Auth.Token()
			_ = acct.Token()
			_ = acct.TokenRemaining()
			_ = acct.TokenAge()
			_ = acct.TokenExpired()
			_ = acct.TokenExpiringSoon(24 * time.Hour)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			p.SyncToDir([]*auth.Auth{auth.New("u1", "alice", fmt.Sprintf("reload-%d", i))})
		}
	}()

	wg.Wait()
	if acct.Auth != published {
		t.Fatal("hot replacement changed published Auth pointer")
	}
	if acct.Token() == "" {
		t.Fatal("token became empty after concurrent hot replacement")
	}
}
