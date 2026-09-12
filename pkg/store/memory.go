package store

import (
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type MetricPoint struct {
	Timestamp int64   `json:"ts"`
	Load1     float64 `json:"load1"`
	MemUsedPct float64 `json:"mem_pct"`
	DiskUsedPct float64 `json:"disk_pct"`
}

type NodeState struct {
	LastHeartbeat time.Time          `json:"last_heartbeat"`
	Info          protocol.NodeInfo  `json:"info"`
	Latest        protocol.Heartbeat `json:"latest"`
	Status        string             `json:"status"` // online, warning, offline
	History       []MetricPoint      `json:"history"`
}

type Store struct {
	mu    sync.RWMutex
	nodes map[string]*NodeState
}

func New() *Store {
	return &Store{
		nodes: make(map[string]*NodeState),
	}
}

func (s *Store) Ingest(hb *protocol.Heartbeat) {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := "online"
	if hb.CPU.Load1 > float64(hb.CPU.Cores)*1.5 || hb.Memory.UsedPercent > 90.0 {
		status = "warning"
	}

	diskPct := 0.0
	if len(hb.Disks) > 0 {
		diskPct = hb.Disks[0].UsedPercent
	}

	pt := MetricPoint{
		Timestamp: hb.Timestamp,
		Load1:     hb.CPU.Load1,
		MemUsedPct: hb.Memory.UsedPercent,
		DiskUsedPct: diskPct,
	}

	state, exists := s.nodes[hb.NodeID]
	var hist []MetricPoint
	if exists {
		hist = state.History
	}
	hist = append(hist, pt)
	if len(hist) > 60 { // keep last 60 heartbeats (e.g. 10-15 mins of high-res history)
		hist = hist[len(hist)-60:]
	}

	s.nodes[hb.NodeID] = &NodeState{
		LastHeartbeat: time.Now(),
		Info:          hb.Node,
		Latest:        *hb,
		Status:        status,
		History:       hist,
	}
}

func (s *Store) GetAll() map[string]*NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	copyMap := make(map[string]*NodeState, len(s.nodes))
	now := time.Now()
	for k, v := range s.nodes {
		state := *v
		if now.Sub(state.LastHeartbeat) > 35*time.Second {
			state.Status = "offline"
		}
		copyMap[k] = &state
	}
	return copyMap
}
