package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codesweep-ai/vcr/internal/config"
)

// connectServer is a cs-vcr in front of a real listener, which a CONNECT needs:
// httptest.NewRecorder cannot be hijacked.
func connectServer(t *testing.T, off bool) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Cassettes: t.TempDir(),
		Providers: map[string]*config.Provider{
			"anthropic": {BaseURL: "https://api.anthropic.com"},
			"openai":    {BaseURL: "https://api.openai.com"},
		},
	}
	srv := httptest.NewServer(New(cfg, slog.New(slog.DiscardHandler), off))
	t.Cleanup(srv.Close)
	return srv
}

// connectTo sends one CONNECT to the proxy and returns the status line's code,
// plus the live connection when the tunnel opened.
func connectTo(t *testing.T, proxyAddr, target string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, c, br
}

// TestConnectRefusesTheHostsAnAgentContactsItself: the default list is refused,
// whatever else the session is doing. These are the calls a base URL does not
// govern, and the whole point of answering CONNECT is to refuse them.
func TestConnectRefusesTheHostsAnAgentContactsItself(t *testing.T) {
	srv := connectServer(t, online)
	for _, host := range []string{
		"api.anthropic.com:443",
		"chatgpt.com:443",
		"ab.chatgpt.com:443",
		// Case and a trailing dot are the same host to a resolver, so they are
		// the same host here.
		"API.Anthropic.com:443",
		"chatgpt.com.:443",
	} {
		t.Run(host, func(t *testing.T) {
			code, conn, _ := connectTo(t, srv.Listener.Addr().String(), host)
			if conn != nil {
				conn.Close()
			}
			if code != http.StatusForbidden {
				t.Errorf("CONNECT %s = %d, want %d", host, code, http.StatusForbidden)
			}
		})
	}
}

// TestConnectAllowsTheHostsToolsNeed: the list is a refusal list, not an
// allowlist, and github.com is the case that says so. Codex reaches it, but an
// agent's tools reach it far more — a clone, a release download, an API call in
// a script the agent wrote — and nothing it returns has been seen to change a
// prompt. Asserted by name so that adding it later is a decision rather than a
// slip.
func TestConnectAllowsTheHostsToolsNeed(t *testing.T) {
	s := New(&config.Config{}, slog.New(slog.DiscardHandler), online)
	for _, host := range []string{
		"github.com", "api.github.com", "raw.githubusercontent.com",
		"pypi.org", "registry.npmjs.org", "proxy.golang.org",
	} {
		if why := s.tunnelRefusal(host); why != "" {
			t.Errorf("%s refused: %s", host, why)
		}
	}
}

// TestConnectTunnelsEverythingElse: an agent's tools share its environment, so
// a proxy that refused everything would take away git, curl and every package
// manager. github.com is the case that says so out loud — Codex reaches it, and
// it is deliberately NOT on the list.
func TestConnectTunnelsEverythingElse(t *testing.T) {
	// A stand-in for "somewhere on the internet", so the test needs no network.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b := make([]byte, 5)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		_, _ = c.Write(append([]byte("echo:"), b...))
	}()

	srv := connectServer(t, online)
	code, conn, br := connectTo(t, srv.Listener.Addr().String(), upstream.Addr().String())
	if code != http.StatusOK {
		t.Fatalf("CONNECT to an unlisted host = %d, want 200", code)
	}
	defer conn.Close()

	// The tunnel carries bytes in both directions, untouched.
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 10)
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "echo:hello" {
		t.Errorf("tunnel carried %q, want %q", got, "echo:hello")
	}
}

// TestConnectLeavesTheRequestCountersAlone: a recording session asserts that
// every request it counted was recorded, and a tunnel records nothing. Counting
// one as a request failed that assertion for every agent that reaches past its
// base URL, which was six of the eight fixtures.
func TestConnectLeavesTheRequestCountersAlone(t *testing.T) {
	srv := connectServer(t, online)
	addr := srv.Listener.Addr().String()

	// One refused, and one carried.
	code, conn, _ := connectTo(t, addr, "api.anthropic.com:443")
	if conn != nil {
		conn.Close()
	}
	if code != http.StatusForbidden {
		t.Fatalf("CONNECT to a listed host = %d, want 403", code)
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		if c, err := upstream.Accept(); err == nil {
			c.Close()
		}
	}()
	if code, conn, _ := connectTo(t, addr, upstream.Addr().String()); code == http.StatusOK {
		conn.Close()
	} else {
		t.Fatalf("CONNECT to an unlisted host = %d, want 200", code)
	}

	got := srv.Config.Handler.(*Server).Snapshot()
	if got.TunnelOpened != 1 || got.TunnelBlocked != 1 {
		t.Errorf("tunnel counters = opened %d blocked %d, want 1 and 1", got.TunnelOpened, got.TunnelBlocked)
	}
	if got.Requests != 0 {
		t.Errorf("Requests = %d after two tunnels and no provider call, want 0", got.Requests)
	}
}

// TestConnectRefusesProvidersWhileReplaying: `replay` says of itself that no
// provider will be contacted. A tunnel is the one way a client could reach one
// through cs-vcr anyway, so an offline session refuses the configured providers
// on top of the default list.
func TestConnectRefusesProvidersWhileReplaying(t *testing.T) {
	srv := connectServer(t, offline)
	// Configured, and not on the default list: only the offline rule stops it.
	code, conn, _ := connectTo(t, srv.Listener.Addr().String(), "api.openai.com:443")
	if conn != nil {
		conn.Close()
	}
	if code != http.StatusForbidden {
		t.Errorf("replaying, CONNECT api.openai.com = %d, want %d", code, http.StatusForbidden)
	}

	// Recording, the same host is a provider this session may legitimately
	// reach, so only the offline rule refuses it.
	rec := New(&config.Config{Providers: map[string]*config.Provider{
		"openai": {BaseURL: "https://api.openai.com"},
	}}, slog.New(slog.DiscardHandler), online)
	if why := rec.tunnelRefusal("api.openai.com"); why != "" {
		t.Errorf("recording, api.openai.com refused: %s", why)
	}
}
