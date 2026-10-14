// Command frp-go is the Go port of frp-mini's tunnel pieces.
//
// One binary, --mode selects the piece (flags mirror the TS server/client):
//
//	frp-go --mode ws-server --http 18080 --ttl-hours 24 --upstream-timeout 5m
//	frp-go --mode ws-client --server wss://host/__tunnel --name colab1 --upstream http://127.0.0.1:43210
//	frp-go --mode ssh-client --server dev.cxu.lol --name colab1 --local 43210
//
// Set --mode once; everything else has the same defaults as the TS version,
// except --upstream-timeout (the TS server hardcodes 25s; here 5m, tunable).
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"frp-go/gotunnel"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	mode := flag.String("mode", "", "ws-server | ws-client | ssh-client (required)")
	// shared
	httpPort := flag.Int("http", 18080, "ws-server listen port (go branch, offset from main)")
	token := flag.String("token", os.Getenv("TUNNEL_TOKEN"), "agent token (ws-server expects, ws-client sends)")
	ttlHours := flag.Float64("ttl-hours", 24, "agent/route TTL hours")
	upstreamTimeout := flag.Duration("upstream-timeout", 5*time.Minute, "per-request upstream deadline (the 25s-wall fix)")
	db := flag.String("db", "./data/routes.json", "ssh routes DB (same JSON as the TS server)")
	publicHost := flag.String("public-host", "proxy.cxu.lol", "public hostname for URLs")
	staticRoot := flag.String("static-root", "http://127.0.0.1:43210", "static / binding (empty disables; go branch, offset from main)")
	staticBinds := flag.String("static-binds", "", "extra static binds name=url,...")
	repoDir := flag.String("repo-dir", ".", "repo root serving get.sh/client.ts")
	sshBase := flag.Int("ssh-base-port", 15000, "ssh -R port range base (go branch, offset from main)")
	sshMax := flag.Int("ssh-max-slots", 200, "ssh -R port range size")
	enrollSecret := flag.String("enroll-secret", "cxu.lol", "enroll secret (server verifies, ssh-client computes)")
	enrollStep := flag.Int64("enroll-step-sec", 300, "enroll time step seconds")
	authKeys := flag.String("auth-keys", "/home/tunnel/.ssh/authorized_keys", "ssh-mode authorized_keys file")
	sshHost := flag.String("ssh-host", "", "ssh public host (server advertises, default public-host)")
	sshUser := flag.String("ssh-user", "tunnel", "ssh remote user (server advertises, ssh-client dials)")
	adminToken := flag.String("admin-token", os.Getenv("ADMIN_TOKEN"), "POST /__admin/* token (empty disables auth)")
	getshFile := flag.String("getsh-file", "get.sh", "script served at /get.sh (ws flavour)")
	sshshFile := flag.String("sshsh-file", "get-ssh.sh", "script served at /ssh.sh (original openssh flavour)")
	// client
	server := flag.String("server", "", "tunnel server URL (ws-client: wss://host/__tunnel; ssh-client: host)")
	name := flag.String("name", "colab1", "tunnel name")
	upstream := flag.String("upstream", "http://127.0.0.1:43210", "local upstream base (go branch, offset from main)")
	local := flag.Int("local", 43210, "ssh-client local port (go branch, offset from main)")
	keyFile := flag.String("key", os.ExpandEnv("$HOME/.ssh/tunnel"), "ssh-client key file")
	flag.Parse()

	switch *mode {
	case "ws-server":
		opt := gotunnel.DefaultOptions()
		opt.HTTPPort = *httpPort
		opt.Token = *token
		opt.TTL = time.Duration(*ttlHours * float64(time.Hour))
		opt.UpstreamTimeout = *upstreamTimeout
		opt.DBFile = *db
		opt.PublicHost = *publicHost
		opt.StaticRoot = *staticRoot
		opt.StaticBinds = parseBinds(*staticBinds)
		opt.RepoDir = *repoDir
		opt.SSHBasePort = *sshBase
		opt.SSHMaxSlots = *sshMax
		opt.EnrollSecret = *enrollSecret
		opt.EnrollStepSec = *enrollStep
		opt.AuthKeysFile = *authKeys
		opt.SSHHost = *sshHost
		opt.SSHUser = *sshUser
		opt.AdminToken = *adminToken
		opt.GetSHFile = *getshFile
		opt.SSHSHFile = *sshshFile
		srv := gotunnel.NewServer(opt)
		log.Fatal(srv.Run())
	case "ws-client":
		if *server == "" {
			fatal("ws-client needs --server")
		}
		gotunnel.NewClient(gotunnel.ClientOptions{
			Server: *server, Name: *name, Upstream: *upstream, Token: *token,
		}).Run()
	case "ssh-client":
		if *server == "" {
			fatal("ssh-client needs --server")
		}
		gotunnel.NewSSHClient(gotunnel.SSHOptions{
			Server: *server, User: *sshUser, Local: *local, KeyFile: *keyFile,
			Name: *name, Secret: *enrollSecret, StepSec: *enrollStep,
			Upstream: *upstream,
		}).Run()
	default:
		fmt.Fprintln(os.Stderr, "need --mode ws-server|ws-client|ssh-client")
		flag.Usage()
		os.Exit(2)
	}
}

func parseBinds(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.Index(pair, "=")
		if i <= 0 {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(pair[:i]))] = strings.TrimSuffix(strings.TrimSpace(pair[i+1:]), "/")
	}
	return out
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}
