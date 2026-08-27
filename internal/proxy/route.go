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
	Provider string // key into Config.Providers, as the base URL named it
	Surface  Surface
}

// routeFor is which shape a request is and, for a session that forwards, which
// upstream it goes to.
//
// The provider is the one the base URL named, for every path on the prefix. A
// client is configured with one base URL per provider, so the URL has already
// answered the question — including for the bodiless startup probes, which
// carry the prefix and nothing a guess could have read.
//
// A replay session asks only the shape, and the provider segment goes unread.
// It reads no provider configuration at all, which is what lets a recorded
// session replay with none configured: the step it serves was chosen by the
// cassette and the request, and there is no upstream in the picture to name.
func (s *Server) routeFor(r *http.Request, provider string) Route {
	if !s.ReachesUpstream() {
		return Route{Surface: surfaceOf(r)}
	}
	return Route{Provider: provider, Surface: surfaceOf(r)}
}

// surfaceOf is the API shape a request belongs to, which is a property of its
// path and of nothing else.
//
// Separate from the provider because replay wants one and not the other: it
// reports by surface, and a surface read off the path costs it no configuration.
func surfaceOf(r *http.Request) Surface {
	switch apiPath(r.URL.Path) {
	case "/messages":
		return SurfaceAnthropicMessages
	case "/responses":
		return SurfaceOpenAIResponses
	case "/chat/completions":
		return SurfaceOpenAIChat
	}
	return SurfaceUnknown
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
