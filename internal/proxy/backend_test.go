package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func backendCall(t *testing.T, base, method, path, account string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, base+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer a-real-or-fabricated-login")
	if account != "" {
		req.Header.Set(accountHeader, account)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// Codex 0.156 and later will not start on a ChatGPT login until this call
// succeeds (R33a). cs-vcr answers it itself, the same recording as replaying,
// with the account the request named and an HTTPS origin, which Codex requires
// where NO_CONSTRAINT would resolve to this plain-HTTP listener. Nothing is
// forwarded, recorded or counted as a request, so a recording made under a real
// login replays under a fabricated one.
func TestAWorkspaceDiscoveryIsAnsweredInBothModes(t *testing.T) {
	for _, mode := range []struct {
		name string
		off  bool
	}{{"recording", online}, {"replaying", offline}} {
		t.Run(mode.name, func(t *testing.T) {
			srv := connectServer(t, mode.off)
			code, body := backendCall(t, srv.URL, http.MethodGet, discoveryPath, "acct-1")
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			var got accountsCheck
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("not the answer Codex reads: %v\n%s", err, body)
			}
			if len(got.Accounts) != 1 || got.Accounts[0].ID != "acct-1" || got.DefaultAccount != "acct-1" ||
				len(got.AccountOrdering) != 1 || got.AccountOrdering[0] != "acct-1" {
				t.Errorf("the answer names %+v, want the account the request named throughout", got)
			}
			if a := got.Accounts[0]; a.Origin != "https://chatgpt.com" || a.Override != "NO_CONSTRAINT" {
				t.Errorf("origin %q, override %q: Codex needs an HTTPS origin and no routing override", a.Origin, a.Override)
			}
			st := srv.Config.Handler.(*Server).Snapshot()
			if st.BackendAnswered != 1 || st.Requests != 0 || st.Upstream != 0 || st.Misses != 0 {
				t.Errorf("counted %+v, want one backend call answered and nothing else", st)
			}
			// Nothing was recorded: the cassette store is still empty.
			entries, _ := os.ReadDir(srv.Config.Handler.(*Server).cfg.Cassettes)
			if len(entries) != 0 {
				t.Errorf("a backend call left %d entries in the store", len(entries))
			}
		})
	}
}

// The rest of the backend is refused, as it was at the tunnel: what it returns
// would be a step no replay could rebuild.
func TestTheRestOfABackendIsRefused(t *testing.T) {
	srv := connectServer(t, online)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, backendPrefix + "/wham/usage"},
		{http.MethodGet, backendPrefix + "/ps/plugins/installed"},
		{http.MethodPost, discoveryPath},
		{http.MethodGet, backendPrefix},
	} {
		code, body := backendCall(t, srv.URL, c.method, c.path, "acct-1")
		if code != http.StatusForbidden || !strings.Contains(string(body), "backend_refused") {
			t.Errorf("%s %s: status %d: %s", c.method, c.path, code, body)
		}
	}
	if st := srv.Config.Handler.(*Server).Snapshot(); st.BackendRefused != 4 || st.Requests != 0 || st.Upstream != 0 {
		t.Errorf("counted %+v, want four refused and nothing else", st)
	}
}

// A discovery that names no account cannot be answered for, and is not
// answered with a made-up one.
func TestADiscoveryWithNoAccountIsRefused(t *testing.T) {
	srv := connectServer(t, offline)
	code, body := backendCall(t, srv.URL, http.MethodGet, discoveryPath, "")
	if code != http.StatusBadRequest || !strings.Contains(string(body), "no_account") {
		t.Errorf("status %d: %s", code, body)
	}
}

// A model call under a cassette prefix is routed as before, even when the
// provider's own path contains the backend's.
func TestAPrefixedCallIsNotABackendCall(t *testing.T) {
	srv := connectServer(t, offline)
	code, body := backendCall(t, srv.URL, http.MethodPost, "/c/openai/demo"+discoveryPath, "acct-1")
	if code == http.StatusOK || strings.Contains(string(body), "backend_") {
		t.Errorf("a prefixed call was taken for a backend call: status %d: %s", code, body)
	}
}
