package gotunnel

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ClientOptions configures a WS tunnel client.
type ClientOptions struct {
	Server   string // ws(s)://host/__tunnel or host:port
	Name     string // comma-separated names; all register on one control conn
	Upstream string // upstream set: "base" or "/prefix=http://host:port,..."
	Token    string
}

// upstreamRoute maps a path prefix to a local base URL.
type upstreamRoute struct {
	prefix string // always starts with "/"
	base   string // no trailing slash
}

// parseUpstreams parses "http://127.0.0.1:43110" (single default) or
// "/app=http://127.0.0.1:43110,/api=http://127.0.0.1:8080" plus an
// optional bare default entry. Result is longest-prefix-first.
func parseUpstreams(s string) ([]upstreamRoute, error) {
	var out []upstreamRoute
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, base := "/", part
		if i := strings.Index(part, "="); i >= 0 {
			prefix, base = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("bad upstream prefix %q (want /path=...)", prefix)
		}
		base = strings.TrimSuffix(base, "/")
		if base == "" || strings.Contains(base, " ") {
			return nil, fmt.Errorf("bad upstream base %q", base)
		}
		out = append(out, upstreamRoute{prefix, base})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no upstreams")
	}
	sortByPrefixLen(out)
	return out, nil
}

func sortByPrefixLen(routes []upstreamRoute) {
	for i := 1; i < len(routes); i++ {
		for j := i; j > 0 && len(routes[j].prefix) > len(routes[j-1].prefix); j-- {
			routes[j], routes[j-1] = routes[j-1], routes[j]
		}
	}
}

// routeFor picks the longest matching prefix ("/" default always matches).
func routeFor(routes []upstreamRoute, path string) upstreamRoute {
	best := routes[len(routes)-1]
	for _, r := range routes {
		if r.prefix == "/" {
			continue
		}
		if path == r.prefix || strings.HasPrefix(path, r.prefix+"/") {
			return r
		}
	}
	// fall back to the "/" entry (or the last route if none declared)
	for _, r := range routes {
		if r.prefix == "/" {
			return r
		}
	}
	return best
}

// Client runs the agent side of the WS tunnel.
type Client struct {
	opt      ClientOptions
	names    []string
	routes   []upstreamRoute
	dialer   websocket.Dialer
	http     *http.Client
	mu       sync.Mutex
	conn     *websocket.Conn
	upConns  map[string]*websocket.Conn // connId -> upstream ws
	upMu     sync.Mutex                 // serializes upstream writes (incl. close frames)
	streamOK bool                       // server advertised streaming
}

// writeUp writes one frame to an upstream connection. Gorilla connections
// (data and close frames alike) must not be written concurrently.
func (c *Client) writeUp(connID string, mt int, data []byte) error {
	c.mu.Lock()
	u, ok := c.upConns[connID]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("upstream %s gone", connID)
	}
	c.upMu.Lock()
	defer c.upMu.Unlock()
	return u.WriteMessage(mt, data)
}

// NewClient builds a Client.
func NewClient(opt ClientOptions) *Client {
	opt.Upstream = strings.TrimSpace(opt.Upstream)
	return &Client{
		opt:     opt,
		dialer:  websocket.Dialer{HandshakeTimeout: 20 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
		http:    &http.Client{Timeout: 0}, // streaming reads; per-request bound by server deadline
		upConns: map[string]*websocket.Conn{},
	}
}

// clientNames splits --name into lowercased names.
func clientNames(opt ClientOptions) []string {
	var out []string
	for _, n := range strings.Split(opt.Name, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		out = []string{"colab1"}
	}
	return out
}

// upstreamBase resolves the local base URL for one client name (shared
// routing table across all names on this control connection).
func (c *Client) upstreamBase() string {
	if routes, err := parseUpstreams(c.opt.Upstream); err == nil && len(routes) > 0 {
		for _, r := range routes {
			if r.prefix == "/" {
				return r.base
			}
		}
		return routes[0].base
	}
	return strings.TrimSuffix(c.opt.Upstream, "/")
}

func (c *Client) serverURL(name string) string {
	raw := c.opt.Server
	if strings.Contains(raw, "://") {
		sep := "?"
		if strings.Contains(raw, "?") {
			sep = "&"
		}
		return raw + sep + "name=" + url.QueryEscape(name)
	}
	h, p := raw, "8080"
	if i := strings.LastIndex(raw, ":"); i >= 0 {
		h, p = raw[:i], raw[i+1:]
	}
	return "ws://" + h + ":" + p + "/__tunnel?name=" + url.QueryEscape(name)
}

// Run connects, registers all names, and serves until the process dies
// (reconnects with backoff like the TS client).
func (c *Client) Run() error {
	routes, err := parseUpstreams(c.opt.Upstream)
	if err != nil {
		return err
	}
	c.routes = routes
	c.names = clientNames(c.opt)
	for _, n := range c.names {
		log.Printf("[client] %s -> upstream %s", n, c.opt.Upstream)
	}
	backoff := time.Second
	for {
		if err := c.serveOnce(); err != nil {
			log.Printf("[client] control error: %v", err)
		}
		log.Printf("[client] disconnected, retry in %s", backoff)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (c *Client) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("no control connection")
	}
	return c.conn.WriteJSON(v)
}

func (c *Client) serveOnce() error {
	conn, _, err := c.dialer.Dial(c.serverURL(c.names[0]), nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.streamOK = false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = conn.Close()
	}()
	for _, n := range c.names {
		if err := conn.WriteJSON(RegisterMsg{Type: MsgRegister, Name: n, Token: c.opt.Token}); err != nil {
			return err
		}
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			continue
		}
		switch base.Type {
		case MsgPing:
			_ = c.send(map[string]string{"type": MsgPong})
		case MsgRegistered:
			var m struct {
				Name      string `json:"name"`
				Streaming bool   `json:"streaming"`
			}
			if json.Unmarshal(raw, &m) == nil {
				c.mu.Lock()
				c.streamOK = m.Streaming
				c.mu.Unlock()
				log.Printf("[client] route live: /%s/ (streaming=%v)", m.Name, m.Streaming)
				continue
			}
			log.Printf("[client] route live")
		case MsgError:
			var m struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &m)
			log.Printf("[client] server error: %s", m.Error)
		case MsgReq:
			var m ReqMsg
			if json.Unmarshal(raw, &m) == nil {
				go c.handleReq(m)
			}
		case MsgWSOpen:
			var m WSOpenMsg
			if json.Unmarshal(raw, &m) == nil {
				go c.handleWSOpen(m)
			}
		case MsgWSData:
			var m WSDataMsg
			if json.Unmarshal(raw, &m) == nil {
				if m.IsBinary && m.Data != "" {
					if b, err := DecodeBody(m.Data); err == nil {
						_ = c.writeUp(m.ConnID, websocket.BinaryMessage, b)
					}
				} else {
					_ = c.writeUp(m.ConnID, websocket.TextMessage, []byte(m.Data))
				}
			}
		case MsgWSClose:
			var m WSCloseMsg
			if json.Unmarshal(raw, &m) == nil {
				c.closeUpstream(m.ConnID, m.Code, m.Reason)
			}
		}
	}
}

func (c *Client) closeUpstream(connID string, code int, reason string) {
	c.mu.Lock()
	u, ok := c.upConns[connID]
	if ok {
		delete(c.upConns, connID)
	}
	c.mu.Unlock()
	if ok {
		if code == 0 {
			code = websocket.CloseNormalClosure
		}
		c.upMu.Lock()
		_ = u.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
		_ = u.Close()
		c.upMu.Unlock()
	}
}

// handleReq forwards one HTTP request upstream. With a streaming-capable
// server the body is forwarded incrementally (res_start/chunk/end), so SSE
// streams end-to-end; otherwise it falls back to one legacy res message.
// The local base is picked by longest-prefix match, so one agent serves
// many local ports behind one public name.
func (c *Client) handleReq(m ReqMsg) {
	name := m.Tunnel
	if name == "" {
		name = c.names[0]
	}
	route := routeFor(c.routes, m.Path)
	fail := func(err error) {
		_ = c.send(ResMsg{Type: MsgRes, ID: m.ID, Status: http.StatusBadGateway,
			Headers: map[string]string{"content-type": "text/plain"},
			BodyB64: EncodeBody([]byte(fmt.Sprintf("upstream %s unreachable: %v", route.base, err)))})
	}
	upURL := route.base + m.Path
	var body io.Reader
	if m.BodyB64 != "" {
		b, err := DecodeBody(m.BodyB64)
		if err != nil {
			fail(err)
			return
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(m.Method, upURL, body)
	if err != nil {
		fail(err)
		return
	}
	for k, v := range m.Headers {
		lk := strings.ToLower(k)
		if lk == "host" {
			continue
		}
		req.Header.Set(k, v)
	}
	if u, err := url.Parse(route.base); err == nil {
		req.Host = u.Host
		req.Header.Set("host", u.Host)
		req.Header.Set("x-forwarded-prefix", "/"+name)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	headers := hopHeaders(resp.Header)

	c.mu.Lock()
	streaming := c.streamOK
	c.mu.Unlock()
	if !streaming {
		// Legacy path: buffer whole body like the TS client.
		buf, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
		if err != nil {
			fail(err)
			return
		}
		if len(buf) > MaxBody {
			buf = buf[:MaxBody]
		}
		_ = c.send(ResMsg{Type: MsgRes, ID: m.ID, Status: resp.StatusCode, Headers: headers, BodyB64: EncodeBody(buf)})
		return
	}
	// Streaming path: headers first, then chunks as they arrive.
	if err := c.send(ResStartMsg{Type: MsgResStart, ID: m.ID, Status: resp.StatusCode, Headers: headers}); err != nil {
		return
	}
	buf := make([]byte, ChunkSize)
	total := 0
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			total += n
			if total > MaxBody {
				_ = c.send(ResErrorMsg{Type: MsgResError, ID: m.ID, Error: "body too large"})
				return
			}
			if serr := c.send(ResChunkMsg{Type: MsgResChunk, ID: m.ID, Data: EncodeBody(buf[:n])}); serr != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	_ = c.send(ResEndMsg{Type: MsgResEnd, ID: m.ID})
}

// handleWSOpen proxies one full-duplex websocket upstream.
func (c *Client) handleWSOpen(m WSOpenMsg) {
	route := routeFor(c.routes, m.Path)
	upBase := strings.Replace(route.base, "http://", "ws://", 1)
	upBase = strings.Replace(upBase, "https://", "wss://", 1)
	upURL := upBase + m.Path
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	hdr := http.Header{}
	if m.Protocols != "" {
		hdr.Set("Sec-WebSocket-Protocol", m.Protocols)
	}
	up, _, err := dialer.Dial(upURL, hdr)
	if err != nil {
		_ = c.send(WSCloseMsg{Type: MsgWSClose, ConnID: m.ConnID, Code: 1011, Reason: "upstream ws error"})
		return
	}
	c.mu.Lock()
	c.upConns[m.ConnID] = up
	c.mu.Unlock()
	go func() {
		defer c.closeUpstream(m.ConnID, websocket.CloseNormalClosure, "")
		for {
			mt, data, err := up.ReadMessage()
			if err != nil {
				_ = c.send(WSCloseMsg{Type: MsgWSClose, ConnID: m.ConnID, Code: 1011, Reason: "upstream ws error"})
				return
			}
			msg := WSDataMsg{Type: MsgWSData, ConnID: m.ConnID}
			if mt == websocket.BinaryMessage {
				msg.Data = EncodeBody(data)
				msg.IsBinary = true
			} else {
				msg.Data = string(data)
			}
			if err := c.send(msg); err != nil {
				return
			}
		}
	}()
}
