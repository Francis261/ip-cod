package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentPortsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	a, plain, err := s.CreateAgent(ctx, "web-1", []uint16{443, 22, 8443}, "tcp")
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if plain == "" {
		t.Fatal("expected a plaintext token")
	}

	got, err := s.GetAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.PrimaryPort() != 22 {
		t.Fatalf("PrimaryPort = %d, want 22 (sorted first)", got.PrimaryPort())
	}
	want := []uint16{22, 443, 8443}
	if !samePorts(got.Ports, want) {
		t.Fatalf("Ports = %v, want %v", got.Ports, want)
	}

	byTok, err := s.GetAgentByTokenHash(ctx, a.TokenHash)
	if err != nil {
		t.Fatalf("GetAgentByTokenHash: %v", err)
	}
	if !samePorts(byTok.Ports, want) {
		t.Fatalf("by-token Ports = %v, want %v", byTok.Ports, want)
	}

	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(list) != 1 || !samePorts(list[0].Ports, want) {
		t.Fatalf("ListAgents = %+v, want one agent with %v", list, want)
	}

	if err := s.UpdateAgentPorts(ctx, a.ID, []uint16{8080}, "udp"); err != nil {
		t.Fatalf("UpdateAgentPorts: %v", err)
	}
	upd, err := s.GetAgent(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAgent after update: %v", err)
	}
	if !samePorts(upd.Ports, []uint16{8080}) || upd.Proto != "udp" {
		t.Fatalf("after update: ports=%v proto=%q, want [8080] udp", upd.Ports, upd.Proto)
	}

	if _, _, err := s.CreateAgent(ctx, "no-ports", nil, "tcp"); err == nil {
		t.Fatal("CreateAgent with no ports should fail")
	}
}

// TestLegacySinglePortMigration verifies a database created before the ports
// column existed migrates cleanly: existing rows keep their single port.
func TestLegacySinglePortMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Hand-build a pre-multi-port database.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	const legacySchema = `
CREATE TABLE agents (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	token_hash  TEXT NOT NULL UNIQUE,
	port        INTEGER NOT NULL,
	proto       TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	last_seen   TEXT NOT NULL DEFAULT '',
	last_ip     TEXT NOT NULL DEFAULT '',
	last_revision INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL
)`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(`INSERT INTO agents (id, name, token_hash, port, proto, created_at)
		VALUES ('legacy-1', 'old-agent', 'hash1', 2222, 'tcp', ?)`, now)
	if err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy db: %v", err)
	}
	defer s.Close()

	got, err := s.GetAgent(ctx, "legacy-1")
	if err != nil {
		t.Fatalf("GetAgent on migrated row: %v", err)
	}
	if !samePorts(got.Ports, []uint16{2222}) {
		t.Fatalf("migrated Ports = %v, want [2222]", got.Ports)
	}
	if got.Proto != "tcp" {
		t.Fatalf("migrated Proto = %q, want tcp", got.Proto)
	}
}

func samePorts(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
