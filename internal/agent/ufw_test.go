package agent

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// fakeRunner records the commands the applier would run and returns scripted
// results, so we can test rule logic without root or a real ufw.
type call struct {
	name string
	args []string
}

type fakeUFW struct {
	calls   []call
	results map[string]error // key: joined args -> error (default nil)
	status  string           // output of "ufw status"
}

func (f *fakeUFW) run(name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, call{name, args})
	if err, ok := f.results[joined]; ok && err != nil {
		return "err-out", err
	}
	if len(args) > 0 && args[0] == "status" {
		return f.status, nil
	}
	return "ok", nil
}

func newTestApplier(f *fakeUFW) *UFWApplier {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	u := NewUFWApplier("ufw", false, log)
	u.runner = f.run
	return u
}

// statusRule renders one ipcod-tagged rule the way real `ufw status` prints it
// on Ubuntu/Debian — plain "ALLOW" with no "IN" (the parser must also accept
// "ALLOW IN"/"ALLOW FWD" variants other ufw versions emit).
func statusRule(portProto, from string) string {
	return fmt.Sprintf("%-27s ALLOW    %-22s # ipcod", portProto, from)
}

func statusOf(rules ...string) string {
	header := "Status: active\n\nTo                         Action      From\n--                         ------      ----\n"
	return header + strings.Join(rules, "\n") + "\n"
}

func joined(f *fakeUFW, i int) string { return strings.Join(f.calls[i].args, " ") }

// verbOf returns the ufw verb of a call, skipping a leading global option
// such as --force.
func verbOf(args []string) string {
	if len(args) > 0 && args[0] == "--force" && len(args) > 1 {
		return args[1]
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func countCalls(f *fakeUFW, verb string) int {
	n := 0
	for _, c := range f.calls {
		if verbOf(c.args) == verb {
			n++
		}
	}
	return n
}

func hasCall(f *fakeUFW, verb, substr string) bool {
	for i, c := range f.calls {
		if verbOf(c.args) == verb && strings.Contains(joined(f, i), substr) {
			return true
		}
	}
	return false
}

func TestEnsureAddsMissingRules(t *testing.T) {
	f := &fakeUFW{status: statusOf()}
	u := newTestApplier(f)

	changed, err := u.Ensure("1.2.3.4", []uint16{22, 443}, "tcp")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !changed {
		t.Fatal("expected changes on an empty firewall")
	}
	if got := countCalls(f, "allow"); got != 2 {
		t.Fatalf("allow calls = %d, want 2 (calls=%v)", got, f.calls)
	}
	if !hasCall(f, "allow", "allow from 1.2.3.4 to any port 22 proto tcp comment ipcod") {
		t.Fatalf("missing allow for 22, calls=%v", f.calls)
	}
	if !hasCall(f, "allow", "allow from 1.2.3.4 to any port 443 proto tcp comment ipcod") {
		t.Fatalf("missing allow for 443, calls=%v", f.calls)
	}
	// The initial state is empty: nothing should be deleted.
	if countCalls(f, "delete") != 0 {
		t.Fatalf("unexpected deletes on empty firewall: %v", f.calls)
	}
}

func TestEnsureDefaultProtoTCP(t *testing.T) {
	f := &fakeUFW{status: statusOf()}
	u := newTestApplier(f)
	if _, err := u.Ensure("1.2.3.4", []uint16{8080}, ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !hasCall(f, "allow", "proto tcp") {
		t.Fatalf("empty proto should default to tcp, calls=%v", f.calls)
	}
}

func TestEnsureNoopWhenInSync(t *testing.T) {
	f := &fakeUFW{status: statusOf(
		statusRule("22/tcp", "1.2.3.4"),
		statusRule("443/tcp", "1.2.3.4"),
		// Not ours — must be ignored:
		"80                         ALLOW IN    Anywhere",
		"3306/tcp                   DENY IN     Anywhere",
	)}
	u := newTestApplier(f)

	changed, err := u.Ensure("1.2.3.4", []uint16{22, 443}, "tcp")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if changed {
		t.Fatalf("in-sync firewall should be a no-op, ran %v", f.calls)
	}
	// Only the read-only status scan may have run.
	for _, c := range f.calls {
		if c.args[0] != "status" {
			t.Fatalf("unexpected command %v", c)
		}
	}
}

func TestEnsureRepairsMissingRule(t *testing.T) {
	f := &fakeUFW{status: statusOf(statusRule("22/tcp", "1.2.3.4"))}
	u := newTestApplier(f)

	changed, err := u.Ensure("1.2.3.4", []uint16{22, 443}, "tcp")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !changed {
		t.Fatal("expected a repair")
	}
	if countCalls(f, "allow") != 1 || !hasCall(f, "allow", "port 443") {
		t.Fatalf("expected exactly one allow for the missing 443, calls=%v", f.calls)
	}
	if countCalls(f, "delete") != 0 {
		t.Fatalf("existing rule must not be touched, calls=%v", f.calls)
	}
}

func TestEnsureReplacesStaleIP(t *testing.T) {
	f := &fakeUFW{status: statusOf(statusRule("22/tcp", "1.1.1.1"))}
	u := newTestApplier(f)

	changed, err := u.Ensure("2.2.2.2", []uint16{22}, "tcp")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !changed {
		t.Fatal("expected a change")
	}
	if !hasCall(f, "delete", "--force delete allow from 1.1.1.1 to any port 22 proto tcp comment ipcod") {
		t.Fatalf("expected delete of stale rule, calls=%v", f.calls)
	}
	if !hasCall(f, "allow", "allow from 2.2.2.2 to any port 22 proto tcp comment ipcod") {
		t.Fatalf("expected allow for new IP, calls=%v", f.calls)
	}
	// Order matters: delete before allow, so a stale IP is never allowed
	// alongside the new one.
	delIdx, allowIdx := -1, -1
	for i, c := range f.calls {
		if verbOf(c.args) == "delete" && strings.Contains(joined(f, i), "1.1.1.1") {
			delIdx = i
		}
		if verbOf(c.args) == "allow" && strings.Contains(joined(f, i), "2.2.2.2") {
			allowIdx = i
		}
	}
	if delIdx == -1 || allowIdx == -1 || delIdx > allowIdx {
		t.Fatalf("delete(stale) must precede allow(new); del=%d allow=%d", delIdx, allowIdx)
	}
}

func TestEnsureRemovesDroppedPort(t *testing.T) {
	f := &fakeUFW{status: statusOf(
		statusRule("22/tcp", "1.2.3.4"),
		statusRule("8080/tcp", "1.2.3.4"),
	)}
	u := newTestApplier(f)

	changed, err := u.Ensure("1.2.3.4", []uint16{22}, "tcp")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !changed {
		t.Fatal("expected a change")
	}
	if countCalls(f, "delete") != 1 || !hasCall(f, "delete", "port 8080") {
		t.Fatalf("expected exactly one delete for dropped 8080, calls=%v", f.calls)
	}
	if countCalls(f, "allow") != 0 {
		t.Fatalf("22 already present; no allows expected, calls=%v", f.calls)
	}
}

func TestEnsureDeleteFailureIsNonFatal(t *testing.T) {
	f := &fakeUFW{results: map[string]error{}, status: statusOf(statusRule("22/tcp", "1.1.1.1"))}
	u := newTestApplier(f)
	f.results["--force delete allow from 1.1.1.1 to any port 22 proto tcp comment ipcod"] = os.ErrPermission

	changed, err := u.Ensure("2.2.2.2", []uint16{22}, "tcp")
	if err != nil {
		t.Fatalf("Ensure should tolerate delete failure, got %v", err)
	}
	if !changed {
		t.Fatal("expected the add to have happened")
	}
	if !hasCall(f, "allow", "from 2.2.2.2") {
		t.Fatalf("expected new allow despite delete failure, calls=%v", f.calls)
	}
}

func TestEnsureEmptyPortsErrors(t *testing.T) {
	f := &fakeUFW{status: statusOf()}
	u := newTestApplier(f)
	if _, err := u.Ensure("1.2.3.4", nil, "tcp"); err == nil {
		t.Fatal("expected an error for an empty port list")
	}
}

func TestParseStatus(t *testing.T) {
	out := statusOf(
		statusRule("22/tcp", "1.2.3.4"),
		statusRule("443/tcp", "2001:db8::1"),
		// Other ufw versions print the direction word; must parse too:
		"8443/tcp                   ALLOW IN    5.6.7.8              # ipcod",
		// Not ours / not rules we ever create:
		"80                         ALLOW    Anywhere              # ipcod",
		"3306/tcp                   DENY IN     Anywhere                 # ipcod",
		"8080/tcp                   ALLOW    Anywhere",
		"53/udp                     ALLOW    5.6.7.8", // not tagged
	)
	got := parseStatus(out)
	if len(got) != 3 {
		t.Fatalf("parseStatus = %v, want exactly the three tagged non-Anywhere allows", got)
	}
	r, ok := got["22/tcp"]
	if !ok || r.ip != "1.2.3.4" || r.proto != "tcp" {
		t.Fatalf("missing 22/tcp rule: %v", got)
	}
	r, ok = got["443/tcp"]
	if !ok || r.ip != "2001:db8::1" {
		t.Fatalf("missing v6 443/tcp rule: %v", got)
	}
	r, ok = got["8443/tcp"]
	if !ok || r.ip != "5.6.7.8" {
		t.Fatalf("missing ALLOW-IN-style 8443/tcp rule: %v", got)
	}
}

func TestClearRemovesAllManagedRules(t *testing.T) {
	f := &fakeUFW{status: statusOf(
		statusRule("22/tcp", "1.2.3.4"),
		statusRule("443/tcp", "1.2.3.4"),
	)}
	u := newTestApplier(f)
	if err := u.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := countCalls(f, "delete"); got != 2 {
		t.Fatalf("delete calls = %d, want 2 (calls=%v)", got, f.calls)
	}
}

func TestClearNoRuleIsNoop(t *testing.T) {
	f := &fakeUFW{status: statusOf()}
	u := newTestApplier(f)
	if err := u.Clear(); err != nil {
		t.Fatalf("Clear with no rule: %v", err)
	}
	for _, c := range f.calls {
		if c.args[0] != "status" {
			t.Fatalf("expected no mutating commands, got %v", c)
		}
	}
}

// TestDryRunTracksSimulatedState proves dry-run never execs (a missing ufw
// binary would error), while still behaving idempotently across repeated
// checks: the first Ensure announces the rules, later ones are no-ops.
func TestDryRunTracksSimulatedState(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	u := NewUFWApplier("ufw-does-not-exist", true, log)

	changed, err := u.Ensure("1.2.3.4", []uint16{22, 443}, "tcp")
	if err != nil {
		t.Fatalf("dry-run Ensure errored: %v", err)
	}
	if !changed {
		t.Fatal("first dry-run Ensure should report changes")
	}
	changed, err = u.Ensure("1.2.3.4", []uint16{22, 443}, "tcp")
	if err != nil {
		t.Fatalf("dry-run re-Ensure errored: %v", err)
	}
	if changed {
		t.Fatal("second dry-run Ensure should be a no-op")
	}
	// An IP change updates the simulation: one delete, one add.
	changed, err = u.Ensure("2.2.2.2", []uint16{22, 443}, "tcp")
	if err != nil {
		t.Fatalf("dry-run Ensure (new ip) errored: %v", err)
	}
	if !changed {
		t.Fatal("IP change should report changes")
	}
	if err := u.Clear(); err != nil {
		t.Fatalf("dry-run Clear errored: %v", err)
	}
	if err := u.Clear(); err != nil {
		t.Fatalf("second dry-run Clear errored: %v", err)
	}
}
