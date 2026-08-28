package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/codesweep-ai/vcr/internal/config"
)

// proxy is a cs-vcr process the suite starts, drives an agent at, and stops.
//
// The binary is built from this checkout rather than taken from the PATH: the
// suite is a test of this build, and an installed cs-vcr from last week would
// answer a question nobody asked.
type proxy struct {
	cmd     *exec.Cmd
	out     *bytes.Buffer
	port    int
	admin   int
	summary summary
	err     error
	done    chan struct{}
}

// summary is the accounting cs-vcr prints on the way out, which is the artifact
// a CI log shows and the thing this suite asserts on.
type summary struct {
	Requests int
	Replayed int
	Recorded int
	Upstream int
	Misses   int
}

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// binary builds cs-vcr once per test run and returns its path.
func binary() (string, error) {
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cs-vcr-agents-bin")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "cs-vcr")
		// This tier drives the real binary rather than calling into it, so what
		// the proxy executes counts towards coverage only when it is built
		// instrumented and told where to write. Without this the whole live tier
		// contributes nothing and the packages only it reaches read as dead.
		//
		// CS_COVERDIR is set by the Makefile's test targets. It carries the path
		// rather than GOCOVERDIR because `go test` overwrites GOCOVERDIR in the
		// test process with a directory of its own and does not fold what lands
		// there back into the profile. Setting GOCOVERDIR here, after that, is
		// what points the proxy at the tier directory: startProxy builds its
		// environment from os.Environ(), so it is inherited with no further
		// wiring. An instrumented binary writes its counters as it exits, which
		// is why stop() asks with SIGINT and only escalates to Kill on a
		// timeout — a killed proxy still ends the test, it just contributes no
		// coverage for that run.
		build := []string{"build", "-o", buildPath, "github.com/codesweep-ai/vcr/cmd/cs-vcr"}
		if coverDir := os.Getenv("CS_COVERDIR"); coverDir != "" {
			build = append([]string{"build", "-cover", "-covermode=atomic",
				"-coverpkg=github.com/codesweep-ai/vcr/..."}, build[1:]...)
			_ = os.Setenv("GOCOVERDIR", coverDir)
		}
		cmd := exec.Command("go", build...)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building cs-vcr: %v\n%s", err, out)
		}
	})
	return buildPath, buildErr
}

// startProxy runs `cs-vcr record` or `cs-vcr replay` and waits for it to answer
// on its admin port. An empty configPath passes no --config, leaving the
// session to whatever configuration the machine holds.
//
// Waiting is not politeness: the agent is started immediately afterwards, and an
// agent that reaches a port nothing is listening on yet reports a network error
// rather than a missing recording.
func startProxy(ctx context.Context, ws *workspace, cassettes, configPath string, offline bool, missDir string) (*proxy, error) {
	bin, err := binary()
	if err != nil {
		return nil, err
	}
	port, admin, err := freePorts()
	if err != nil {
		return nil, err
	}
	mode := "record"
	if offline {
		mode = "replay"
	}
	// The agent's base URL carries a /c/<provider>/<scenario> prefix, which is
	// the only way a request names its upstream and its cassette: one that
	// arrived without it would be refused, which is what makes this an
	// assertion rather than a configuration.
	//
	// An empty configPath means no --config at all, which is the replay half:
	// it reads no provider configuration, so it has nothing to be given and
	// runs on whatever config the machine has, including none.
	var args []string
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, mode,
		"--cassettes", cassettes,
		"--listen", "127.0.0.1:"+strconv.Itoa(port),
		"--admin", "127.0.0.1:"+strconv.Itoa(admin))
	if missDir != "" {
		args = append(args, "--dump-misses", missDir)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// The proxy's own environment, which is NOT the agent's: it is the process
	// that must reach the provider while recording, so it gets no dead proxy —
	// and it is the process that decides which checkout path is <ROOT>.
	cmd.Env = append(os.Environ(), "VCR_ROOT="+ws.root)
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	// Its own process group, so the SIGINT that asks it to finish writing the
	// cassette does not go to the whole test run.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &proxy{cmd: cmd, out: out, port: port, admin: admin, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	if err := p.waitReady(); err != nil {
		_ = p.stop()
		return nil, fmt.Errorf("%s: %w\n%s", mode, err, out)
	}
	return p, nil
}

// baseURL is what an agent is pointed at: this proxy, the prefix naming the
// cassette its traffic belongs to, and whatever path fragment the client
// expects to be given. Where the prefix sits relative to that fragment is the
// thing this suite is proving, so it is composed here and nowhere else.
// origin is the proxy without any cassette path: what HTTP_PROXY takes.
func (p *proxy) origin() string {
	return "http://127.0.0.1:" + strconv.Itoa(p.port)
}

func (p *proxy) baseURL(provider, cassette, suffix string) string {
	return "http://127.0.0.1:" + strconv.Itoa(p.port) +
		config.Prefix + provider + "/" + cassette + suffix
}

func (p *proxy) waitReady() error {
	url := "http://127.0.0.1:" + strconv.Itoa(p.admin) + "/healthz"
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return errors.New("cs-vcr exited before it was ready")
		default:
		}
		resp, err := http.Get(url) //nolint:gosec // a loopback address this test just chose
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("cs-vcr did not answer /healthz")
}

// stop asks the session to finish and reads the summary it prints.
//
// SIGINT rather than a kill, because a recording session writes each step when
// its response is complete: killed instead, the last turn of the session — the
// one that says the agent finished — is missing from the cassette.
func (p *proxy) stop() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGINT)
	}
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-p.done
	}
	p.summary = parseSummary(p.out.String())
	// A replay session with a miss exits 4, which is the whole point of it, so
	// the error is carried rather than swallowed and the caller reports it
	// beside the summary.
	return p.err
}

func (p *proxy) log() string { return p.out.String() }

// parseSummary reads the counters out of the printed summary.
//
// Printed prose is the only machine-readable form there is, so this is coupled
// to it deliberately and the tests assert on the numbers rather than the words.
func parseSummary(s string) summary {
	num := func(label string) int {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(label) + `\s+(\d+)\s*$`)
		if m := re.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return -1
	}
	return summary{
		Requests: num("requests"),
		Replayed: num("replayed"),
		Recorded: num("recorded"),
		Upstream: num("upstream calls"),
		Misses:   num("misses"),
	}
}

// freePorts asks the kernel for two ports nothing is using. Two proxies never
// run at once here, but a developer's own cs-vcr on the default port is common
// enough that hard-coding 8080 would fail the suite for a reason unrelated to it.
func freePorts() (int, int, error) {
	var ports []int
	for range 2 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, 0, err
		}
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
		if err := l.Close(); err != nil {
			return 0, 0, err
		}
	}
	return ports[0], ports[1], nil
}

// runCassetteCmd drives one of cs-vcr's offline commands — `cassette scrub`,
// `cassette verify` — against the fixtures.
func runCassetteCmd(cassettes string, args ...string) (string, error) {
	bin, err := binary()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "CS_VCR_CASSETTES="+cassettes)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
