package gotunnel

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/ssh"
)

// SSHOptions configures a reverse-forward client (Go replacement for
// `ssh -NTR 127.0.0.1:<port>:127.0.0.1:<local>`): no openssh needed.
type SSHOptions struct {
	Server   string // https://host for enroll + ssh host
	User     string
	Local    int    // local upstream port (maps remote 127.0.0.1:<port>)
	KeyFile  string // ed25519 key (generated if missing, openssh-compatible)
	Name     string // requested name ("" = server-derived)
	Secret   string // enroll secret (default cxu.lol)
	StepSec  int64  // enroll time step (default 300)
	Upstream string // local upstream base (default http://127.0.0.1:<Local>)
}

// SSHClient runs the enroll + reverse-forward loop.
type SSHClient struct {
	opt SSHOptions
}

// NewSSHClient builds one.
func NewSSHClient(opt SSHOptions) *SSHClient {
	if opt.User == "" {
		opt.User = "tunnel"
	}
	if opt.Secret == "" {
		opt.Secret = "cxu.lol"
	}
	if opt.StepSec <= 0 {
		opt.StepSec = 300
	}
	if opt.Upstream == "" {
		opt.Upstream = fmt.Sprintf("http://127.0.0.1:%d", opt.Local)
	}
	return &SSHClient{opt: opt}
}

// enrollCode mirrors get.sh: sha256("secret:floor(now/step)")[0:8].
func enrollCode(secret string, stepSec int64, now time.Time) string {
	step := now.Unix() / stepSec
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", secret, step)))
	return hex.EncodeToString(sum[:])[:8]
}

// ensureKey loads or generates an ed25519 keypair (openssh file formats).
func ensureKey(path string) (ssh.Signer, string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, "", fmt.Errorf("parse key: %w", err)
		}
		return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	pk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, "", err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(pk), 0o600); err != nil {
		return nil, "", err
	}
	authorized, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	return signer, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(authorized))), nil
}

// encryptPubkey replicates `openssl enc -aes-256-cbc -pbkdf2
// -pass pass:$CODE`: base64("Salted__"+salt8+ct), key/iv from
// PBKDF2-SHA256(pass, salt, 10k, 48).
func encryptPubkey(pubkey, code string) (string, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2.Key([]byte(code), salt, 10_000, 48, sha256.New)
	block, err := aes.NewCipher(dk[:32])
	if err != nil {
		return "", err
	}
	plain := []byte(pubkey)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, dk[32:48]).CryptBlocks(ct, plain)
	out := append([]byte("Salted__"), salt...)
	out = append(out, ct...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Enroll registers name+pubkey and returns the assigned remote port.
func (c *SSHClient) Enroll(signerPub string) (remotePort int, assignedName, publicURL string, err error) {
	server := c.opt.Server
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	code := enrollCode(c.opt.Secret, c.opt.StepSec, time.Now())
	enc, err := encryptPubkey(signerPub, code)
	if err != nil {
		return 0, "", "", err
	}
	payload, _ := json.Marshal(map[string]string{
		"name": c.opt.Name, "code": code, "pubkey_enc": enc,
	})
	resp, err := http.Post(server+"/__enroll", "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode/100 != 2 {
		return 0, "", "", fmt.Errorf("enroll %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var doc struct {
		Port      int    `json:"port"`
		Name      string `json:"name"`
		PublicURL string `json:"publicUrl"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, "", "", fmt.Errorf("enroll decode: %w", err)
	}
	if doc.Port <= 0 {
		return 0, "", "", fmt.Errorf("enroll gave no port: %s", raw)
	}
	return doc.Port, doc.Name, doc.PublicURL, nil
}

// Run enrolls (if needed) and holds the reverse forward with reconnects.
func (c *SSHClient) Run() {
	backoff := time.Second
	for {
		if err := c.serveOnce(); err != nil {
			log.Printf("[ssh] error: %v", err)
		}
		log.Printf("[ssh] retry in %s", backoff)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (c *SSHClient) serveOnce() error {
	signer, pub, err := ensureKey(c.opt.KeyFile)
	if err != nil {
		return err
	}
	port, name, publicURL, err := c.Enroll(pub)
	if err != nil {
		return err
	}
	log.Printf("[ssh] %s -> remote 127.0.0.1:%d (local %s)", name, port, c.opt.Upstream)
	log.Printf("[ssh] public: %s", publicURL)

	host := c.opt.Server
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			host = u.Hostname()
		}
	}
	cfg := &ssh.ClientConfig{
		User:            c.opt.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         20 * time.Second,
	}
	conn, err := ssh.Dial("tcp", net.JoinHostPort(host, "22"), cfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer conn.Close()
	ln, err := conn.ListenTCP(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		return fmt.Errorf("remote listen 127.0.0.1:%d: %w", port, err)
	}
	defer ln.Close()
	upHost := c.opt.Upstream
	if strings.Contains(upHost, "://") {
		if u, err := url.Parse(upHost); err == nil {
			upHost = u.Host
		}
	}
	for {
		remote, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go func() {
			defer remote.Close()
			local, err := net.DialTimeout("tcp", upHost, 15*time.Second)
			if err != nil {
				return
			}
			defer local.Close()
			go func() { _, _ = io.Copy(local, remote) }()
			_, _ = io.Copy(remote, local)
		}()
	}
}
