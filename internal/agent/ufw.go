package agent

import (
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// managedTag marks the rules this applier owns (the ufw comment). Only rules
// carrying it are ever listed, modified, or deleted — the agent never touches
// rules the user added themselves.
const managedTag = "ipcod"

// rule is one managed ufw rule: allow <ip> to any port <port> proto <proto>.
type rule struct {
	ip    string
	port  uint16
	proto string
}

func (r rule) key() string  { return fmt.Sprintf("%d/%s", r.port, r.proto) }
func (r rule) desc() string { return fmt.Sprintf("%s:%d/%s", r.ip, r.port, r.proto) }

// UFWApplier converges ufw to exactly the set of allow rules the coordinator
// asked for. It is safe for concurrent use and idempotent: Ensure compares the
// live rule set with the desired one and only touches the difference, so the
// coordinator's periodic re-pushes are no-ops while rules deleted out-of-band
// (or never added, e.g. after a crash) get repaired.
//
// When dryRun is set, no commands that change anything are executed; the
// applier logs what it would do and tracks the would-be state in memory (so
// repeated checks stay no-ops without ever exec'ing ufw, which may not even
// exist on a testing machine).
type UFWApplier struct {
	bin    string // path to the ufw binary, default "ufw"
	dryRun bool
	log    *slog.Logger

	mu sync.Mutex
	// sim is the simulated rule set, used in dry-run mode where `ufw status`
	// cannot be consulted (nothing is exec'd at all).
	sim map[string]rule
	// runner executes a ufw invocation (overridden in tests).
	runner func(name string, args ...string) (string, error)
}

// NewUFWApplier constructs an applier. bin may be "" to use "ufw" on PATH.
func NewUFWApplier(bin string, dryRun bool, log *slog.Logger) *UFWApplier {
	if bin == "" {
		bin = "ufw"
	}
	u := &UFWApplier{bin: bin, dryRun: dryRun, log: log, sim: map[string]rule{}}
	u.runner = u.execRun
	return u
}

// Ensure converges the firewall to exactly the rules implied by (ip, ports,
// proto): rules that already match are left alone, missing ones are added, and
// ipcod-tagged rules that no longer match (wrong IP, port removed, …) are
// deleted. It reports whether anything changed, so the caller can keep the
// logs quiet on periodic no-op verifications.
func (u *UFWApplier) Ensure(ip string, ports []uint16, proto string) (bool, error) {
	if proto == "" {
		proto = "tcp"
	}
	if len(ports) == 0 {
		return false, fmt.Errorf("no ports to guard")
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	desired := make(map[string]rule, len(ports))
	for _, p := range ports {
		r := rule{ip: ip, port: p, proto: proto}
		desired[r.key()] = r
	}

	actual, err := u.listLocked()
	if err != nil {
		return false, err
	}

	var toDelete, toAdd []rule
	for k, r := range actual {
		if d, ok := desired[k]; !ok || d.ip != r.ip {
			toDelete = append(toDelete, r)
		}
	}
	for k, r := range desired {
		// A same-port rule with a different IP was queued for delete above;
		// it still needs its replacement added here.
		if a, ok := actual[k]; !ok || a.ip != r.ip {
			toAdd = append(toAdd, r)
		}
	}
	sort.Slice(toDelete, func(i, j int) bool { return toDelete[i].key() < toDelete[j].key() })
	sort.Slice(toAdd, func(i, j int) bool { return toAdd[i].key() < toAdd[j].key() })

	if len(toDelete) == 0 && len(toAdd) == 0 {
		u.log.Debug("firewall already in sync", "ip", ip, "ports", ports, "proto", proto)
		return false, nil
	}

	// Remove stale rules before adding new ones: the window where a stale IP
	// is still allowed is worse than the window where none is.
	for _, r := range toDelete {
		if err := u.deleteLocked(r); err != nil {
			// Non-fatal: the rule may already be gone. Log and continue.
			u.log.Warn("could not remove rule", "rule", r.desc(), "err", err)
		}
	}
	for _, r := range toAdd {
		if err := u.allowLocked(r); err != nil {
			if u.dryRun {
				u.syncSimLocked(toDelete, toAdd)
			}
			return false, err
		}
	}
	if u.dryRun {
		u.syncSimLocked(toDelete, toAdd)
	}
	u.log.Info("firewall updated", "ip", ip, "ports", ports, "proto", proto,
		"added", len(toAdd), "removed", len(toDelete))
	return true, nil
}

// Clear removes every rule this applier manages, if any. Used on clean
// shutdown so we do not leave a firewall hole behind.
func (u *UFWApplier) Clear() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	actual, err := u.listLocked()
	if err != nil {
		return err
	}
	if len(actual) == 0 {
		return nil
	}
	keys := make([]string, 0, len(actual))
	for k := range actual {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := actual[k]
		if err := u.deleteLocked(r); err != nil {
			u.log.Warn("could not remove rule", "rule", r.desc(), "err", err)
			continue
		}
		u.log.Info("ufw rule cleared", "rule", r.desc())
		if u.dryRun {
			delete(u.sim, k)
		}
	}
	return nil
}

// listLocked returns the currently-managed rules: parsed from `ufw status` in
// real mode, simulated from memory in dry-run mode. Caller holds u.mu.
func (u *UFWApplier) listLocked() (map[string]rule, error) {
	if u.dryRun {
		out := make(map[string]rule, len(u.sim))
		for k, r := range u.sim {
			out[k] = r
		}
		return out, nil
	}
	out, err := u.runner(u.bin, "status")
	if err != nil {
		return nil, fmt.Errorf("ufw status failed: %v: %s", err, out)
	}
	return parseStatus(out), nil
}

// syncSimLocked applies the performed ops to the dry-run simulation.
// Caller holds u.mu.
func (u *UFWApplier) syncSimLocked(toDelete, toAdd []rule) {
	for _, r := range toDelete {
		delete(u.sim, r.key())
	}
	for _, r := range toAdd {
		u.sim[r.key()] = r
	}
}

// parseStatus extracts the ipcod-tagged allow rules from `ufw status` output.
// A managed line looks like:
//
//	22/tcp    ALLOW IN    1.2.3.4    # ipcod
//
// Only lines carrying our comment are considered, so rules the user added
// themselves are never touched.
func parseStatus(out string) map[string]rule {
	rules := map[string]rule{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "# "+managedTag) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		ai := -1
		for i, f := range fields {
			if strings.EqualFold(f, "ALLOW") {
				ai = i
				break
			}
		}
		// Layout: <port[/proto]> ALLOW IN <from> # ipcod
		if ai < 1 || ai+2 >= len(fields) || !strings.EqualFold(fields[ai+1], "IN") {
			continue
		}
		from := fields[ai+2]
		if strings.EqualFold(from, "anywhere") {
			continue // we never add From-Anywhere rules; not ours
		}
		pf := strings.SplitN(fields[0], "/", 2)
		n, err := strconv.ParseUint(pf[0], 10, 16)
		if err != nil || n == 0 {
			continue
		}
		proto := "tcp"
		if len(pf) == 2 && pf[1] != "" {
			proto = strings.ToLower(pf[1])
		}
		r := rule{ip: from, port: uint16(n), proto: proto}
		rules[r.key()] = r
	}
	return rules
}

// allowLocked runs "ufw allow" for one rule. Caller must hold u.mu.
func (u *UFWApplier) allowLocked(r rule) error {
	// ufw allow from <ip> to any port <port> proto <proto>
	// The comment tag makes the rule identifiable in "ufw status" output.
	args := []string{
		"allow", "from", r.ip,
		"to", "any", "port", strconv.FormatUint(uint64(r.port), 10),
		"proto", r.proto,
		"comment", managedTag,
	}
	out, err := u.runner(u.bin, args...)
	if err != nil {
		return fmt.Errorf("ufw allow failed: %v: %s", err, out)
	}
	return nil
}

// deleteLocked runs "ufw delete" for one rule. Caller holds u.mu. --force
// skips ufw's interactive confirmation, which would otherwise abort (nothing
// answers the y/n prompt) when the agent runs unattended under systemd.
func (u *UFWApplier) deleteLocked(r rule) error {
	args := []string{
		"--force", "delete", "allow", "from", r.ip,
		"to", "any", "port", strconv.FormatUint(uint64(r.port), 10),
		"proto", r.proto,
		"comment", managedTag,
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
