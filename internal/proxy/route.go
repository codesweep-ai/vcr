package proxy

import (
	"net/http"
	"strings"
)

// Surface is a request shape the proxy understands well enough to key on. It
// is deliberately a short list: cs-vcr is a narrow proxy, knowing
// just enough of each shape to canonicalize and hash it, and passing the rest
// through untouched.
type Surface string

const (
	SurfaceAnthropicMessages Surface = "anthropic.messages"
	SurfaceOpenAIResponses   Surface = "openai.responses"
	SurfaceOpenAIChat        Surface = "openai.chat"
	// SurfaceUnknown is a path cs-vcr does not model. It is still proxied,
	// recorded and replayed — a request that is proxied but not recorded is one
	// replay can never serve — just with no model and no surface to key on.
	SurfaceUnknown Surface = "unknown"
)

// Recordable reports whether this surface is one cs-vcr knows how to key.
func (s Surface) Recordable() bool { return s != SurfaceUnknown }

// Route is which upstream a request is going to, and which shape it is.
type Route struct {
	Provider string // key into Config.Providers
	Surface  Surface
}

// classify routes a request by path, falling back to which auth header is
// present when the path is one cs-vcr does not model.
//
// Path is the stronger signal and is tried first: `/v1/messages` is the
// Anthropic surface whether the caller is Claude Code or OpenCode pointed at
// Anthropic, which is exactly the case not to assume away.
//
// The glance at the auth header reads only WHICH header is present, never its
// value. It is routing, not credential handling: cs-vcr does not validate,
// store, redact or replace anything a client sends.
func classify(r *http.Request, defaultProvider string) Route {
	switch apiPath(r.URL.Path) {
	case "/messages":
		return Route{Provider: "anthropic", Surface: SurfaceAnthropicMessages}
	case "/responses":
		return Route{Provider: "openai", Surface: SurfaceOpenAIResponses}
	case "/chat/completions":
		// OpenAI-compatible, which is also what OpenCode Zen and every local
		// model server speak. The provider is whichever upstream is configured
		// for the OpenAI shape, not necessarily OpenAI itself.
		return Route{Provider: "openai", Surface: SurfaceOpenAIChat}
	}
	// Unrecognized path: it goes where the configuration says, and nowhere
	// else. There is no header that reliably identifies a provider — what
	// identified this request was the base URL the client was pointed at, and
	// origin mode threw that away by serving every provider on one listener.
	//
	// Guessing does not work here. Reading a bearer token as an OpenAI client
	// is wrong for Claude Code with a Pro/Max subscription, which sends
	// `Authorization: Bearer` too, and its `HEAD /api/hello` startup probe
	// sends no identifying header at all — not anthropic-version, not
	// x-api-key, and `User-Agent: Bun/1.4.0` from the runtime's own fetch. A
	// guess that sends one client's traffic to another provider is not worth
	// the convenience of not writing a config line.
	//
	// One check survives, because it cannot misroute anything: anthropic-version
	// is Anthropic's own API-versioning header, so a request carrying it is
	// Anthropic's by definition. It can only keep a mixed deployment — an
	// Anthropic client and an OpenAI one through the same listener — from
	// sending the wrong request to the default. There is no OpenAI header with
	// the same property, which is why the reverse is not attempted.
	if r.Header.Get("anthropic-version") != "" || r.Header.Get("x-api-key") != "" {
		return Route{Provider: "anthropic", Surface: SurfaceUnknown}
	}
	return Route{Provider: defaultProvider, Surface: SurfaceUnknown}
}

// apiPath is the path a surface is recognized by: the request path without the
// version prefix, which is the client's to choose and not part of the shape.
//
// It is not always there. `/v1` sits in the base URL a client is pointed at,
// and where the provider's own path has none the client's does not either:
// Codex signed in with ChatGPT talks to `chatgpt.com/backend-api/codex`, whose
// endpoint is `/responses`. Matching only the versioned spelling recorded that
// whole session as an unrecognized surface — proxied and replayed, but with no
// model, no surface, and a miss diagnostic that could not say what it was.
func apiPath(p string) string {
	p = strings.TrimSuffix(p, "/")
	if rest, ok := strings.CutPrefix(p, "/v1/"); ok {
		return "/" + rest
	}
	return p
}
