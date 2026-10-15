package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTakeAllowsOneHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "repos-api.lock")
	release, err := Take(path)
	if err != nil {
		t.Fatalf("Take() = %v (it creates the directory too)", err)
	}
	if _, err := Take(path); !errors.Is(err, ErrHeld) {
		t.Errorf("a second Take() = %v, want ErrHeld", err)
	}
	release()
	again, err := Take(path)
	if err != nil {
		t.Fatalf("Take() after release = %v", err)
	}
	again()
}

func TestHeldNeitherLocksNorCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if Held(path) {
		t.Error("Held() = true for a lock file that doesn't exist")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("Held() created the lock file")
	}
	release, err := Take(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Held(path) {
		t.Error("Held() = false while held")
	}
	release()
	if Held(path) {
		t.Error("Held() = true after release")
	}
	// Held only reads /proc/locks, so a Take right after it never meets a lock Held took.
	for i := 0; i < 100; i++ {
		Held(path)
		r, err := Take(path)
		if err != nil {
			t.Fatalf("Take() after Held() = %v", err)
		}
		r()
	}
}
