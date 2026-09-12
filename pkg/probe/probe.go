// Package probe runs synthetic HTTP probes against a configured set of
// target URLs and produces protocol.ProbeResult values that the agent can
// attach to each heartbeat. The control plane treats probes as opaque
// observations: it stores results, surfaces them on the public status page
// and raises an incident when a probe flaps from OK to failing.
//
// Design constraints:
//   - One Runner per agent; share via Run().
//   - HTTP only. TCP / DNS / ICMP probes are out of scope for now; add
//     tagged kinds once an operator actually asks for them.
//   - Bounded latency: every probe must complete within `Timeout`, even if
//     the server hangs mid-response. Without that bound a slow upstream
//     stalls the agent's heartbeat loop.
//   - No redirects followed by default: a 3xx is a probe outcome, not a
//     silent retry. Set FollowRedirects to opt into the classic client
//     behavior.
//   - Single shared HTTP transport to keep-alive reuse and avoid the cost
//     of TLS handshakes on every probe (and to avoid leaking sockets when
//     the agent runs for weeks).
package probe

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// Runner executes HTTP probes against a fixed set of targets. The target
// list is set once at construction (see NewRunner) and reused for every
// Run() call; operators edit targets by restarting the agent with a new
// -probe-urls flag.
type Runner struct {
	targets        []string
	timeout        time.Duration
	followRedirect bool
	client         *http.Client
}

// NewRunner builds a Runner that will probe each URL in `targets`. Empty
// entries are filtered out. Timeout is the per-probe wall-clock budget;
// zero or negative falls back to a sane default (5s). Passing the same
// URL twice is harmless: the second copy just produces duplicate results,
// which is fine for the public status page (it groups by URL anyway).
func NewRunner(targets []string, timeout time.Duration, followRedirect bool) *Runner {
	clean := make([]string, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, raw := range targets {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		clean = append(clean, t)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	return &Runner{
		targets:        clean,
		timeout:        timeout,
		followRedirect: followRedirect,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if !followRedirect {
					return http.ErrUseLastResponse
				}
				if len(via) >= 5 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}
}

// Targets returns the normalized target list (after dedup + trim).
func (r *Runner) Targets() []string {
	out := make([]string, len(r.targets))
	copy(out, r.targets)
	return out
}

// Run executes every configured target in parallel and returns the
// observations in input order. Failures of one target never abort the
// batch; each ProbeResult carries its own OK flag and Error string so the
// caller can ship them all in one heartbeat.
//
// ponytail: parallelism is bounded by len(targets). For typical configs
// (≤10 targets) spawning one goroutine each is fine; switch to a worker
// pool if operators start listing hundreds of URLs.
func (r *Runner) Run() []protocol.ProbeResult {
	if len(r.targets) == 0 {
		return nil
	}
	now := time.Now().Unix()
	results := make([]protocol.ProbeResult, len(r.targets))
	var wg sync.WaitGroup
	wg.Add(len(r.targets))
	for i, target := range r.targets {
		go func(i int, target string) {
			defer wg.Done()
			results[i] = r.runOne(target, now)
		}(i, target)
	}
	wg.Wait()
	return results
}

func (r *Runner) runOne(target string, ts int64) protocol.ProbeResult {
	res := protocol.ProbeResult{URL: target, Ts: ts}

	// Validate the URL once, up front, so a typo in the agent config
	// doesn't produce a flood of generic "connection refused" results.
	if _, err := url.ParseRequestURI(target); err != nil {
		res.Error = "invalid_url: " + err.Error()
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res.Error = "request_build: " + err.Error()
		return res
	}
	// Pretend to be a normal browser so upstreams that vary content on
	// User-Agent don't 403 a synthetic probe. No cookies, no referer, no
	// accept headers — keep the probe noise minimal.
	req.Header.Set("User-Agent", "NodePulse-Probe/1.0")

	start := time.Now()
	resp, err := r.client.Do(req)
	elapsed := time.Since(start)
	res.LatencyMs = elapsed.Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.StatusCode = resp.StatusCode
	// A probe is "OK" iff the server returned a 2xx status. 3xx is
	// already not-OK when followRedirect is off (we'd see the redirect
	// itself); when followRedirect is on we only land here on the final
	// response, which may legitimately be 2xx.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.OK = true
	} else {
		res.Error = http.StatusText(resp.StatusCode)
	}
	return res
}
