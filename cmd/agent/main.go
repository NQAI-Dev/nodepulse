package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/autoheal"
	"github.com/NQAI-Dev/nodepulse/pkg/collector"
	"github.com/NQAI-Dev/nodepulse/pkg/policy"
	"github.com/NQAI-Dev/nodepulse/pkg/probe"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func main() {
	nodeID := flag.String("node", "", "Unique Node ID")
	serverURL := flag.String("server", "http://127.0.0.1:8080/api/v1/ingest", "NodePulse Central URL")
	token := flag.String("token", "", "NodePulse Ingest API Token")
	interval := flag.Duration("interval", 10*time.Second, "Heartbeat interval")
	unitsFlag := flag.String("units", "", "Comma-separated systemd units to monitor")
	tagsFlag := flag.String("tags", "", "Comma-separated node tags, e.g. env=prod,region=eu,role=db")
	dryRun := flag.Bool("dry-run", false, "Collect and print without network push")
	probeURLs := flag.String("probe-urls", "", "Comma-separated HTTP URLs to probe on every heartbeat (e.g. https://api.example.com/health)")
	probeTCP := flag.String("probe-tcp", "", "Comma-separated TCP targets host:port[=banner] to probe on every heartbeat (e.g. db:5432=postgres,redis:6379)")
	probeTLS := flag.String("probe-tls", "", "Comma-separated TLS handshake targets host:port[=Nd[:1.2|1.3[:insecure]]] to probe on every heartbeat (e.g. api.example.com:443=30d:1.3)")
	probeDNS := flag.String("probe-dns", "", "Comma-separated DNS targets host[=substring|:TYPE[=substring]] to probe on every heartbeat (e.g. internal.svc=10.0.,api.example.com:AAAA=2001:db8)")
	probeICMP := flag.String("probe-icmp", "", "Comma-separated ICMP echo targets host[=N] to probe on every heartbeat (e.g. 1.1.1.1=4,gateway=2). Requires CAP_NET_RAW or net.ipv4.ping_group_range on the host.")
	probeTimeout := flag.Duration("probe-timeout", 5*time.Second, "Per-probe wall-clock timeout")
	probeFollowRedirect := flag.Bool("probe-follow-redirect", false, "Follow HTTP 3xx redirects during probes (off by default)")
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("NODEPULSE_TOKEN")
		if *token == "" {
			log.Fatalf("no API token provided — pass -token=<your-api-token> or set NODEPULSE_TOKEN (get one by logging in at https://pulse.nqai.es-cloud.ru/)")
		}
	}

	if *nodeID == "" {
		*nodeID = os.Getenv("NODEPULSE_NODE_ID")
		if *nodeID == "" {
			hostname, _ := os.Hostname()
			*nodeID = hostname
		}
	}

	units := []string{}
	rawUnits := *unitsFlag
	if rawUnits == "" {
		rawUnits = os.Getenv("NODEPULSE_SYSTEMD_UNITS")
	}
	if rawUnits != "" {
		for _, u := range strings.Split(rawUnits, ",") {
			if trimmed := strings.TrimSpace(u); trimmed != "" {
				units = append(units, trimmed)
			}
		}
	}

	c := collector.New(*nodeID, units).WithTags(collector.TagsFromEnv(*tagsFlag))
	client := &http.Client{Timeout: 5 * time.Second}
	// Adaptive remediation: classify each incoming command by its target's
	// image/name/labels and pick a strategy (db / cache / stateless / ...)
	// instead of one-size-fits-all. The breaker defaults to the legacy
	// behaviour and switches to the per-class strategy the first time it
	// sees a key. Once assigned, the class is sticky for the life of the
	// agent: flipping a Postgres pod to the Stateless class mid-incident
	// would be a great way to hammer its WAL.
	classifyCmd := classifyCommandFunc(c)
	breakers := newStrategyBreakers()

	probeTargets := splitCSV(*probeURLs, os.Getenv("NODEPULSE_PROBE_URLS"))
	prober := probe.NewRunner(probeTargets, *probeTimeout, *probeFollowRedirect)
	if len(probeTargets) > 0 {
		log.Printf("Synthetic probes enabled: %d target(s), timeout=%s, follow_redirect=%v",
			len(prober.Targets()), *probeTimeout, *probeFollowRedirect)
	}

	tcpRaw := *probeTCP
	if tcpRaw == "" {
		tcpRaw = os.Getenv("NODEPULSE_PROBE_TCP")
	}
	tcpTargets, err := probe.ParseTCPTargets(tcpRaw)
	if err != nil {
		log.Fatalf("Invalid -probe-tcp value: %v", err)
	}
	tcpRunner := probe.NewTCPRunner(tcpTargets, *probeTimeout)
	if len(tcpTargets) > 0 {
		log.Printf("TCP probes enabled: %d target(s), timeout=%s",
			len(tcpRunner.Targets()), *probeTimeout)
	}

	tlsRaw := *probeTLS
	if tlsRaw == "" {
		tlsRaw = os.Getenv("NODEPULSE_PROBE_TLS")
	}
	tlsTargets, err := probe.ParseTLSTargets(tlsRaw)
	if err != nil {
		log.Fatalf("Invalid -probe-tls value: %v", err)
	}
	tlsRunner := probe.NewTLSRunner(tlsTargets, *probeTimeout)
	if len(tlsTargets) > 0 {
		log.Printf("TLS probes enabled: %d target(s), timeout=%s",
			len(tlsRunner.Targets()), *probeTimeout)
	}

	dnsRaw := *probeDNS
	if dnsRaw == "" {
		dnsRaw = os.Getenv("NODEPULSE_PROBE_DNS")
	}
	dnsTargets, err := probe.ParseDNSTargets(dnsRaw)
	if err != nil {
		log.Fatalf("Invalid -probe-dns value: %v", err)
	}
	dnsRunner := probe.NewDNSRunner(dnsTargets, *probeTimeout)
	if len(dnsTargets) > 0 {
		log.Printf("DNS probes enabled: %d target(s), timeout=%s",
			len(dnsRunner.Targets()), *probeTimeout)
	}

	icmpRaw := *probeICMP
	if icmpRaw == "" {
		icmpRaw = os.Getenv("NODEPULSE_PROBE_ICMP")
	}
	icmpTargets, err := probe.ParseICMPTargets(icmpRaw)
	if err != nil {
		log.Fatalf("Invalid -probe-icmp value: %v", err)
	}
	icmpRunner := probe.NewICMPRunner(icmpTargets, *probeTimeout)
	if len(icmpTargets) > 0 {
		log.Printf("ICMP probes enabled: %d target(s), timeout=%s (needs CAP_NET_RAW or net.ipv4.ping_group_range)",
			len(icmpRunner.Targets()), *probeTimeout)
	}

	var pendingMu sync.Mutex
	pending := []protocol.AutoHealLog{}

	flushLogs := func() {
		pendingMu.Lock()
		if len(pending) == 0 {
			pendingMu.Unlock()
			return
		}
		batch := pending
		pending = nil
		pendingMu.Unlock()

		base := *serverURL
		if i := strings.Index(base, "/api/v1/"); i >= 0 {
			base = base[:i]
		}
		uploadURL := base + "/api/v1/autoheal/log"

		body, err := json.Marshal(map[string]interface{}{
			"node_id": *nodeID,
			"events":  batch,
		})
		if err != nil {
			return
		}
		req, err := http.NewRequest(http.MethodPost, uploadURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("X-NodePulse-Token", *token)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}

	log.Printf("NodePulse Edge Agent active. ID: %s, Destination: %s, Cadence: %v, Monitored units: %d", *nodeID, *serverURL, *interval, len(units))

	for {
		hb, err := c.Collect()
		if err != nil {
			log.Printf("Collection failure: %v", err)
			time.Sleep(*interval)
			continue
		}

		// Attach synthetic probe results to the heartbeat when probes are
		// configured. We run them after collection so a slow probe cannot
		// block the system metrics snapshot, but before marshal so the
		// results ride on the same payload. Failures are isolated per
		// target and never abort the batch.
		if len(probeTargets) > 0 || len(tcpTargets) > 0 || len(tlsTargets) > 0 || len(dnsTargets) > 0 || len(icmpTargets) > 0 {
			var httpR, tcpR, tlsR, dnsR, icmpR []protocol.ProbeResult
			if len(probeTargets) > 0 {
				httpR = prober.Run()
			}
			if len(tcpTargets) > 0 {
				tcpR = tcpRunner.Run()
			}
			if len(tlsTargets) > 0 {
				tlsR = tlsRunner.Run()
			}
			if len(dnsTargets) > 0 {
				dnsR = dnsRunner.Run()
			}
			if len(icmpTargets) > 0 {
				icmpR = icmpRunner.Run()
			}
			hb.Probes = probe.MergeProbeResultsAll(httpR, tcpR, tlsR, dnsR, icmpR)
			for _, p := range hb.Probes {
				if !p.OK {
					log.Printf("Probe [%s] %s failed: status=%d err=%q",
						defaultKind(p.Kind), p.URL, p.StatusCode, p.Error)
				}
			}
		}

		data, err := json.Marshal(hb)
		if err != nil {
			log.Printf("Marshal error: %v", err)
			time.Sleep(*interval)
			continue
		}

		if *dryRun {
			var pretty bytes.Buffer
			json.Indent(&pretty, data, "", "  ")
			fmt.Println(pretty.String())
			return
		}

		req, err := http.NewRequest("POST", *serverURL, bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Request creation error: %v", err)
			time.Sleep(*interval)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("X-NodePulse-Token", *token)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Heartbeat delivery failed: %v", err)
		} else {
			var hbResp protocol.HeartbeatResponse
			if err := json.NewDecoder(resp.Body).Decode(&hbResp); err == nil {
				for _, cmd := range hbResp.Commands {
					key := autoheal.Key(cmd)
					class := classifyCmd(cmd)
					strategy := policy.StrategyFor(class)
					breaker := breakers.forKey(key, strategy)

					decision := breaker.Allow(key)
					ts := time.Now().Unix()

					if !decision.Allowed {
						log.Printf("Auto-heal %s skipped (%s, retry=%s) class=%s", key, decision.Reason, decision.RetryAfter, class)
						pendingMu.Lock()
						pending = append(pending, protocol.AutoHealLog{
							Command:  key,
							Status:   "skipped",
							Reason:   decision.Reason,
							Ts:       ts,
							RetrySec: int64(decision.RetryAfter.Seconds()),
							Class:    string(class),
						})
						pendingMu.Unlock()
						continue
					}

					execErr := collector.ExecuteCommand(cmd)
					breaker.Record(key, execErr)
					if execErr != nil {
						log.Printf("Auto-heal %s error: %v", key, execErr)
						// Post-restart live check failure: the action itself
						// returned an error but the breaker should know it's
						// a crash-loop signal so it trips faster than the
						// generic burst counter. Map ErrPostRestartDown and
						// any wrapped variant of it to RecordCrash.
						if errors.Is(execErr, collector.ErrPostRestartDown) {
							breaker.RecordCrash(key)
							log.Printf("Auto-heal %s crash-loop detected, breaker notified", key)
						}
					} else {
						log.Printf("Auto-heal %s ok", key)
					}
					pendingMu.Lock()
					pending = append(pending, protocol.AutoHealLog{
						Command: key,
						Status:  boolToStatus(execErr == nil),
						Error:   errString(execErr),
						Ts:      ts,
						Class:   string(class),
					})
					pendingMu.Unlock()
				}
			}
			resp.Body.Close()
		}

		flushLogs()
		time.Sleep(*interval)
	}
}

func boolToStatus(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func defaultKind(k string) string {
	if k == "" {
		return "http"
	}
	return k
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

// splitCSV returns the comma-separated values from `direct` (the flag value)
// with `fallback` used when direct is empty. Empty entries are dropped so
// operators can leave trailing commas in their config without errors.
// splitCSV returns the comma-separated values from `direct` (the flag value)
// with `fallback` used when direct is empty. Empty entries are dropped so
// operators can leave trailing commas in their config without errors.
func splitCSV(direct, fallback string) []string {
	src := direct
	if src == "" {
		src = fallback
	}
	if src == "" {
		return nil
	}
	parts := strings.Split(src, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// classifyCommandFunc returns a closure that classifies a command's
// target into a remediation class. For Docker commands it consults the
// most recent docker inventory snapshot; for systemd targets the class is
// inferred from the service name only (no image to look at). The
// collector cache means we don't hit the docker socket per command.
func classifyCommandFunc(c *collector.Collector) func(string) policy.Class {
	return func(cmd string) policy.Class {
		_, target, err := collector.ParseCommand(cmd)
		if err != nil || target == "" {
			return policy.ClassDefault
		}
		action := strings.SplitN(cmd, ":", 2)[0]
		services := c.LastServices()
		for _, svc := range services {
			if action == "restart_docker" && svc.Type == "docker" && svc.Name == target {
				return policy.Classify(svc.Message, svc.Name, svc.Labels)
			}
		}
		// systemd target — no image/labels; let the name heuristic work.
		if action == "restart_systemd" {
			return policy.Classify("", target, nil)
		}
		return policy.ClassDefault
	}
}

// strategyBreakers is a tiny per-class breaker cache so a single
// Postgres target doesn't drag 4 different breakers behind it. The
// first call for a key pins the class; subsequent calls reuse the same
// breaker. If we ever ship hot class changes, this needs a version field
// on the breaker; for now sticky is the safe default.
type strategyBreakers struct {
	mu       sync.Mutex
	breakers map[string]*autoheal.Breaker
	classes  map[string]policy.Class
}

func newStrategyBreakers() *strategyBreakers {
	return &strategyBreakers{
		breakers: make(map[string]*autoheal.Breaker),
		classes:  make(map[string]policy.Class),
	}
}

func (s *strategyBreakers) forKey(key string, strat policy.Strategy) *autoheal.Breaker {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.breakers[key]; ok {
		return b
	}
	b := autoheal.NewBreakerWithStrategy(autoheal.Strategy{
		Cooldown:    strat.Cooldown,
		BurstLimit:  strat.BurstLimit,
		BurstWindow: strat.BurstWindow,
		OpenFor:     strat.OpenFor,
	})
	s.breakers[key] = b
	// We don't currently emit the class to the breaker; it's logged with
	// each skip / failure so telemetry can correlate.
	s.classes[key] = strat.Class
	return b
}
