package gotunnel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Golden vector generated with system openssl (test-only key, never used):
// openssl enc -aes-256-cbc -pbkdf2 -pass pass:abcdef12 -in golden.pub
const goldenPub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBxMMlPlzkDFTRdMF6oiQAczfjZv9/9ShJZBAqmB4y9Z root@c677ebcbf8f8"
const goldenCode = "abcdef12"
const goldenEnc = "U2FsdGVkX1/1jWO79/DFe1EQTu92Cn5+Allrao5/g5YGPiu8+614FeD3DZtmLRa0NpSLDG8xLobUiuZLTrYH06rqrdPOef2mMraxyPoqi0Ey3COIImhZO7eW4+BbgPR2OZz6vH9yJ/hP55UgKny3gh+ueVgUxjqE34D6IUzUXOk="

func TestDecryptGoldenOpenssl(t *testing.T) {
	got, err := decryptEnrollPubkey(goldenEnc, goldenCode)
	if err != nil {
		t.Fatalf("openssl vector must decrypt: %v", err)
	}
	if got != goldenPub {
		t.Fatalf("got %q", got)
	}
	if _, err := decryptEnrollPubkey(goldenEnc, "wrongcode"); err == nil {
		t.Fatalf("wrong code must fail")
	}
}

func TestVerifyEnrollCodeSkew(t *testing.T) {
	at := time.Unix(1_700_000_100, 0)
	good := enrollCodeFor("s3cr3t", 300, at)
	if !verifyEnrollCode("s3cr3t", 300, at, good) {
		t.Errorf("current step must verify")
	}
	if !verifyEnrollCode("s3cr3t", 300, at.Add(299*time.Second), good) {
		t.Errorf("same step must verify")
	}
	if !verifyEnrollCode("s3cr3t", 300, at.Add(-300*time.Second), good) {
		t.Errorf("-1 skew must verify")
	}
	if verifyEnrollCode("s3cr3t", 300, at.Add(600*time.Second), good) {
		t.Errorf("+2 skew must fail")
	}
	if verifyEnrollCode("s3cr3t", 300, at, "deadbeef") {
		t.Errorf("bogus must fail")
	}
}

func enrollTestServer(t *testing.T) *Server {
	t.Helper()
	opt := DefaultOptions()
	opt.DBFile = t.TempDir() + "/routes.json"
	opt.AuthKeysFile = filepath.Join(t.TempDir(), "authorized_keys")
	opt.SSHBasePort = 15000
	opt.SSHMaxSlots = 5
	opt.EnrollSecret = "testsecret"
	return NewServer(opt)
}

func enrollPost(t *testing.T, srv *Server, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/__enroll", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	var doc map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &doc)
	return w.Code, doc
}

func TestEnrollFlow(t *testing.T) {
	srv := enrollTestServer(t)
	code := enrollCodeFor("testsecret", 300, time.Now())
	payload := `{"name":"origtest","code":"` + code + `","pubkey_enc":"` + goldenEncFixedCode(t, code) + `"}`
	status, doc := enrollPost(t, srv, payload)
	if status != 200 {
		t.Fatalf("enroll status=%d doc=%v", status, doc)
	}
	if doc["name"] != "origtest" || doc["via"] != "ssh" {
		t.Fatalf("doc=%v", doc)
	}
	if _, ok := doc["port"].(float64); !ok {
		t.Fatalf("no port in %v", doc)
	}
	if !strings.Contains(doc["publicUrl"].(string), "/origtest/") || !strings.Contains(doc["sshCmd"].(string), "ssh -NTR") {
		t.Fatalf("doc=%v", doc)
	}
	// authorized_keys got the marker line (no secret material asserted).
	raw, _ := os.ReadFile(srv.opt.AuthKeysFile)
	if !strings.Contains(string(raw), "# frp-auto origtest ") {
		t.Fatalf("authorized_keys missing marker:\n%s", raw)
	}
	// bad code -> 403, bad name -> 400.
	if st, _ := enrollPost(t, srv, `{"name":"x","code":"nope","pubkey_enc":"`+goldenEncFixedCode(t, code)+`"}`); st != 403 {
		t.Fatalf("bad code status=%d", st)
	}
	if st, _ := enrollPost(t, srv, `{"name":"BAD NAME","code":"`+code+`"}`); st != 400 {
		t.Fatalf("bad name status=%d", st)
	}
}

// goldenEncFixedCode seals the golden pubkey for code (round-trip asserts
// our encryptor/decryptor agree; openssl interop is proven separately by
// TestDecryptGoldenOpenssl against a real openssl vector).
func goldenEncFixedCode(t *testing.T, code string) string {
	t.Helper()
	enc, err := encryptPubkey(goldenPub, code)
	if err != nil {
		t.Fatal(err)
	}
	// round-trip through our own decryptor (openssl interop proven separately
	// by TestDecryptGoldenOpenssl against a real openssl vector).
	if back, err := decryptEnrollPubkey(enc, code); err != nil || back != goldenPub {
		t.Fatalf("roundtrip: %q %v", back, err)
	}
	return enc
}

func TestAdminAllocRelease(t *testing.T) {
	srv := enrollTestServer(t)
	srv.opt.AdminToken = "adm123"
	call := func(path, token, body string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("x-admin-token", token)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		return w.Code
	}
	if st := call("/__admin/alloc", "", `{"name":"a1"}`); st != 403 {
		t.Fatalf("no token must 403, got %d", st)
	}
	if st := call("/__admin/alloc", "adm123", `{"name":"a1"}`); st != 200 {
		t.Fatalf("alloc status=%d", st)
	}
	if st := call("/__admin/release", "adm123", `{"name":"a1"}`); st != 200 {
		t.Fatalf("release status=%d", st)
	}
	srv.mu.Lock()
	_, left := srv.routes["a1"]
	srv.mu.Unlock()
	if left {
		t.Errorf("route must be gone after release")
	}
}
