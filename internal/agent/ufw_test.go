package agent

import (
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
}

func (f *fakeUFW) run(name string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	f.calls = append(f.calls, call{name, args})
	if err, ok := f.results[joined]; ok {
		if err != nil {
			return "err-out", err
		}
	}
	return "ok", nil
}

func newTestApplier(f *fakeUFW) *UFWApplier {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	u := NewUFWApplier("ufw", false, log)
	u.runner = f.run
	return u
}

func lastAllow(f *fakeUFW) (string, bool) {
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].args[0] == "allow" {
			return strings.Join(f.calls[i].args, " "), true
		}
	}
	return "", false
}

func hasDelete(f *fakeUFW, ip string, port string) bool {
	for _, c := range f.calls {
		if c.args[0] == "delete" && strings.Contains(strings.Join(c.args, " "), ip) &&
			strings.Contains(strings.Join(c.args, " "), port) {
			return true
		}
	}
	return false
}

func TestApplyAddsRule(t *testing.T) {
	f := &fakeUFW{results: map[string]error{}}
	u := newTestApplier(f)

	if err := u.Apply("1.2.3.4", 22, "tcp"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, ok := lastAllow(f)
	if !ok {
		t.Fatal("no allow command recorded")
	}
	want := "allow from 1.2.3.4 to any port 22 proto tcp comment ipcod"
	if got != want {
		t.Fatalf("allow cmd = %q, want %q", got, want)
	}
}

func TestApplyDefaultProtoTCP(t *testing.T) {
	f := &fakeUFW{results: map[string]error{}}
	u := newTestApplier(f)
	if err := u.Apply("1.2.3.4", 8080, ""); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := lastAllow(f)
	if !strings.Contains(got, "proto tcp") {
		t.Fatalf("empty proto should default to tcp, got %q", got)
	}
}

func TestApplyIdempotentSameRule(t *testing.T) {
	f := &fakeUFW{results: map[string]error{}}
	u := newTestApplier(f)
	if err := u.Apply("1.2.3.4", 22, "tcp"); err != nil {
		t.Fatal(err)
	}
	n := len(f.calls)
	// Applying the identical rule again should be a no-op.
	if err := u.Apply("1.2.3.4", 22, "tcp"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != n {
		t.Fatalf("re-issuing same rule ran %d extra commands, want 0", len(f.calls)-n)
	}
}

func TestApplyIPChangeRemovesOldRule(t *testing.T) {
	f := &fakeUFW{results: map[string]error{}}
	u := newTestApplier(f)
	if err := u.Apply("1.1.1.1", 22, "tcp"); err != nil {
		t.Fatal(err)
	}
	// New IP must delete the old and add the new.
	if err := u.Apply("2.2.2.2", 22, "tcp"); err != nil {
		t.Fatal(err)
	}
	if !hasDelete(f, "1.1.1.1", "22") {
		t.Fatalf("expected a delete of old rule 1.1.1.1, calls=%v", f.calls)
	}
	got, _ := lastAllow(f)
	if !strings.Contains(got, "from 2.2.2.2") {
		t.Fatalf("expected allow for new IP, got %q", got)
	}
	// Order matters: the delete must come before the new allow (we never want
	// both IPs allowed simultaneously, nor a gap where neither is).
	delIdx, allowIdx := -1, -1
	for i, c := range f.calls {
		if c.args[0] == "delete" && strings.Contains(strings.Join(c.args, " "), "1.1.1.1") {
			delIdx = i
		}
		if c.args[0] == "allow" && strings.Contains(strings.Join(c.args, " "), "2.2.2.2") {
			allowIdx = i
		}
	}
	if delIdx == -1 || allowIdx == -1 || delIdx > allowIdx {
		t.Fatalf("delete(old) must precede allow(new); del=%d allow=%d", delIdx, allowIdx)
	}
}

func TestApplyDeleteFailureIsNonFatal(t *testing.T) {
	// If removing the old rule errors, Apply should still try to add the new
	// one (and return its own error only if the allow itself fails).
	f := &fakeUFW{results: map[string]error{}}
	u := newTestApplier(f)
	if err := u.Apply("1.1.1.1", 22, "tcp"); err != nil {
		t.Fatal(err)
	}
	// Make the delete of 1.1.1.1 fail.
	f.results["delete allow from 1.1.1.1 to any port 22 proto tcp comment ipcod"] = os.ErrPermission
	if err := u.Apply("2.2.2.2", 22, "tcp"); err != nil {
		t.Fatalf("Apply should tolerate delete failure, got %v", err)
	}
	got, _ := lastAllow(f)
	if !strings.Contains(got, "from 2.2.2.2") {
		t.Fatalf("expected new allow despite delete failure, got %q", got)
	}
}

func TestClearRemovesRule(t *testing.T) {
	f := &fakeUFW{results: map[string]error{}}
	u := newTestApplier(f)
	if err := u.Apply("1.2.3.4", 22, "tcp"); err != nil {
		t.Fatal(err)
	}
	if err := u.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if !hasDelete(f, "1.2.3.4", "22") {
		t.Fatalf("Clear did not delete rule, calls=%v", f.calls)
	}
	// Clearing twice with nothing applied is a no-op.
	n := len(f.calls)
	if err := u.Clear(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != n {
		t.Fatal("second Clear should be a no-op")
	}
}

func TestClearNoRuleIsNoop(t *testing.T) {
	f := &fakeUFW{results: map[string]error{}}
	u := newTestApplier(f)
	if err := u.Clear(); err != nil {
		t.Fatalf("Clear with no rule: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("expected no commands, got %v", f.calls)
	}
}

func TestDryRunRunsNothing(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	u := NewUFWApplier("ufw", true, log)
	// real runner is exec; in dry-run it must not attempt exec. If it tried to
	// exec a missing "ufw" it would error. So a nil error proves dry-run path.
	if err := u.Apply("1.2.3.4", 22, "tcp"); err != nil {
		t.Fatalf("dry-run Apply errored: %v", err)
	}
	if err := u.Apply("2.2.2.2", 22, "tcp"); err != nil {
		t.Fatalf("dry-run re-Apply errored: %v", err)
	}
	if err := u.Clear(); err != nil {
		t.Fatalf("dry-run Clear errored: %v", err)
	}
}
