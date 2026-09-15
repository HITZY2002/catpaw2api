package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"catpaw2api/internal/auth"
)

func TestConcurrentStatePersistenceRemainsAtomic(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "nested", "state.json")
	p, err := New([]*auth.Auth{auth.New("u1", "alice", "tok")}, Config{}, state)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	const iterations = 40
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				p.SetBalance("u1", int64(w*iterations+i))
				if i%3 == 0 {
					p.Cooldown("u1", CoolSoft, time.Millisecond, "test")
				} else {
					p.ClearCooldown("u1")
				}
			}
		}()
	}
	wg.Wait()

	// 最终确定性写入，用于确认最后一次完整快照确实落盘。
	p.ClearCooldown("u1")
	p.SetBalance("u1", 777)

	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var entries []stateEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("state is not valid JSON: %v\n%s", err, raw)
	}
	if len(entries) != 1 || entries[0].Name != "u1" || entries[0].Balance != 777 {
		t.Fatalf("entries=%+v", entries)
	}
	if _, err := os.Stat(state + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary state file should not remain, err=%v", err)
	}
}
