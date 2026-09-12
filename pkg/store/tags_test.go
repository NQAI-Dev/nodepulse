package store

import (
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestUpsertAndReadTags(t *testing.T) {
	st, err := NewPersistentStore(":memory:", "", 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := st.UpsertNodeTags("node-a", []string{"env=prod", "region=eu"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := st.TagsForNode("node-a")
	if len(got) != 2 || got[0] != "env=prod" || got[1] != "region=eu" {
		t.Fatalf("read tags: %v", got)
	}
}

func TestUpsertEmptyClearsTags(t *testing.T) {
	st, _ := NewPersistentStore(":memory:", "", 0)
	_ = st.UpsertNodeTags("node-a", []string{"env=prod"})
	_ = st.UpsertNodeTags("node-a", nil)
	if tags := st.TagsForNode("node-a"); len(tags) != 0 {
		t.Fatalf("expected cleared tags, got %v", tags)
	}
	if m := st.AllTaggedNodes(); len(m) != 0 {
		t.Fatalf("expected empty AllTaggedNodes, got %v", m)
	}
}

func TestNodeIDsByTagFiltersExact(t *testing.T) {
	st, _ := NewPersistentStore(":memory:", "", 0)
	_ = st.UpsertNodeTags("node-a", []string{"env=prod", "role=db"})
	_ = st.UpsertNodeTags("node-b", []string{"env=prod", "role=web"})
	_ = st.UpsertNodeTags("node-c", []string{"env=staging"})

	got := st.NodeIDsByTag("env=prod")
	if len(got) != 2 || got[0] != "node-a" || got[1] != "node-b" {
		t.Fatalf("env=prod filter: %v", got)
	}
	if got := st.NodeIDsByTag("env=staging"); len(got) != 1 || got[0] != "node-c" {
		t.Fatalf("env=staging filter: %v", got)
	}
	if got := st.NodeIDsByTag("nope"); len(got) != 0 {
		t.Fatalf("nope filter: %v", got)
	}
	if got := st.NodeIDsByTag(""); got != nil {
		t.Fatalf("empty filter should be nil, got %v", got)
	}
}

func TestNodeIDsByTagPrefixDoesNotMatch(t *testing.T) {
	// Ensure LIKE-based query doesn't accidentally match `env=prod-nightly`
	// when filtering for `env=prod`. The \n separators around the tag value
	// are what make this exact.
	st, _ := NewPersistentStore(":memory:", "", 0)
	_ = st.UpsertNodeTags("node-a", []string{"env=prod"})
	_ = st.UpsertNodeTags("node-b", []string{"env=prod-nightly"})
	got := st.NodeIDsByTag("env=prod")
	if len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("prefix must not match: %v", got)
	}
}

func TestListPublicNodesByTag(t *testing.T) {
	st, _ := NewPersistentStore(":memory:", "", 0)
	ingest := func(id string, tags []string) {
		st.Ingest(&protocol.Heartbeat{
			NodeID:    id,
			Timestamp: time.Now().Unix(),
			Node: protocol.NodeInfo{
				ID:       id,
				Hostname: id + ".host",
				Tags:     tags,
			},
			CPU: protocol.CPUStats{Cores: 2},
		})
	}
	ingest("alpha", []string{"env=prod", "role=db"})
	ingest("beta", []string{"env=prod", "role=web"})
	ingest("gamma", []string{"env=staging"})

	all := st.ListPublicNodes("")
	if len(all) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(all))
	}
	prod := st.ListPublicNodes("env=prod")
	if len(prod) != 2 {
		t.Fatalf("expected 2 prod nodes, got %d", len(prod))
	}
	for _, n := range prod {
		found := false
		for _, tg := range n.Tags {
			if tg == "env=prod" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("node %s missing env=prod tag in result: %v", n.NodeID, n.Tags)
		}
	}
	staging := st.ListPublicNodes("env=staging")
	if len(staging) != 1 || staging[0].NodeID != "gamma" {
		t.Fatalf("staging filter: %+v", staging)
	}
}

func TestListPublicNodesEmptyTagsIsSlice(t *testing.T) {
	st, _ := NewPersistentStore(":memory:", "", 0)
	st.Ingest(&protocol.Heartbeat{
		NodeID:    "solo",
		Timestamp: time.Now().Unix(),
		Node:      protocol.NodeInfo{ID: "solo"},
		CPU:       protocol.CPUStats{Cores: 1},
	})
	nodes := st.ListPublicNodes("")
	if len(nodes) != 1 {
		t.Fatalf("want 1, got %d", len(nodes))
	}
	if nodes[0].Tags == nil {
		t.Fatalf("Tags must be non-nil empty slice for JSON serialisation")
	}
}
