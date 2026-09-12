package store

import (
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type NodeState struct {
	LastHeartbeat time.Time          `json:"last_heartbeat"`
	Info          protocol.NodeInfo  `json:"info"`
	Latest        protocol.Heartbeat `json:"latest"`
	Status        string             `json:"status"` // online, warning, critical, offline
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

	s.nodes[hb.NodeID] = &NodeState{
		LastHeartbeat: time.Now(),
		Info:          hb.Node,
		Latest:        *hb,
		Status:        status,
	}
}

func (s *Store) GetAll() map[string]*NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	copyMap := make(map[string]*NodeState, len(s.nodes))
	now := time.Now()
	for k, v := range s.nodes {
		state := *v
		if now.Sub(state.LastHeartbeat) > 90*time.Second {
			state.Status = "offline"
		}
		copyMap[k] = &state
	}
	return copyMap
}
