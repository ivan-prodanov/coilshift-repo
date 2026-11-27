package authority

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var pk = make([]byte, 65)

func TestCheckRoles(t *testing.T) {
	for r := int32(0); r <= 11; r++ {
		err := Check(context.Background(), Fake{Role: r, Owner: true}, pk)
		if (r == 2 || r == 3) != (err == nil) {
			t.Fatalf("role %d: err=%v", r, err)
		}
	}
}

func TestCheckOwnerFalse(t *testing.T) {
	if Check(context.Background(), Fake{Role: 2, Owner: false}, pk) == nil {
		t.Fatal("spoofed owner accepted")
	}
}

func TestCheckTimeout(t *testing.T) {
	st := time.Now()
	if Check(context.Background(), Fake{Role: 2, Owner: true, Delay: 600 * time.Millisecond}, pk) == nil {
		t.Fatal("slow oracle accepted")
	}
	if time.Since(st) > 580*time.Millisecond {
		t.Fatal("did not time out at 500ms")
	}
}

func TestCheckError(t *testing.T) {
	if Check(context.Background(), Fake{Role: 2, Owner: true, Err: errors.New("boom")}, pk) == nil {
		t.Fatal("error accepted")
	}
}

func TestLookupUID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "passwd")
	os.WriteFile(p, []byte("root:x:0:0::/root:/bin/sh\nauthd:x:403:403:auth:/:/sbin/nologin\n"), 0o644)
	u, err := lookupUID(p, "authd")
	if err != nil || u != 403 {
		t.Fatalf("u=%d err=%v", u, err)
	}
	if _, err := lookupUID(p, "nobody"); err == nil {
		t.Fatal("missing user accepted")
	}
	if _, err := lookupUID(filepath.Join(t.TempDir(), "none"), "authd"); err == nil {
		t.Fatal("missing file accepted")
	}
}
