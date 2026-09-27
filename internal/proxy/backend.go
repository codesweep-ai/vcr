package proxy

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// A vendor's own backend: the calls a client makes beside its model calls,
// which no base URL for the model governs.
//
// Codex 0.156 and later will not start on a ChatGPT login until a workspace
// routing discovery succeeds: GET {chatgpt_base_url}/wham/accounts/check, with
// the token it holds, answered with its own account. Left at chatgpt.com, that
// call arrives here as a CONNECT and is refused (R32), so the client exits. Let
// through, it would answer for a real login and 401 for a fabricated one, and
// a recording could not be replayed.
//
// So a client pointed here for that backend is answered by cs-vcr itself, the
// same in a recording session and a replaying one, and nothing is forwarded or
// recorded (R33a). The discovery gets the one answer a client can start on.
// Every other call there is refused, as it was at the tunnel, because what it
// returns (usage, settings, plugins) would otherwise be a step no replay could
// rebuild.
//
// The answer names the account the request itself named, in the header Codex
// sends with every call, so it holds for a real login and a fabricated one
// alike. It names a concrete HTTPS origin, and it has to. The real answer for
// an account with no residency rule is NO_CONSTRAINT, which Codex resolves to
// the origin of chatgpt_base_url and requires to be HTTPS, and cs-vcr is plain
// HTTP. Codex applies the origin only to a model provider on the backend's own
// origin, so the client has to reach this under a name its model base URL does
// not use.

// backendPrefix is where a client's calls to its vendor's backend arrive. A
// model call carries the /c/ prefix, so none starts with it.
const backendPrefix = "/backend-api"

// discoveryPath is the one backend call a Codex needs to succeed.
const discoveryPath = backendPrefix + "/wham/accounts/check"

// discoveredOrigin is the workspace origin the answer names: what
// NO_CONSTRAINT resolves to for the same account against chatgpt.com.
const discoveredOrigin = "https://chatgpt.com"

// accountHeader is the header Codex names its ChatGPT account in.
const accountHeader = "ChatGPT-Account-ID"

type accountsCheck struct {
	Accounts        []accountEntry `json:"accounts"`
	AccountOrdering []string       `json:"account_ordering"`
	DefaultAccount  string         `json:"default_account_id"`
}

type accountEntry struct {
	ID        string `json:"id"`
	Origin    string `json:"workspace_backend_origin"`
	Override  string `json:"account_routing_override"`
	Structure string `json:"structure"`
}

// isBackend reports whether a request is a call to a vendor's backend.
func isBackend(path string) bool {
	return path == backendPrefix || strings.HasPrefix(path, backendPrefix+"/")
}

// serveBackend answers a backend call here, or refuses it. Nothing is
// forwarded, recorded or matched against a cassette.
func (s *Server) serveBackend(w http.ResponseWriter, r *http.Request) {
	attrs := []any{slog.String("method", r.Method), slog.String("path", r.URL.Path)}
	if r.Method != http.MethodGet || r.URL.Path != discoveryPath {
		s.count(func(st *Stats) { st.BackendRefused++ })
		s.log.Info("backend call refused", attrs...)
		writeError(w, http.StatusForbidden, "backend_refused",
			"cs-vcr answers a client's workspace discovery and nothing else of its vendor's backend: "+
				"what the rest returns would be a step no replay could rebuild")
		return
	}
	account := r.Header.Get(accountHeader)
	if account == "" {
		s.count(func(st *Stats) { st.BackendRefused++ })
		s.log.Warn("backend call named no account", attrs...)
		writeError(w, http.StatusBadRequest, "no_account",
			"a workspace discovery has to name its account in "+accountHeader+", which is the account the answer lists")
		return
	}
	s.count(func(st *Stats) { st.BackendAnswered++ })
	s.log.Info("answered a workspace discovery", attrs...)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(accountsCheck{
		Accounts: []accountEntry{{
			ID: account, Origin: discoveredOrigin, Override: "NO_CONSTRAINT", Structure: "personal",
		}},
		AccountOrdering: []string{account},
		DefaultAccount:  account,
	})
}
