package gotunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnrollCodeStable(t *testing.T) {
	at := time.Unix(1700000100, 0) // exactly on a 300s step edge
	if at.Unix()%300 != 0 {
		t.Fatalf("bad anchor: %d", at.Unix()%300)
	}
	if enrollCode("cxu.lol", 300, at) != enrollCode("cxu.lol", 300, at.Add(299*time.Second)) {
		t.Errorf("same step must give same code")
	}
	if enrollCode("cxu.lol", 300, at) == enrollCode("cxu.lol", 300, at.Add(300*time.Second)) {
		t.Errorf("next step must differ")
	}
	if enrollCode("other", 300, at) == enrollCode("cxu.lol", 300, at) {
		t.Errorf("secret must matter")
	}
	if len(enrollCode("cxu.lol", 300, at)) != 8 {
		t.Errorf("code must be 8 hex chars")
	}
}

func TestEnsureKeyRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tunnel")
	s1, pub1, err := ensureKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if s1 == nil || !strings.HasPrefix(pub1, "ssh-ed25519 ") {
		t.Fatalf("bad keypair: %q", pub1)
	}
	s2, pub2, err := ensureKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if pub1 != pub2 || s2 == nil {
		t.Errorf("reload must return same key")
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("key file missing: %v", err)
	}
}
