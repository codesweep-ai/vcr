// Package agents is the live agent suite: real Claude Code, Codex and OpenCode
// sessions recorded through cs-vcr, and replayed against the cassettes those
// recordings produced.
//
// It is the test for the claim on the front page. The Go suite under internal/
// drives the proxy against bodies the tests themselves wrote, so it proves the
// format, the framing and the alignment rules — and it cannot prove that a real
// agent's real session replays, because no hand-written fixture has a real
// agent's prompt in it. This one runs the agents.
//
// Two halves, and only the first needs a provider:
//
//	make fixtures          record: real credentials, real providers, costs money
//	make test-integration  replay: fabricated credentials, no provider reachable
//
// The fixtures are committed, so CI runs the second half on every push. Both
// halves skip what this host cannot do — an agent that is not installed, a
// credential that is not present — and say which, because a suite that fails
// for want of a login teaches contributors to ignore it.
//
// # What makes a real session replayable
//
// An agent builds its prompt from what it can see, so the two runs have to see
// the same things. Three mechanisms, in the order they matter:
//
//  1. The agent is denied every network path except cs-vcr. Claude Code asks a
//     profile endpoint who is signed in, and Codex asks for the connectors and
//     plugins the account has: with a real credential those calls answer, with a
//     fabricated one they 401, and the prompt differs by whole blocks. Blocked
//     in both halves, the two runs ask the same question. It also means the
//     recording half cannot rotate the developer's OAuth token.
//  2. Each run gets a fresh HOME and a fresh agent config directory, seeded with
//     the same fixed identity. Nothing carries over from the developer's own
//     configuration, and nothing a first run cached changes what a second sends.
//  3. Each run is given the same explicit environment and the same flags, so
//     what the agent reports about its machine is the same sentence.
//
// What is left over is what the shipped normalization ruleset is for: the date,
// the checkout path, the kernel release, the per-session identifiers.
package agents
