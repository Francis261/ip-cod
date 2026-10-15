package agent

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// UFWApplier applies and removes ufw allow rules for a laptop IP on a given
// port. It is safe for concurrent use and idempotent: it tracks the rule it
// last applied so an IP change replaces the old rule rather than piling up.
//
// When dryRun is set, no commands are executed; the agent logs what it would
// do. This is for local testing on machines without ufw or without root.
type UFWApplier struct {
	bin    string // path to the ufw binary, default "ufw"
	dryRun bool
	log    *slog.Logger

	mu      sync.Mutex
	applied string // description of the currently-applied rule, e.g. "1.2.3.4/32:22/tcp"
	hasRule bool
	runner  func(name string, args ...string) (string, error)
}

// NewUFWApplier constructs an applier. bin may be "" to use "ufw" on PATH.
func NewUFWApplier(bin string, dryRun bool, log *slog.Logger) *UFWApplier {
	if bin == "" {
		bin = "ufw"
	}
	u := &UFWApplier{bin: bin, dryRun: dryRun, log: log}
	u.runner = u.execRun
	return u
}

// Apply ensures the allow rule matches (ip, port, proto). If a different rule
// was previously applied by this process, it is removed first so we never
// leave stale entries.
func (u *UFWApplier) Apply(ip string, port uint16, proto string) error {
	if proto == "" {
		proto = "tcp"
	}
	desired := fmt.Sprintf("%s:%d/%s", ip, port, proto)

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.hasRule && u.applied == desired {
		u.log.Debug("rule already applied", "rule", desired)
		return nil
	}

	// Remove the previous rule (if any) before adding the new one, so the old
	// IP stops being allowed. Order matters: add-then-remove would briefly
	// allow both, remove-then-add briefly allows neither. We choose
	// remove-then-add because the window where neither is allowed is safer
	// than a window where a stale IP is allowed.
	if u.hasRule {
		if err := u.removeLocked(u.applied); err != nil {
			// Non-fatal: the old rule may already be gone. Log and continue.
			u.log.Warn("could not remove previous rule", "rule", u.applied, "err", err)
		}
	}

	if err := u.allowLocked(ip, port, proto); err != nil {
		return err
	}
	u.applied = desired
	u.hasRule = true
	u.log.Info("ufw rule applied", "rule", desired)
	return nil
}

// Clear removes the currently-applied rule, if any. Used on clean shutdown so
// we do not leave a firewall hole behind.
func (u *UFWApplier) Clear() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.hasRule {
		return nil
	}
	err := u.removeLocked(u.applied)
	if err == nil {
		u.log.Info("ufw rule cleared", "rule", u.applied)
		u.hasRule = false
		u.applied = ""
	}
	return err
}

// allowLocked runs "ufw allow". Caller must hold u.mu.
func (u *UFWApplier) allowLocked(ip string, port uint16, proto string) error {
	// ufw allow from <ip> to any port <port> proto <proto>
	// The comment tag makes the rule identifiable in "ufw status" output.
	args := []string{
		"allow", "from", ip,
		"to", "any", "port", fmt.Sprintf("%d", port),
		"proto", proto,
		"comment", "ipcod",
	}
	out, err := u.runner(u.bin, args...)
	if err != nil {
		return fmt.Errorf("ufw allow failed: %v: %s", err, out)
	}
	return nil
}

// removeLocked runs "ufw delete" for a previously-added rule. Caller holds u.mu.
func (u *UFWApplier) removeLocked(rule string) error {
	// rule is "ip:port/proto"; reconstruct the delete arguments.
	ip, rest, ok := splitOnce(rule, ":")
	if !ok {
		return fmt.Errorf("malformed rule %q", rule)
	}
	port, proto, ok := splitOnce(rest, "/")
	if !ok {
		proto = "tcp"
	}
	args := []string{
		"delete", "allow", "from", ip,
		"to", "any", "port", port,
		"proto", proto,
		"comment", "ipcod",
	}
	out, err := u.runner(u.bin, args...)
	if err != nil {
		return fmt.Errorf("ufw delete failed: %v: %s", err, out)
	}
	return nil
}

// execRun is the real command runner (overridden in tests).
func (u *UFWApplier) execRun(name string, args ...string) (string, error) {
	if u.dryRun {
		u.log.Info("[dry-run] would run", "cmd", name+" "+strings.Join(args, " "))
		return "", nil
	}
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// splitOnce splits s at the first occurrence of sep.
func splitOnce(s, sep string) (before, after string, found bool) {
	i := strings.Index(s, sep)
	if i == -1 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
