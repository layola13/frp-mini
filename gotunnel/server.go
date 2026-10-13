// Command line server: HTTP+WS reverse tunnel multiplexor.
//
//	frp-go --mode ws-server --http 8080 --ttl-hours 24 \
//	  --upstream-timeout 5m --public-host dev.cxu.lol
package gotunnel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Options configures a Server. Flag names mirror the TS server.
type Options struct {
	HTTPPort        int
	Token           string
	AdminToken      string
	TTL             time.Duration
	UpstreamTimeout time.Duration
	SSHBasePort     int
	SSHMaxSlots     int
	DBFile          string
	PublicHost      string
	StaticRoot      string
	StaticBinds     map[string]string
	RepoDir         string // serves get.sh/client.ts/landing like the TS server
	// SSH-mode onboarding (original openssh get.sh flow).
	EnrollSecret  string
	EnrollStepSec int64
	AuthKeysFile  string
	SSHHost       string
	SSHUser       string
	// GetSHFile is served at /get.sh (ws/bun version by default);
	// SSHSHFile is served at /ssh.sh and /get-ssh.sh (original openssh
	// version). Two /get.sh flavours, like the old deployment.
	GetSHFile string
	SSHSHFile string
}

// DefaultOptions mirrors the TS defaults (except UpstreamTimeout, which is
// the fix: 25s hardcoded there, 5m here and flag-tunable).
func DefaultOptions() Options {
	return Options{
		HTTPPort:        8080,
		TTL:             24 * time.Hour,
		UpstreamTimeout: 5 * time.Minute,
		SSHBasePort:     14000,
		SSHMaxSlots:     200,
		DBFile:          "./data/routes.json",
		PublicHost:      "proxy.cxu.lol",
		StaticRoot:      "http://127.0.0.1:43110",
		StaticBinds:     map[string]string{},
		RepoDir:         ".",
		EnrollSecret:    "cxu.lol",
		EnrollStepSec:   300,
		AuthKeysFile:    "/home/tunnel/.ssh/authorized_keys",
		SSHUser:         "tunnel",
		GetSHFile:       "get.sh",
		SSHSHFile:       "get-ssh.sh",
	}
}

type agent struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	createdAt time.Time
	expiresAt time.Time
}

type tcpRoute struct {
	Name      string `json:"name"`
	Port      int    `json:"port"`
	PubkeyFp  string `json:"pubkeyFp,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Via       string `json:"via"`
}

type pendingReq struct {
	w       http.ResponseWriter
	flusher http.Flusher
	name    string
	timer   *time.Timer
	mu      sync.Mutex
	started bool // headers already flushed (streaming mode)
	done    bool
	doneCh  chan struct{}
}

// Server is the tunnel multiplexor.
type Server struct {
	opt Options

	mu       sync.Mutex
	agents   map[string]*agent
	routes   map[string]*tcpRoute
	pending  map[string]*pendingReq
	visitors map[string]*websocket.Conn // connId -> visitor ws

	wsUpgrader websocket.Upgrader
	seq        uint64
}

var reservedExact = []string{"get", "join", "get.sh", "join.sh", "client.ts", "remote-auto.sh", "__health", "__routes", "__enroll", "__tunnel", "__admin"}

// NewServer builds a Server and loads persisted ssh routes (same JSON the
// TS server writes, so both can share one DB file during migration).
func NewServer(opt Options) *Server {
	s := &Server{
		opt:      opt,
		agents:   map[string]*agent{},
		routes:   map[string]*tcpRoute{},
		pending:  map[string]*pendingReq{},
		visitors: map[string]*websocket.Conn{},
		wsUpgrader: websocket.Upgrader{
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
	s.loadDB()
	go s.sweepLoop()
	return s
}

func (s *Server) isReserved(name string) bool {
	if strings.HasPrefix(name, "__") {
		return true
	}
	for _, r := range reservedExact {
		if name == r {
			return true
		}
	}
	if _, ok := s.opt.StaticBinds[name]; ok {
		return true
	}
	return false
}

func (s *Server) findActiveTunnel(name string) bool {
	if name == "" || !ValidName(name) {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.opt.StaticBinds[name]; ok {
		return true
	}
	if a, ok := s.agents[name]; ok && a.expiresAt.After(now) {
		return true
	}
	if r, ok := s.routes[name]; ok && r.ExpiresAt > now.UnixMilli() {
		return true
	}
	return false
}

// ---- persistence (compatible with the TS server's routes.json) ----

func (s *Server) loadDB() {
	raw, err := os.ReadFile(s.opt.DBFile)
	if err != nil {
		return
	}
	var doc struct {
		Routes []*tcpRoute `json:"routes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("[server] db load fail %s: %v", s.opt.DBFile, err)
		return
	}
	now := time.Now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range doc.Routes {
		if r.ExpiresAt > now && ValidName(r.Name) {
			s.routes[r.Name] = r
		}
	}
}

func (s *Server) saveDB() {
	s.mu.Lock()
	routes := make([]*tcpRoute, 0, len(s.routes))
	for _, r := range s.routes {
		routes = append(routes, r)
	}
	s.mu.Unlock()
	raw, err := json.MarshalIndent(map[string]any{"routes": routes}, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(s.opt.DBFile)
	if dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(s.opt.DBFile, raw, 0o644); err != nil {
		log.Printf("[server] db save fail: %v", err)
	}
}

func (s *Server) allocPort() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *Server) sweepLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.sweep()
	}
}

func (s *Server) sweep() {
	now := time.Now()
	var changed bool
	s.mu.Lock()
	for n, a := range s.agents {
		if !a.expiresAt.After(now) {
			_ = a.conn.Close()
			delete(s.agents, n)
			changed = true
			log.Printf("[server] expire ws agent %s", n)
		}
	}
	for n, r := range s.routes {
		if r.ExpiresAt <= now.UnixMilli() {
			delete(s.routes, n)
			changed = true
			log.Printf("[server] expire ssh route %s :%d", n, r.Port)
		}
	}
	s.mu.Unlock()
	if changed {
		s.saveDB()
	}
	// keep-alive pings like the TS server (25s).
	s.mu.Lock()
	for _, a := range s.agents {
		a.writeMu.Lock()
		_ = a.conn.WriteJSON(map[string]string{"type": MsgPing})
		a.writeMu.Unlock()
	}
	s.mu.Unlock()
}

// ---- HTTP routing ----

// ServeHTTP implements the full route table.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__tunnel" {
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "websocket required", http.StatusUpgradeRequired)
			return
		}
		s.serveControl(w, r)
		return
	}
	if isWSUpgrade(r) {
		s.serveVisitorWS(w, r)
		return
	}
	if r.URL.Path == "/__health" {
		s.mu.Lock()
		ws := make([]string, 0, len(s.agents))
		for n := range s.agents {
			ws = append(ws, n)
		}
		type sshInfo struct {
			Name      string `json:"name"`
			Port      int    `json:"port"`
			ExpiresAt int64  `json:"expiresAt"`
		}
		var ssh []sshInfo
		for _, rt := range s.routes {
			ssh = append(ssh, sshInfo{rt.Name, rt.Port, rt.ExpiresAt})
		}
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "ws": ws, "ssh": ssh})
		return
	}
	if r.URL.Path == "/__routes" {
		s.serveRoutes(w, r)
		return
	}
	if r.URL.Path == "/get" || r.URL.Path == "/join" {
		// landing text (HTML variant omitted for brevity; scripts below).
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "远端一条命令: curl -fsSL http://%s/get.sh | bash\n", s.opt.PublicHost)
		return
	}
	if r.URL.Path == "/get.sh" || r.URL.Path == "/join.sh" {
		s.serveScript(w, s.opt.GetSHFile)
		return
	}
	// Original openssh flavour of /get.sh (proxy.cxu.lol mode).
	if r.URL.Path == "/ssh.sh" || r.URL.Path == "/get-ssh.sh" || r.URL.Path == "/remote-auto.sh" || r.URL.Path == "/remote-ssh.sh" {
		s.serveScript(w, s.opt.SSHSHFile)
		return
	}
	if r.URL.Path == "/__enroll" && r.Method == "POST" {
		s.handleEnroll(w, r)
		return
	}
	if r.URL.Path == "/__enroll/help" {
		writeJSON(w, 200, map[string]any{
			"ok": true, "host": s.opt.PublicHost,
			"algo":       "code = sha256(SECRET + ':' + floor(unix_now / STEP))[0:8]",
			"secretHint": "cxu.lol", "stepSec": s.opt.EnrollStepSec, "skew": "±1 step",
			"example_sh": `CODE=$(echo -n "cxu.lol:$(( $(date +%s) / 300 ))" | sha256sum | cut -c1-8)`,
			"encrypt_sh": `openssl enc -aes-256-cbc -pbkdf2 -pass pass:$CODE -in ~/.ssh/tunnel.pub | base64 | tr -d '\n'`,
			"post":       fmt.Sprintf(`curl -XPOST https://%s/__enroll -d '{"name":"colab1","code":"$CODE","pubkey_enc":"..."}'`, s.opt.PublicHost),
		})
		return
	}
	if r.URL.Path == "/__admin/alloc" && r.Method == "POST" {
		s.handleAdminAlloc(w, r)
		return
	}
	if r.URL.Path == "/__admin/release" && r.Method == "POST" {
		s.handleAdminRelease(w, r)
		return
	}
	if r.URL.Path == "/client.ts" {
		s.serveRepoFile(w, "src/client.ts", "text/plain; charset=utf-8", nil)
		return
	}
	if (r.URL.Path == "/" || r.URL.Path == "") && s.opt.StaticRoot != "" {
		s.forwardViaURL(s.opt.StaticRoot, w, r, "/"+r.URL.RawQuery, "")
		return
	}
	if m := namePathRE.FindStringSubmatch(r.URL.Path); m != nil {
		name := strings.ToLower(m[1])
		if s.findActiveTunnel(name) {
			if m[2] == "" && !strings.HasSuffix(r.URL.Path, "/") {
				http.Redirect(w, r, "/"+name+"/"+r.URL.RawQuery, http.StatusFound)
				return
			}
			rest := m[2]
			if rest == "" {
				rest = "/"
			}
			s.forwardTunnel(name, w, r, rest)
			return
		}
	}
	if fallback := s.resolveFromReq(r); fallback != "" {
		s.forwardTunnel(fallback, w, r, r.URL.Path)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

var namePathRE = regexp.MustCompile(`^/([A-Za-z0-9-]{1,32})(/.*)?$`)

func isWSUpgrade(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// resolveFromReq mirrors resolveTunnelFromReq: Referer path, then
// cxu_tunnel cookie.
func (s *Server) resolveFromReq(r *http.Request) string {
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			if m := namePathRE.FindStringSubmatch(u.Path); m != nil {
				if c := strings.ToLower(m[1]); s.findActiveTunnel(c) {
					return c
				}
			}
		}
	}
	if ck, err := r.Cookie("cxu_tunnel"); err == nil {
		if c := strings.ToLower(strings.TrimSpace(ck.Value)); s.findActiveTunnel(c) {
			return c
		}
	}
	return ""
}

// serveRepoFile serves repo scripts with optional token replacement.
func (s *Server) serveRepoFile(w http.ResponseWriter, rel, ctype string, repl map[string]string) {
	raw, err := os.ReadFile(filepath.Join(s.opt.RepoDir, rel))
	if err != nil {
		http.Error(w, rel+" missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	body := string(raw)
	for k, v := range repl {
		body = strings.ReplaceAll(body, k, v)
	}
	w.Header().Set("content-type", ctype)
	w.Header().Set("cache-control", "no-cache")
	_, _ = io.WriteString(w, body)
}

var serverDefaultRE = regexp.MustCompile(`SERVER="\$\{SERVER:-[^}]*\}"`)

// serveScript serves a get.sh flavour with its embedded default SERVER
// rewritten to this server's public host (both ws + ssh flavours share the
// same `SERVER="${SERVER:-...}"` idiom).
func (s *Server) serveScript(w http.ResponseWriter, rel string) {
	raw, err := os.ReadFile(filepath.Join(s.opt.RepoDir, rel))
	if err != nil {
		http.Error(w, rel+" missing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	body := serverDefaultRE.ReplaceAllString(string(raw),
		`SERVER="${SERVER:-`+s.opt.PublicHost+`}"`)
	w.Header().Set("content-type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	_, _ = io.WriteString(w, body)
}

func (s *Server) serveRoutes(w http.ResponseWriter, r *http.Request) {
	_ = r
	now := time.Now()
	type route struct {
		Name         string `json:"name"`
		Via          string `json:"via"`
		Path         string `json:"path,omitempty"`
		Port         int    `json:"port,omitempty"`
		ExpiresInSec int64  `json:"expiresInSec"`
	}
	var out []route
	s.mu.Lock()
	for n, a := range s.agents {
		secs := int64(a.expiresAt.Sub(now).Seconds())
		if secs < 0 {
			secs = 0
		}
		out = append(out, route{n, "ws", "/" + n + "/", 0, secs})
	}
	for n, rt := range s.routes {
		secs := (rt.ExpiresAt - now.UnixMilli()) / 1000
		if secs < 0 {
			secs = 0
		}
		out = append(out, route{n, "ssh", "/" + n + "/", rt.Port, secs})
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "routes": out})
}

// forwardTunnel dispatches to static bind, ws agent, or ssh local port.
func (s *Server) forwardTunnel(name string, w http.ResponseWriter, r *http.Request, fwdPath string) {
	if fwdPath == "" {
		fwdPath = "/"
	} else if !strings.HasPrefix(fwdPath, "/") {
		fwdPath = "/" + fwdPath
	}
	if sb, ok := s.opt.StaticBinds[name]; ok {
		s.forwardViaURL(sb, w, r, fwdPath, name)
		return
	}
	s.mu.Lock()
	_, hasAgent := s.agents[name]
	rt, hasRoute := s.routes[name]
	s.mu.Unlock()
	if hasAgent {
		s.forwardViaAgent(name, w, r, fwdPath)
		return
	}
	if hasRoute {
		if rt.ExpiresAt <= time.Now().UnixMilli() {
			s.mu.Lock()
			delete(s.routes, name)
			s.mu.Unlock()
			s.saveDB()
			http.Error(w, "expired, re-alloc", http.StatusGone)
			return
		}
		s.forwardViaLocalPort(rt.Port, w, r, fwdPath, name)
		return
	}
	http.Error(w, fmt.Sprintf("no such tunnel '%s'", name), http.StatusNotFound)
}

var httpNoRedirect = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// forwardViaURL proxies to a static/local URL (ssh-port and static binds).
func (s *Server) forwardViaURL(base string, w http.ResponseWriter, r *http.Request, fwdPath, name string) {
	base = strings.TrimSuffix(base, "/")
	target := base + fwdPath
	if r.URL.RawQuery != "" && !strings.Contains(fwdPath, "?") {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "bad upstream url", http.StatusBadGateway)
		return
	}
	copyHeadersExcept(out.Header, r.Header)
	if u, err := url.Parse(base); err == nil {
		out.Host = u.Host
		out.Header.Set("host", u.Host)
	}
	resp, err := httpNoRedirect.Do(out)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream offline (%s): %v", base, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	s.writeProcessed(w, resp, name)
}

// forwardViaLocalPort proxies to 127.0.0.1:port (ssh -R data path).
func (s *Server) forwardViaLocalPort(port int, w http.ResponseWriter, r *http.Request, fwdPath, name string) {
	s.forwardViaURL(fmt.Sprintf("http://127.0.0.1:%d", port), w, r, fwdPath, name)
}

// writeProcessed copies an upstream response through the HTML/cookie
// rewriting layer (port of processResponse).
func (s *Server) writeProcessed(w http.ResponseWriter, resp *http.Response, name string) {
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		switch lk {
		case "content-encoding", "content-length", "transfer-encoding", "connection":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if name != "" {
		w.Header().Add("Set-Cookie", fmt.Sprintf("cxu_tunnel=%s; Path=/; SameSite=Lax", name))
		if loc := w.Header().Get("location"); loc != "" && strings.HasPrefix(loc, "/") &&
			!strings.HasPrefix(loc, "/"+name+"/") && loc != "/"+name {
			w.Header().Set("location", "/"+name+loc)
		}
	}
	ctype := w.Header().Get("content-type")
	if name != "" && strings.Contains(ctype, "text/html") {
		w.Header().Del("content-security-policy")
		w.Header().Del("content-security-policy-report-only")
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write([]byte(rewriteHTML(string(raw), name)))
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// rewriteHTML injects <base>, rewrites root-relative links, and installs the
// fetch/WS prefix interceptor (port of the TS helper).
func rewriteHTML(html, name string) string {
	headRE := regexp.MustCompile(`(?i)(<head[^>]*>)`)
	if headRE.MatchString(html) {
		html = headRE.ReplaceAllString(html, `$1`+"\n    "+`<base href="/`+name+`/" />`)
	}
	linkRE := regexp.MustCompile(`(?i)(href|src)=["']\/([^"']*)["']`)
	html = linkRE.ReplaceAllStringFunc(html, func(m string) string {
		sub := linkRE.FindStringSubmatch(m)
		if len(sub) != 3 {
			return m
		}
		attr, path := sub[1], sub[2]
		if strings.HasPrefix(path, "/") || strings.HasPrefix(path, name+"/") || path == name {
			return m
		}
		q := `"`
		if strings.Contains(m, `'`) && !strings.Contains(m, `"`) {
			q = `'`
		}
		return attr + "=" + q + "/" + name + "/" + path + q
	})
	helper := `
    <script>
    window.__CODEX_UI_WEBSOCKET_URL__ = "/` + name + `/ws";
    (function() {
      var prefix = "/` + name + `";
      var origFetch = window.fetch;
      if (origFetch) {
        window.fetch = function(input, init) {
          try {
            if (typeof input === "string") {
              if (input.startsWith("/") && !input.startsWith("//") && !input.startsWith(prefix + "/") && input !== prefix) {
                input = prefix + input;
              }
            }
          } catch (e) {}
          return origFetch.call(this, input, init);
        };
      }
      var origOpen = window.XMLHttpRequest && window.XMLHttpRequest.prototype.open;
      if (origOpen) {
        window.XMLHttpRequest.prototype.open = function(method, url) {
          try {
            if (typeof url === "string" && url.startsWith("/") && !url.startsWith("//") && !url.startsWith(prefix + "/") && url !== prefix) {
              url = prefix + url;
            }
          } catch (e) {}
          return origOpen.apply(this, arguments);
        };
      }
    })();
    </script>
`
	if idx := strings.LastIndex(strings.ToLower(html), "</head>"); idx >= 0 {
		html = html[:idx] + helper + html[idx:]
	} else {
		html += helper
	}
	return html
}

func copyHeadersExcept(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		switch lk {
		case "host", "connection", "content-length":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// forwardViaAgent relays one HTTP request over the agent control channel.
// Timeout is flag-tunable (default 5m); the legacy 25s wall is gone.
func (s *Server) forwardViaAgent(name string, w http.ResponseWriter, r *http.Request, fwdPath string) {
	s.mu.Lock()
	a, ok := s.agents[name]
	s.mu.Unlock()
	if !ok {
		http.Error(w, fmt.Sprintf("agent '%s' offline", name), http.StatusBadGateway)
		return
	}
	var body []byte
	if r.Method != "GET" && r.Method != "HEAD" {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > MaxBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
	}
	id := s.nextID()
	msg, _ := json.Marshal(ReqMsg{
		Type: MsgReq, Method: r.Method, Path: fwdPath, Tunnel: name,
		ID: id, Headers: hopHeaders(r.Header), BodyB64: EncodeBody(body),
	})
	flusher, canFlush := w.(http.Flusher)
	p := &pendingReq{name: name, doneCh: make(chan struct{})}
	p.timer = time.AfterFunc(s.opt.UpstreamTimeout, func() {
		s.mu.Lock()
		if cur, ok := s.pending[id]; ok && cur == p {
			delete(s.pending, id)
		}
		s.mu.Unlock()
		s.finishPending(p, func() {
			if !p.started {
				http.Error(p.w, ResponseError(fmt.Errorf("upstream timeout")), http.StatusGatewayTimeout)
			}
			// Streaming already in flight: just stop; the visitor sees a
			// truncated body instead of a late 504.
		})
	})
	s.mu.Lock()
	s.pending[id] = p
	s.mu.Unlock()
	p.w = w
	if canFlush {
		p.flusher = flusher
	}
	a.writeMu.Lock()
	err := a.conn.WriteMessage(websocket.TextMessage, msg)
	a.writeMu.Unlock()
	if err != nil {
		p.timer.Stop()
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		http.Error(w, fmt.Sprintf("agent '%s' unreachable: %v", name, err), http.StatusBadGateway)
		return
	}
	<-p.doneCh
}

// finishPending marks completion exactly once and runs fn under lock.
func (s *Server) finishPending(p *pendingReq, fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	fn()
	close(p.doneCh)
}

// nextID mints request ids.
func (s *Server) nextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("%x-%x", time.Now().UnixMilli(), s.seq)
}

// applyRes assembles a legacy single-shot agent response.
func (s *Server) applyRes(id string, m ResMsg) {
	s.mu.Lock()
	p, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	p.timer.Stop()
	body, err := DecodeBody(m.BodyB64)
	if err != nil {
		s.finishPending(p, func() {
			http.Error(p.w, "bad body encoding", http.StatusBadGateway)
		})
		return
	}
	hdr := http.Header{}
	for k, v := range m.Headers {
		lk := strings.ToLower(k)
		switch lk {
		case "content-encoding", "content-length", "transfer-encoding", "connection":
			continue
		}
		hdr.Set(k, v)
	}
	resp := &http.Response{StatusCode: m.Status, Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}
	s.finishPending(p, func() {
		s.writeProcessed(p.w, resp, p.name)
	})
}

// applyChunked handles res_start/res_chunk/res_end/res_error: headers flush
// immediately and every chunk flushes, so SSE streams end-to-end.
func (s *Server) applyChunkedStart(id string, m ResStartMsg) {
	s.mu.Lock()
	p, ok := s.pending[id]
	s.mu.Unlock()
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done || p.started {
		return
	}
	if p.flusher == nil {
		// Visitor doesn't support flushing: fall back to buffering.
		return
	}
	for k, v := range m.Headers {
		lk := strings.ToLower(k)
		switch lk {
		case "content-encoding", "content-length", "transfer-encoding", "connection":
			continue
		}
		p.w.Header().Set(k, v)
	}
	if p.name != "" {
		p.w.Header().Add("Set-Cookie", fmt.Sprintf("cxu_tunnel=%s; Path=/; SameSite=Lax", p.name))
	}
	p.w.WriteHeader(m.Status)
	p.flusher.Flush()
	p.started = true
}

func (s *Server) applyChunkedData(id string, dataB64 string) {
	s.mu.Lock()
	p, ok := s.pending[id]
	s.mu.Unlock()
	if !ok {
		return
	}
	chunk, err := DecodeBody(dataB64)
	if err != nil || len(chunk) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done || !p.started || p.flusher == nil {
		return
	}
	_, _ = p.w.Write(chunk)
	p.flusher.Flush()
}

func (s *Server) applyChunkedEnd(id string, failed bool, errMsg string) {
	s.mu.Lock()
	p, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	p.timer.Stop()
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		// Never started streaming: synthesize the legacy error shape.
		s.finishPending(p, func() {
			if failed {
				http.Error(p.w, ResponseError(fmt.Errorf("%s", errMsg)), http.StatusBadGateway)
			} else {
				p.w.WriteHeader(http.StatusOK)
			}
		})
		return
	}
	s.finishPending(p, func() {})
	// Started + ended: HTTP body simply completes (chunked framing ends it).
}

// Run starts serving (blocking).
func (s *Server) Run() error {
	addr := fmt.Sprintf(":%d", s.opt.HTTPPort)
	log.Printf("[server] http+control ws on %s ttl=%s upstream-timeout=%s db=%s", addr, s.opt.TTL, s.opt.UpstreamTimeout, s.opt.DBFile)
	srv := &http.Server{Addr: addr, Handler: s, ReadHeaderTimeout: 15 * time.Second}
	return srv.ListenAndServe()
}

// ---- control plane ----

// serveControl upgrades /__tunnel to the agent control channel.
func (s *Server) serveControl(w http.ResponseWriter, r *http.Request) {
	conn, err := s.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	var agentNames []string
	defer func() {
		_ = conn.Close()
		if len(agentNames) > 0 {
			s.mu.Lock()
			for _, n := range agentNames {
				if cur, ok := s.agents[n]; ok && cur.conn == conn {
					delete(s.agents, n)
					log.Printf("[server] agent offline: %s", n)
				}
			}
			s.mu.Unlock()
		}
	}()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}
		switch base.Type {
		case MsgPong:
			continue
		case MsgRegister:
			var m RegisterMsg
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(m.Name))
			if !ValidName(name) {
				_ = conn.WriteJSON(map[string]string{"type": MsgError, "error": "bad name"})
				return
			}
			if s.opt.Token != "" && m.Token != s.opt.Token {
				_ = conn.WriteJSON(map[string]string{"type": MsgError, "error": "bad token"})
				return
			}
			s.mu.Lock()
			if old, ok := s.agents[name]; ok && old.conn != conn {
				_ = old.conn.Close()
			}
			now := time.Now()
			created := now
			if old, ok := s.agents[name]; ok {
				created = old.createdAt
			}
			s.agents[name] = &agent{conn: conn, createdAt: created, expiresAt: now.Add(s.opt.TTL)}
			exp := now.Add(s.opt.TTL)
			owned := false
			for _, n := range agentNames {
				if n == name {
					owned = true
					break
				}
			}
			if !owned {
				agentNames = append(agentNames, name)
			}
			s.mu.Unlock()
			_ = conn.WriteJSON(map[string]any{
				"type": MsgRegistered, "name": name,
				"expiresAt": exp.UnixMilli(),
				"publicUrl": fmt.Sprintf("https://%s/%s/", s.opt.PublicHost, name),
				"streaming": true, // Go server: chunked res_* supported
			})
			log.Printf("[server] agent online: %s", name)
		case MsgRes:
			var m ResMsg
			if err := json.Unmarshal(raw, &m); err == nil && m.ID != "" {
				s.applyRes(m.ID, m)
			}
		case MsgResStart:
			var m ResStartMsg
			if err := json.Unmarshal(raw, &m); err == nil && m.ID != "" {
				s.applyChunkedStart(m.ID, m)
			}
		case MsgResChunk:
			var m ResChunkMsg
			if err := json.Unmarshal(raw, &m); err == nil && m.ID != "" {
				s.applyChunkedData(m.ID, m.Data)
			}
		case MsgResEnd:
			var m ResEndMsg
			if err := json.Unmarshal(raw, &m); err == nil && m.ID != "" {
				s.applyChunkedEnd(m.ID, false, "")
			}
		case MsgResError:
			var m ResErrorMsg
			if err := json.Unmarshal(raw, &m); err == nil && m.ID != "" {
				s.applyChunkedEnd(m.ID, true, m.Error)
			}
		case MsgWSData:
			var m WSDataMsg
			if err := json.Unmarshal(raw, &m); err != nil || m.ConnID == "" {
				continue
			}
			s.mu.Lock()
			vws, ok := s.visitors[m.ConnID]
			s.mu.Unlock()
			if !ok {
				continue
			}
			if m.IsBinary && m.Data != "" {
				if b, err := DecodeBody(m.Data); err == nil {
					_ = vws.WriteMessage(websocket.BinaryMessage, b)
				}
			} else {
				_ = vws.WriteMessage(websocket.TextMessage, []byte(m.Data))
			}
		case MsgWSClose:
			var m WSCloseMsg
			if err := json.Unmarshal(raw, &m); err != nil || m.ConnID == "" {
				continue
			}
			s.mu.Lock()
			vws, ok := s.visitors[m.ConnID]
			if ok {
				delete(s.visitors, m.ConnID)
			}
			s.mu.Unlock()
			if ok {
				code := m.Code
				if code == 0 {
					code = websocket.CloseNormalClosure
				}
				_ = vws.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(code, m.Reason))
			}
		}
	}
}

// visitorCtx carries an upgraded visitor websocket and its target.
type visitorCtx struct {
	conn      *websocket.Conn
	connID    string
	agentName string
	fwdPath   string
}

// serveVisitorWS upgrades a visitor websocket for a live tunnel and bridges
// it to the agent (agent_ws_visitor) or straight upstream (static/ssh).
func (s *Server) serveVisitorWS(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path + "?" + r.URL.RawQuery
	var wsName, fwdPath string
	if m := namePathRE.FindStringSubmatch(r.URL.Path); m != nil {
		if c := strings.ToLower(m[1]); s.findActiveTunnel(c) {
			wsName = c
			fwdPath = m[2]
			if fwdPath == "" {
				fwdPath = "/"
			}
			fwdPath += "?" + r.URL.RawQuery
		}
	}
	_ = path
	if wsName == "" {
		wsName = s.resolveFromReq(r)
		fwdPath = r.URL.Path + "?" + r.URL.RawQuery
	}
	if wsName == "" {
		http.Error(w, "websocket tunnel not found", http.StatusNotFound)
		return
	}
	// Agent path: relay frames through the control channel.
	s.mu.Lock()
	ag, hasAgent := s.agents[wsName]
	s.mu.Unlock()
	if hasAgent && ag.expiresAt.After(time.Now()) {
		conn, err := s.wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connID := s.nextID()
		s.mu.Lock()
		s.visitors[connID] = conn
		s.mu.Unlock()
		vc := &visitorCtx{conn: conn, connID: connID, agentName: wsName, fwdPath: fwdPath}
		ag.writeMu.Lock()
		err = ag.conn.WriteJSON(WSOpenMsg{Type: MsgWSOpen, ConnID: connID, Path: fwdPath, Tunnel: wsName,
			Protocols: r.Header.Get("Sec-WebSocket-Protocol")})
		ag.writeMu.Unlock()
		if err != nil {
			s.closeVisitor(vc, websocket.CloseInternalServerErr, "agent offline")
			return
		}
		go s.pumpVisitor(vc)
		return
	}
	// Static/ssh path: bridge straight to the upstream websocket.
	var target string
	if sb, ok := s.opt.StaticBinds[wsName]; ok {
		u, err := url.Parse(sb)
		if err != nil {
			http.Error(w, "bad static bind", http.StatusInternalServerError)
			return
		}
		scheme := "ws"
		if u.Scheme == "https" {
			scheme = "wss"
		}
		target = scheme + "://" + u.Host + fwdPath
	} else {
		s.mu.Lock()
		rt, ok := s.routes[wsName]
		s.mu.Unlock()
		if !ok || rt.ExpiresAt <= time.Now().UnixMilli() {
			http.Error(w, "websocket tunnel not found", http.StatusNotFound)
			return
		}
		target = fmt.Sprintf("ws://127.0.0.1:%d%s", rt.Port, fwdPath)
	}
	s.bridgeWS(w, r, target)
}

func (s *Server) closeVisitor(vc *visitorCtx, code int, reason string) {
	s.mu.Lock()
	if cur, ok := s.visitors[vc.connID]; ok && cur == vc.conn {
		delete(s.visitors, vc.connID)
	}
	ag, ok := s.agents[vc.agentName]
	s.mu.Unlock()
	_ = vc.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
	_ = vc.conn.Close()
	if ok {
		ag.writeMu.Lock()
		_ = ag.conn.WriteJSON(WSCloseMsg{Type: MsgWSClose, ConnID: vc.connID})
		ag.writeMu.Unlock()
	}
}

// pumpVisitor forwards visitor -> agent frames.
func (s *Server) pumpVisitor(vc *visitorCtx) {
	defer s.closeVisitor(vc, websocket.CloseNormalClosure, "")
	for {
		mt, data, err := vc.conn.ReadMessage()
		if err != nil {
			return
		}
		s.mu.Lock()
		ag, ok := s.agents[vc.agentName]
		s.mu.Unlock()
		if !ok {
			return
		}
		var msg WSDataMsg
		msg.Type = MsgWSData
		msg.ConnID = vc.connID
		if mt == websocket.BinaryMessage {
			msg.Data = EncodeBody(data)
			msg.IsBinary = true
		} else {
			msg.Data = string(data)
		}
		ag.writeMu.Lock()
		err = ag.conn.WriteJSON(msg)
		ag.writeMu.Unlock()
		if err != nil {
			return
		}
	}
}

// bridgeWS proxies a visitor websocket straight upstream (static/ssh).
func (s *Server) bridgeWS(w http.ResponseWriter, r *http.Request, target string) {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	hdr := http.Header{}
	if p := r.Header.Get("Sec-WebSocket-Protocol"); p != "" {
		hdr.Set("Sec-WebSocket-Protocol", p)
	}
	up, _, err := dialer.Dial(target, hdr)
	if err != nil {
		http.Error(w, "upstream ws unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	down, err := s.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = up.Close()
		return
	}
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = up.Close()
			_ = down.Close()
		})
	}
	go func() {
		defer closeBoth()
		for {
			mt, data, err := up.ReadMessage()
			if err != nil {
				return
			}
			_ = down.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := down.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}()
	func() {
		defer closeBoth()
		for {
			mt, data, err := down.ReadMessage()
			if err != nil {
				return
			}
			_ = up.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := up.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}()
}
