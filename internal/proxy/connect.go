package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// CONNECT tunnelling: the half of an agent's traffic that a base URL does not
// govern.
//
// Pointing an agent at cs-vcr aims its MODEL calls here and nothing else. These
// clients also talk to hosts of their own choosing — Claude Code checks its
// OAuth session against api.anthropic.com, Codex reaches chatgpt.com for its
// subscription transport and ab.chatgpt.com for experiment assignment — and
// they do it whatever ANTHROPIC_BASE_URL or a provider block says. What comes
// back changes the prompt: with a real credential those calls answer, with a
// fabricated one they 401, and the request the agent then sends is not the one
// the cassette holds.
//
// So HTTP_PROXY points here too, and this refuses exactly those hosts while
// tunnelling everything else. Refused identically whether recording or
// replaying, the two runs ask the same question — which is what makes a
// recording made with a real login replay under a fabricated one.
//
// Everything else is tunnelled rather than blocked, because an agent's TOOLS
// share its environment: a blanket refusal takes away `git`, `curl` and every
// package manager the agent might shell out to, and none of those change the
// prompt.
//
// No certificate is involved. A CONNECT proxy pipes bytes; TLS stays end to end
// between the client and the host it dialled, and the hostname this decides on
// is the one in the CONNECT line, in the clear, before any of that starts.

// defaultBlockedHosts is the refusal list. Short on purpose: it holds the hosts
// these agents phone home to on their own, and nothing else. github.com is
// deliberately absent — Codex reaches it, but an agent's tools reach it far
// more, and nothing it returns has been seen to change a prompt.
var defaultBlockedHosts = []string{
	"api.anthropic.com",  // Claude Code's OAuth session check
	"chatgpt.com",        // Codex's subscription transport
	"ab.chatgpt.com",     // Codex's experiment assignment
	"models.opencode.ai", // OpenCode's model catalog
}

// tunnelRefusal reports why a host may not be tunnelled, or "" to allow it.
//
// The configured providers are refused too, in BOTH modes. Replay needs it to
// keep its own promise: a session that says no provider will be contacted, and
// then carries a client to one, has told the truth about the forward path and
// not about this one.
//
// Recording needs it for a reason of its own. cs-vcr reaches a provider on the
// forward path, where it records what passes. A CONNECT to that same host is a
// model call going AROUND the recorder: bytes are piped, nothing is stored, and
// the cassette comes out missing a request the client will make again on
// replay. Refusing it is what makes the recording complete.
//
// Refusing in both is also the rule the rest of this file rests on. Two of the
// four hosts above are providers already, so the split refused api.anthropic.com
// while recording and let api.openai.com through, which is a line nothing draws
// on purpose.
func (s *Server) tunnelRefusal(host string) string {
	if slices.Contains(defaultBlockedHosts, host) {
		return "cs-vcr does not tunnel " + host + ": it is one of the hosts an agent contacts on its own, and what it answers changes the prompt"
	}
	for name, p := range s.cfg.Providers {
		if p == nil || p.BaseURL == "" {
			continue
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" {
			continue
		}
		if strings.EqualFold(hostOnly(u.Host), host) {
			return "cs-vcr does not tunnel to the " + name + " provider: a model call has to arrive on the base URL, where it can be recorded and replayed"
		}
	}
	return ""
}

// hostOnly drops a port and a trailing dot, so "API.Example.com.:443" and
// "api.example.com" are the same host.
func hostOnly(hostport string) string {
	h := hostport
	if x, _, err := net.SplitHostPort(hostport); err == nil {
		h = x
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// serveConnect answers a CONNECT: refuse the listed hosts, tunnel the rest.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host // CONNECT carries authority-form: host:port
	host := hostOnly(target)
	// Counted by the two tunnel counters and by nothing else. Requests means
	// requests to a provider surface — the things a session can record — and the
	// recording half asserts that every one of them was recorded. A tunnel
	// records nothing, so counting one here fails that assertion for every agent
	// that reaches past its base URL.
	if why := s.tunnelRefusal(host); why != "" {
		s.count(func(st *Stats) { st.TunnelBlocked++ })
		s.log.Info("tunnel refused", slog.String("host", host))
		writeError(w, http.StatusForbidden, "tunnel_blocked", why)
		return
	}

	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		s.log.Warn("tunnel could not be opened", slog.String("host", host), slog.Any("err", err))
		writeError(w, http.StatusBadGateway, "tunnel_failed", err.Error())
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "tunnel_unsupported", "this server cannot hijack a connection")
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	s.count(func(st *Stats) { st.TunnelOpened++ })

	// Both directions, and the first to end finishes the tunnel: a peer that
	// closed has nothing more to say, and the deferred closes free the other.
	// buf rather than client for the read side — the client may already have
	// sent bytes that landed in the server's buffer.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, buf); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
