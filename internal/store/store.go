// Package store persists coordinator state in SQLite. It is the single source
// of truth for registered agents, beacons, the current laptop IP, and an audit
// trail of IP changes.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ipcod/ipcod/internal/token"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo needed
)

// ErrNotFound is returned when a queried row does not exist.
var ErrNotFound = errors.New("store: not found")

// Agent is a registered VPS agent that should guard one or more ports.
type Agent struct {
	ID string
	// Name is a human-friendly label (e.g. "web-1").
	Name string
	// TokenHash is the SHA-256 hex of the agent's bearer token.
	TokenHash string
	// Ports are the ports the agent allows the laptop IP to reach.
	Ports []uint16
	// Proto is "tcp", "udp", or "" (defaults to tcp at the agent).
	Proto string
	// Enabled gates whether the agent is allowed to authenticate.
	Enabled bool
	// LastSeen is when the agent last completed auth (or reported in).
	LastSeen time.Time
	// LastIP is the last laptop IP we successfully pushed to this agent.
	LastIP string
	// LastRevision is the highest revision we've sent this agent.
	LastRevision uint64
	CreatedAt    time.Time
}

// PrimaryPort is the first guarded port, or 0 when none is set.
func (a Agent) PrimaryPort() uint16 {
	if len(a.Ports) == 0 {
		return 0
	}
	return a.Ports[0]
}

// Beacon is a laptop-side reporter whose source IP the coordinator tracks.
type Beacon struct {
	ID        string
	Name      string
	TokenHash string
	Enabled   bool
	LastSeen  time.Time
	LastIP    string
	CreatedAt time.Time
}

// IPChange is one row of the laptop-IP history.
type IPChange struct {
	ID        int64
	IP        string
	BeaconID  string
	ChangedAt time.Time
}

// Store wraps a SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and migrates the
// schema.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// modernc's sqlite is safe for concurrent use but serializes writes; a
	// small pool keeps the coordinator responsive under many agents.
	db.SetMaxOpenConns(4)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS agents (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	token_hash  TEXT NOT NULL UNIQUE,
	port        INTEGER NOT NULL,
	ports       TEXT NOT NULL DEFAULT '',
	proto       TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	last_seen   TEXT NOT NULL DEFAULT '',
	last_ip     TEXT NOT NULL DEFAULT '',
	last_revision INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS beacons (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	token_hash TEXT NOT NULL UNIQUE,
	enabled    INTEGER NOT NULL DEFAULT 1,
	last_seen  TEXT NOT NULL DEFAULT '',
	last_ip    TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ip_history (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ip         TEXT NOT NULL,
	beacon_id  TEXT NOT NULL,
	changed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ip_history_changed ON ip_history(changed_at DESC);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	// Additive migration: multi-port support added a `ports` column. Databases
	// created before it only have the single-port `port` column; existing rows
	// keep working because reads fall back to it when `ports` is empty.
	if has, err := hasColumn(ctx, s.db, "agents", "ports"); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	} else if !has {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE agents ADD COLUMN ports TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("store: migrate ports column: %w", err)
		}
	}
	return nil
}

// hasColumn reports whether table has the named column. table is only ever a
// hardcoded identifier from this package (PRAGMA arguments cannot be bound).
func hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notNull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// portsCSV renders a port list for the ports column, e.g. "22,443".
func portsCSV(ports []uint16) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.FormatUint(uint64(p), 10)
	}
	return strings.Join(parts, ",")
}

// parsePorts turns the ports column into a deduplicated, sorted list. An empty
// column falls back to the legacy single-port column, so agents created before
// multi-port support keep working unchanged.
func parsePorts(csv string, legacy uint16) []uint16 {
	if strings.TrimSpace(csv) == "" {
		if legacy > 0 {
			return []uint16{legacy}
		}
		return nil
	}
	var out []uint16
	seen := map[uint16]bool{}
	for _, part := range strings.Split(csv, ",") {
		n, err := strconv.ParseUint(strings.TrimSpace(part), 10, 16)
		if err != nil || n == 0 || seen[uint16(n)] {
			continue
		}
		seen[uint16(n)] = true
		out = append(out, uint16(n))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---- Agents ----

// CreateAgent inserts a new agent and returns the generated plaintext token
// alongside the stored record. The plaintext token is returned exactly once.
func (s *Store) CreateAgent(ctx context.Context, name string, ports []uint16, proto string) (*Agent, string, error) {
	if len(ports) == 0 {
		return nil, "", errors.New("store: create agent: at least one port is required")
	}
	plain, err := token.Generate(token.KindAgent)
	if err != nil {
		return nil, "", err
	}
	id, err := newID()
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	a := &Agent{
		ID:        id,
		Name:      name,
		TokenHash: token.Hash(plain),
		Ports:     append([]uint16(nil), ports...),
		Proto:     proto,
		Enabled:   true,
		CreatedAt: now,
	}
	const q = `
INSERT INTO agents (id, name, token_hash, port, ports, proto, enabled, created_at)
VALUES (?, ?, ?, ?, ?, ?, 1, ?)`
	if _, err := s.db.ExecContext(ctx, q,
		a.ID, a.Name, a.TokenHash, a.PrimaryPort(), portsCSV(a.Ports), a.Proto,
		a.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return nil, "", fmt.Errorf("store: create agent: %w", err)
	}
	return a, plain, nil
}

// agentCols is the canonical agent column list; scanAgent pairs with it.
const agentCols = `id, name, token_hash, port, ports, proto, enabled, last_seen, last_ip, last_revision, created_at`

// scanAgent reads one agent row. Rows may carry a stale (pre-migration)
// empty `ports` value, in which case the legacy `port` column is used.
func (s *Store) scanAgent(row interface{ Scan(...any) error }) (*Agent, error) {
	a := &Agent{}
	var legacyPort int64
	var portsVal, lastSeen, created string
	var enabled int
	if err := row.Scan(&a.ID, &a.Name, &a.TokenHash, &legacyPort, &portsVal, &a.Proto,
		&enabled, &lastSeen, &a.LastIP, &a.LastRevision, &created); err != nil {
		return nil, err
	}
	a.Ports = parsePorts(portsVal, uint16(legacyPort))
	a.Enabled = enabled != 0
	a.LastSeen = parseTime(lastSeen)
	a.CreatedAt = parseTime(created)
	return a, nil
}

// GetAgentByTokenHash looks up an agent by the hash of its bearer token.
func (s *Store) GetAgentByTokenHash(ctx context.Context, hash string) (*Agent, error) {
	a, err := s.scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE token_hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get agent by token: %w", err)
	}
	return a, nil
}

// GetAgent fetches one agent by ID.
func (s *Store) GetAgent(ctx context.Context, id string) (*Agent, error) {
	a, err := s.scanAgent(s.db.QueryRowContext(ctx,
		`SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get agent: %w", err)
	}
	return a, nil
}

// ListAgents returns all agents ordered by name.
func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list agents: %w", err)
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := s.scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// UpdateAgentPorts changes the ports/proto an agent guards.
func (s *Store) UpdateAgentPorts(ctx context.Context, id string, ports []uint16, proto string) error {
	if len(ports) == 0 {
		return errors.New("store: update agent ports: at least one port is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET port = ?, ports = ?, proto = ? WHERE id = ?`,
		ports[0], portsCSV(ports), proto, id)
	if err != nil {
		return fmt.Errorf("store: update agent ports: %w", err)
	}
	return requireAffected(res)
}

// SetAgentEnabled enables or disables an agent.
func (s *Store) SetAgentEnabled(ctx context.Context, id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET enabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("store: set agent enabled: %w", err)
	}
	return requireAffected(res)
}

// RenameAgent sets a new display name.
func (s *Store) RenameAgent(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("store: rename agent: %w", err)
	}
	return requireAffected(res)
}

// RotateAgentToken issues a new token for an agent, invalidating the old one.
func (s *Store) RotateAgentToken(ctx context.Context, id string) (string, error) {
	plain, err := token.Generate(token.KindAgent)
	if err != nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET token_hash = ? WHERE id = ?`, token.Hash(plain), id)
	if err != nil {
		return "", fmt.Errorf("store: rotate agent token: %w", err)
	}
	if err := requireAffected(res); err != nil {
		return "", err
	}
	return plain, nil
}

// DeleteAgent removes an agent.
func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete agent: %w", err)
	}
	return requireAffected(res)
}

// TouchAgent records that an agent was seen and the last IP/revision pushed.
func (s *Store) TouchAgent(ctx context.Context, id, ip string, revision uint64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET last_seen = ?, last_ip = ?, last_revision = ? WHERE id = ?`,
		now, ip, revision, id)
	if err != nil {
		return fmt.Errorf("store: touch agent: %w", err)
	}
	return nil
}

// TouchAgentSeen records a heartbeat without changing IP/revision.
func (s *Store) TouchAgentSeen(ctx context.Context, id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen = ? WHERE id = ?`, now, id)
	if err != nil {
		return fmt.Errorf("store: touch agent seen: %w", err)
	}
	return nil
}

// ---- Beacons ----

// CreateBeacon inserts a new beacon and returns its plaintext token once.
func (s *Store) CreateBeacon(ctx context.Context, name string) (*Beacon, string, error) {
	plain, err := token.Generate(token.KindBeacon)
	if err != nil {
		return nil, "", err
	}
	id, err := newID()
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	b := &Beacon{
		ID:        id,
		Name:      name,
		TokenHash: token.Hash(plain),
		Enabled:   true,
		CreatedAt: now,
	}
	const q = `
INSERT INTO beacons (id, name, token_hash, enabled, created_at)
VALUES (?, ?, ?, 1, ?)`
	if _, err := s.db.ExecContext(ctx, q, b.ID, b.Name, b.TokenHash, b.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return nil, "", fmt.Errorf("store: create beacon: %w", err)
	}
	return b, plain, nil
}

// GetBeaconByTokenHash looks up a beacon by token hash.
func (s *Store) GetBeaconByTokenHash(ctx context.Context, hash string) (*Beacon, error) {
	const q = `
SELECT id, name, token_hash, enabled, last_seen, last_ip, created_at
FROM beacons WHERE token_hash = ?`
	b := &Beacon{}
	var lastSeen, created string
	var enabled int
	err := s.db.QueryRowContext(ctx, q, hash).Scan(
		&b.ID, &b.Name, &b.TokenHash, &enabled, &lastSeen, &b.LastIP, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get beacon by token: %w", err)
	}
	b.Enabled = enabled != 0
	b.LastSeen = parseTime(lastSeen)
	b.CreatedAt = parseTime(created)
	return b, nil
}

// GetBeacon fetches one beacon by ID.
func (s *Store) GetBeacon(ctx context.Context, id string) (*Beacon, error) {
	const q = `
SELECT id, name, token_hash, enabled, last_seen, last_ip, created_at
FROM beacons WHERE id = ?`
	b := &Beacon{}
	var lastSeen, created string
	var enabled int
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&b.ID, &b.Name, &b.TokenHash, &enabled, &lastSeen, &b.LastIP, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get beacon: %w", err)
	}
	b.Enabled = enabled != 0
	b.LastSeen = parseTime(lastSeen)
	b.CreatedAt = parseTime(created)
	return b, nil
}

// ListBeacons returns all beacons ordered by name.
func (s *Store) ListBeacons(ctx context.Context) ([]Beacon, error) {
	const q = `
SELECT id, name, token_hash, enabled, last_seen, last_ip, created_at
FROM beacons ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list beacons: %w", err)
	}
	defer rows.Close()
	out := []Beacon{}
	for rows.Next() {
		var b Beacon
		var lastSeen, created string
		var enabled int
		if err := rows.Scan(&b.ID, &b.Name, &b.TokenHash, &enabled, &lastSeen, &b.LastIP, &created); err != nil {
			return nil, err
		}
		b.Enabled = enabled != 0
		b.LastSeen = parseTime(lastSeen)
		b.CreatedAt = parseTime(created)
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBeaconEnabled enables or disables a beacon.
func (s *Store) SetBeaconEnabled(ctx context.Context, id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE beacons SET enabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("store: set beacon enabled: %w", err)
	}
	return requireAffected(res)
}

// RenameBeacon sets a new display name.
func (s *Store) RenameBeacon(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE beacons SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return fmt.Errorf("store: rename beacon: %w", err)
	}
	return requireAffected(res)
}

// RotateBeaconToken issues a new beacon token.
func (s *Store) RotateBeaconToken(ctx context.Context, id string) (string, error) {
	plain, err := token.Generate(token.KindBeacon)
	if err != nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE beacons SET token_hash = ? WHERE id = ?`, token.Hash(plain), id)
	if err != nil {
		return "", fmt.Errorf("store: rotate beacon token: %w", err)
	}
	if err := requireAffected(res); err != nil {
		return "", err
	}
	return plain, nil
}

// DeleteBeacon removes a beacon.
func (s *Store) DeleteBeacon(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM beacons WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete beacon: %w", err)
	}
	return requireAffected(res)
}

// TouchBeacon records a beacon check-in with its observed source IP.
func (s *Store) TouchBeacon(ctx context.Context, id, ip string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE beacons SET last_seen = ?, last_ip = ? WHERE id = ?`, now, ip, id)
	if err != nil {
		return fmt.Errorf("store: touch beacon: %w", err)
	}
	return nil
}

// ---- IP history & current IP ----

// meta keys.
const (
	keyCurrentIP   = "current_ip"
	keyCurrentRev  = "current_revision"
	keyIPChangedAt = "ip_changed_at"
)

// CurrentIP returns the laptop's current public IP, its revision, and when it
// last changed. If there is no IP yet, ip is "" and ok is false.
func (s *Store) CurrentIP(ctx context.Context) (ip string, revision uint64, changedAt time.Time, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, keyCurrentIP).Scan(&ip)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, time.Time{}, false, nil
	}
	if err != nil {
		return "", 0, time.Time{}, false, fmt.Errorf("store: current ip: %w", err)
	}
	var revStr, atStr string
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, keyCurrentRev).Scan(&revStr)
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, keyIPChangedAt).Scan(&atStr)
	revision = parseUint(revStr)
	changedAt = parseTime(atStr)
	return ip, revision, changedAt, ip != "", nil
}

// SetCurrentIP stores a new laptop IP, bumping the revision and appending to
// history. It returns the new revision. The caller is responsible for deciding
// whether the IP actually changed; this always records.
func (s *Store) SetCurrentIP(ctx context.Context, ip, beaconID string) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: set current ip begin: %w", err)
	}
	defer tx.Rollback()

	var revStr string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, keyCurrentRev).Scan(&revStr)
	if errors.Is(err, sql.ErrNoRows) {
		revStr = "0"
	} else if err != nil {
		return 0, fmt.Errorf("store: set current ip rev: %w", err)
	}
	newRev := parseUint(revStr) + 1
	now := time.Now().UTC()

	upsert := `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.ExecContext(ctx, upsert, keyCurrentIP, ip); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, upsert, keyCurrentRev, fmt.Sprintf("%d", newRev)); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, upsert, keyIPChangedAt, now.Format(time.RFC3339Nano)); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ip_history (ip, beacon_id, changed_at) VALUES (?, ?, ?)`,
		ip, beaconID, now.Format(time.RFC3339Nano)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: set current ip commit: %w", err)
	}
	return newRev, nil
}

// IPHistory returns the most recent IP changes, newest first.
func (s *Store) IPHistory(ctx context.Context, limit int) ([]IPChange, error) {
	if limit <= 0 {
		limit = 50
	}
	const q = `SELECT id, ip, beacon_id, changed_at FROM ip_history ORDER BY changed_at DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("store: ip history: %w", err)
	}
	defer rows.Close()
	out := []IPChange{}
	for rows.Next() {
		var c IPChange
		var at string
		if err := rows.Scan(&c.ID, &c.IP, &c.BeaconID, &at); err != nil {
			return nil, err
		}
		c.ChangedAt = parseTime(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- helpers ----

func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseUint(s string) uint64 {
	var v uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return v
		}
		v = v*10 + uint64(c-'0')
	}
	return v
}
