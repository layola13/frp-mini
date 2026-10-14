package gotunnel

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// This file ports the TS server's self-service SSH onboarding
// (POST /__enroll, /__admin/*) so the ORIGINAL openssh get.sh flow works
// unchanged against the Go server: same JSON field names, same plain-text
// error messages, same openssl-compatible pubkey encryption.

var pubkeyRE = regexp.MustCompile(`^(ssh-(ed25519|rsa|ecdsa|dss)|ecdsa-sha2-nistp\d+)\s+[A-Za-z0-9+/=]+(\s+\S+)?$`)

// enrollStepOf mirrors TS stepOf: floor(unix_ms / (stepSec*1000)).
func enrollStepOf(stepSec int64, now time.Time) int64 {
	return now.UnixMilli() / (stepSec * 1000)
}

// enrollCodeFor mirrors TS enrollCode: sha256("secret:step")[0:8].
func enrollCodeFor(secret string, stepSec int64, now time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", secret, enrollStepOf(stepSec, now))))
	return hex.EncodeToString(sum[:])[:8]
}

// verifyEnrollCode accepts ±1 step of clock skew, like the TS server.
func verifyEnrollCode(secret string, stepSec int64, now time.Time, code string) bool {
	c := strings.ToLower(strings.TrimSpace(code))
	base := enrollStepOf(stepSec, now)
	for _, s := range []int64{base - 1, base, base + 1} {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", secret, s)))
		if hex.EncodeToString(sum[:])[:8] == c {
			return true
		}
	}
	return false
}

// normalizeEnrollEnc mirrors the TS double-base64 tolerance: the payload is
// base64("Salted__"+salt8+ct), but some shells wrap it in base64 twice.
func normalizeEnrollEnc(enc string) ([]byte, error) {
	enc = strings.TrimSpace(enc)
	if enc == "" {
		return nil, fmt.Errorf("empty")
	}
	if !strings.HasPrefix(enc, "U2FsdGVk") {
		if once, err := base64.StdEncoding.DecodeString(enc); err == nil {
			if twice, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(once))); err == nil {
				if len(twice) > 8 && string(twice[:8]) == "Salted__" {
					return twice, nil
				}
			}
		}
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("bad base64: %w", err)
	}
	return raw, nil
}

func unpadPKCS7(b []byte) ([]byte, error) {
	if len(b) == 0 || len(b)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("bad block size")
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(b) {
		return nil, fmt.Errorf("bad padding")
	}
	for _, v := range b[len(b)-pad:] {
		if int(v) != pad {
			return nil, fmt.Errorf("bad padding")
		}
	}
	return b[:len(b)-pad], nil
}

// decryptEnrollPubkey reverses `openssl enc -aes-256-cbc -pbkdf2
// -pass pass:$CODE`: tries PBKDF2-SHA256(10k) first, then legacy single-round
// EVP_BytesToKey, exactly like the TS server.
func decryptEnrollPubkey(encB64, code string) (string, error) {
	raw, err := normalizeEnrollEnc(encB64)
	if err != nil {
		return "", fmt.Errorf("decrypt fail: %w", err)
	}
	if len(raw) <= 16 || string(raw[:8]) != "Salted__" {
		return "", fmt.Errorf("decrypt fail: bad enc format")
	}
	salt, ct := raw[8:16], raw[16:]
	try := []func() (string, error){
		func() (string, error) {
			dk := pbkdf2.Key([]byte(code), salt, 10_000, 48, sha256.New)
			block, err := aes.NewCipher(dk[:32])
			if err != nil {
				return "", err
			}
			pt := make([]byte, len(ct))
			cipher.NewCBCDecrypter(block, dk[32:48]).CryptBlocks(pt, ct)
			pt, err = unpadPKCS7(pt)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(pt)), nil
		},
		func() (string, error) {
			var out, prev []byte
			for len(out) < 48 {
				h := sha256.New()
				h.Write(prev)
				h.Write([]byte(code))
				h.Write(salt)
				prev = h.Sum(nil)
				out = append(out, prev...)
			}
			block, err := aes.NewCipher(out[:32])
			if err != nil {
				return "", err
			}
			pt := make([]byte, len(ct))
			cipher.NewCBCDecrypter(block, out[32:48]).CryptBlocks(pt, ct)
			pt, err = unpadPKCS7(pt)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(pt)), nil
		},
	}
	var last error = fmt.Errorf("decrypt fail")
	for _, fn := range try {
		s, err := fn()
		if err != nil {
			last = err
			continue
		}
		if pubkeyRE.MatchString(s) {
			return s, nil
		}
		last = fmt.Errorf("decrypted but not a pubkey")
	}
	return "", last
}

// bindPubkey appends the key to authorized_keys with the same marker format.
func bindPubkey(authKeysFile, name, pubkey string) error {
	dir := filepath.Dir(authKeysFile)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	cur, _ := os.ReadFile(authKeysFile)
	fpsum := sha256.Sum256([]byte(strings.TrimSpace(pubkey)))
	fp := hex.EncodeToString(fpsum[:])[:16]
	if bytes.Contains(cur, []byte(fp)) {
		return nil // already bound
	}
	line := fmt.Sprintf("restrict,port-forwarding,command=\"/bin/false\" %s # frp-auto %s %s\n",
		strings.TrimSpace(pubkey), name, fp)
	f, err := os.OpenFile(authKeysFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// randHex returns n random bytes as hex (name suffixes).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "deadbeef"[:2*n]
	}
	return hex.EncodeToString(b)
}

// enrollFields shared by /__enroll and /__admin/alloc responses.
func (s *Server) enrollJSON(name string, r *tcpRoute) map[string]any {
	sshHost := firstNonEmpty(os.Getenv("SSH_HOST"), s.opt.SSHHost, s.opt.PublicHost)
	sshUser := firstNonEmpty(os.Getenv("SSH_USER"), s.opt.SSHUser, "tunnel")
	return map[string]any{
		"ok": true, "name": name, "port": r.Port,
		"createdAt": r.CreatedAt, "expiresAt": r.ExpiresAt, "via": r.Via,
		"publicUrl": fmt.Sprintf("https://%s/%s/", s.opt.PublicHost, name),
		"sshCmd":    fmt.Sprintf("ssh -NTR 127.0.0.1:%d:127.0.0.1:43210 %s@%s", r.Port, sshUser, sshHost),
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// handleEnroll implements POST /__enroll (original get.sh flow).
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name      string `json:"name"`
		Code      string `json:"code"`
		PubkeyEnc string `json:"pubkey_enc"`
		Pubkey    string `json:"pubkey"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&b)
	name := strings.ToLower(strings.TrimSpace(b.Name))
	if name == "" {
		name = fmt.Sprintf("node-%s", randHex(4))
	}
	if !ValidName(name) {
		http.Error(w, "bad name [a-z0-9-]", http.StatusBadRequest)
		return
	}
	if s.isReserved(name) {
		http.Error(w, "name reserved (get/join/静态名不可注册)", http.StatusConflict)
		return
	}
	if !verifyEnrollCode(s.opt.EnrollSecret, s.opt.EnrollStepSec, time.Now(), b.Code) {
		http.Error(w, "bad code (时钟差>5分钟? 按规则重算)", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	_, takenByAgent := s.agents[name]
	s.mu.Unlock()
	if takenByAgent {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "name taken by ws agent"})
		return
	}
	pubkey := strings.TrimSpace(b.Pubkey)
	if pubkey == "" && strings.TrimSpace(b.PubkeyEnc) != "" {
		var err error
		pubkey, err = decryptEnrollPubkey(b.PubkeyEnc, strings.ToLower(strings.TrimSpace(b.Code)))
		if err != nil {
			http.Error(w, fmt.Sprintf("decrypt pubkey fail: %v", err), http.StatusBadRequest)
			return
		}
	}
	pubkey = strings.TrimSpace(pubkey)
	if !pubkeyRE.MatchString(pubkey) {
		http.Error(w, "bad pubkey (要单行 openssh 公钥)", http.StatusBadRequest)
		return
	}
	fpsum := sha256.Sum256([]byte(pubkey))
	fp := hex.EncodeToString(fpsum[:])[:16]

	s.mu.Lock()
	assigned := name
	existing, ok := s.routes[assigned]
	// Clash protection: same name + different key => suffixed name, never overwrite.
	if ok && existing.ExpiresAt > time.Now().UnixMilli() && existing.PubkeyFp != "" && existing.PubkeyFp != fp {
		for i := 0; i < 50; i++ {
			candidate := fmt.Sprintf("%s-%s", name, randHex(3))
			if cur, taken := s.routes[candidate]; !taken || cur.ExpiresAt <= time.Now().UnixMilli() {
				assigned = candidate
				ok = false
				break
			}
		}
	}
	s.mu.Unlock()

	if err := bindPubkey(s.opt.AuthKeysFile, assigned, pubkey); err != nil {
		http.Error(w, fmt.Sprintf("bind pubkey fail: %v", err), http.StatusInternalServerError)
		return
	}
	now := time.Now().UnixMilli()
	s.mu.Lock()
	rt, ok := s.routes[assigned]
	if !ok || rt.ExpiresAt <= now {
		port, hasPort := s.allocPortLocked()
		if !hasPort {
			s.mu.Unlock()
			http.Error(w, "no free ports", http.StatusServiceUnavailable)
			return
		}
		rt = &tcpRoute{Name: assigned, Port: port, PubkeyFp: fp, CreatedAt: now, ExpiresAt: now + s.opt.TTL.Milliseconds(), Via: "ssh"}
		s.routes[assigned] = rt
	} else {
		rt.PubkeyFp = fp
		rt.ExpiresAt = now + s.opt.TTL.Milliseconds()
	}
	s.mu.Unlock()
	s.saveDB()
	log.Printf("[enroll] %s -> port %d (pubkey auto-bound)", assigned, rt.Port)
	writeJSON(w, 200, s.enrollJSON(assigned, rt))
}

// allocPortLocked allocates under the caller's lock.
func (s *Server) allocPortLocked() (int, bool) {
	used := map[int]bool{}
	for _, r := range s.routes {
		used[r.Port] = true
	}
	for i := 0; i < s.opt.SSHMaxSlots; i++ {
		if p := s.opt.SSHBasePort + i; !used[p] {
			return p, true
		}
	}
	return 0, false
}

// handleAdminAlloc implements POST /__admin/alloc.
func (s *Server) handleAdminAlloc(w http.ResponseWriter, r *http.Request) {
	if s.opt.AdminToken != "" && r.Header.Get("x-admin-token") != s.opt.AdminToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&b)
	name := strings.ToLower(strings.TrimSpace(b.Name))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("name")))
	}
	if !ValidName(name) {
		http.Error(w, "bad name [a-z0-9-]", http.StatusBadRequest)
		return
	}
	if s.isReserved(name) {
		http.Error(w, "name reserved (get/join/静态名不可注册)", http.StatusConflict)
		return
	}
	s.mu.Lock()
	if _, taken := s.agents[name]; taken {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "name taken by ws agent"})
		return
	}
	now := time.Now().UnixMilli()
	rt, ok := s.routes[name]
	if !ok || rt.ExpiresAt <= now {
		port, hasPort := s.allocPortLocked()
		if !hasPort {
			s.mu.Unlock()
			http.Error(w, "no free ports", http.StatusServiceUnavailable)
			return
		}
		rt = &tcpRoute{Name: name, Port: port, CreatedAt: now, ExpiresAt: now + s.opt.TTL.Milliseconds(), Via: "ssh"}
		s.routes[name] = rt
	} else {
		rt.ExpiresAt = now + s.opt.TTL.Milliseconds()
	}
	s.mu.Unlock()
	s.saveDB()
	writeJSON(w, 200, s.enrollJSON(name, rt))
}

// handleAdminRelease implements POST /__admin/release.
func (s *Server) handleAdminRelease(w http.ResponseWriter, r *http.Request) {
	if s.opt.AdminToken != "" && r.Header.Get("x-admin-token") != s.opt.AdminToken {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var b struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&b)
	s.mu.Lock()
	delete(s.routes, strings.ToLower(strings.TrimSpace(b.Name)))
	s.mu.Unlock()
	s.saveDB()
	writeJSON(w, 200, map[string]any{"ok": true})
}
