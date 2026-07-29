package main

// Proxmox VE integration.
//
// The agent already reports the host it runs on — CPU, memory, disk,
// temperature. On a hypervisor that is the least interesting half of the
// picture: the host can be perfectly healthy while a VM has been off since
// Tuesday. Everything that actually matters on a Proxmox box is one level up.
//
// So this reads the cluster's own inventory and turns it into metrics you can
// put a threshold on:
//
//	pve_qemu_running / pve_qemu_stopped / pve_qemu_total
//	pve_lxc_running  / pve_lxc_stopped  / pve_lxc_total
//	pve_guests_stopped        every non-template guest that is not running
//	pve_nodes_online / pve_nodes_total
//	pve_storage_pct           the FULLEST storage, because that is the one
//	                          that will break something first
//	pve_quorate               1 when the cluster has quorum, 0 when it does not
//
// A vital rule of "pve_guests_stopped > 0 for 5 minutes" then pages you when a
// VM dies, and the alert carries the names — see the sample below.
//
// WHY SHELL OUT TO pvesh. It is already installed on every Proxmox host, it is
// already authenticated as root, and it speaks the same API the web UI does.
// The alternative is an API token, which means creating one, storing a secret
// on the box, and keeping it valid. The agent runs on the host as root
// already; borrowing the credential that is inherently there is less to set up
// and less to leak.
//
// ONE CALL. /cluster/resources returns guests, storage and nodes in a single
// response, so the whole integration costs one subprocess per interval rather
// than one per guest. On a standalone host (no cluster) it works unchanged —
// Proxmox reports a single-node "cluster".
//
// TEMPLATES ARE NOT STOPPED VMS. A template's status is "stopped" forever, by
// definition. Counting them would mean every Proxmox user with a template — so
// most of them — gets paged on day one for a machine that is working exactly as
// intended. They are excluded everywhere.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type proxmoxConfig struct {
	Enabled   bool
	Bin       string // path to pvesh; default "pvesh"
	TimeoutMS int    // per-command timeout, default 8000
}

// pveResource is the subset of /cluster/resources we use. Proxmox adds fields
// between versions, so this decodes permissively and ignores the rest.
type pveResource struct {
	Type     string  `json:"type"`   // qemu | lxc | storage | node | sdn | pool
	Status   string  `json:"status"` // running | stopped | online | offline | available
	Name     string  `json:"name"`
	Node     string  `json:"node"`
	Storage  string  `json:"storage"`
	VMID     int     `json:"vmid"`
	Template int     `json:"template"` // 1 for a template — never a "stopped VM"
	Disk     float64 `json:"disk"`
	MaxDisk  float64 `json:"maxdisk"`
}

// collectProxmox returns the metrics and, when guests are down, one text sample
// naming them. Any failure returns nothing at all: a hypervisor integration
// that cannot read the hypervisor must never cost the host its check-in.
func collectProxmox(c *proxmoxConfig) (map[string]float64, map[string]string) {
	if c == nil || !c.Enabled {
		return nil, nil
	}
	bin := c.Bin
	if bin == "" {
		bin = "pvesh"
	}
	timeout := time.Duration(c.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	raw, err := runProxmox(bin, timeout, "get", "/cluster/resources", "--output-format", "json")
	if err != nil {
		return nil, nil
	}
	m, samples := parseClusterResources(raw)
	if m == nil {
		return nil, nil
	}
	// Quorum is a separate endpoint and only meaningful in a real cluster. A
	// standalone host has no quorum to lose, so a failure here is silent rather
	// than reported as 0 — "not quorate" on a single box would be a lie that
	// pages people.
	if q, err := runProxmox(bin, timeout, "get", "/cluster/status", "--output-format", "json"); err == nil {
		if v, ok := parseQuorate(q); ok {
			m["pve_quorate"] = v
		}
	}
	return m, samples
}

func runProxmox(bin string, timeout time.Duration, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return nil, fmt.Errorf("pvesh timed out after %s", timeout)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseClusterResources turns one /cluster/resources response into metrics.
// Split out from the command so it is testable against real fixtures without a
// Proxmox host.
func parseClusterResources(raw []byte) (map[string]float64, map[string]string) {
	var rs []pveResource
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, nil
	}
	// NO DATA IS NOT ZERO. `null` and `[]` both decode without error, and
	// falling through would publish pve_nodes_online = 0 and pve_qemu_total = 0
	// — "every node is down and every VM is gone" — when the truth is that we
	// could not read the inventory. A rule like "pve_nodes_online < 1" would
	// then page for a Proxmox host that is perfectly fine and merely returned an
	// empty body. Report nothing and let the check-in itself carry the signal.
	if len(rs) == 0 {
		return nil, nil
	}
	m := map[string]float64{}
	var stopped []string
	worstPct := -1.0

	for _, r := range rs {
		switch r.Type {
		case "qemu", "lxc":
			if r.Template == 1 {
				continue // a template is not a machine that failed to start
			}
			kind := "pve_" + r.Type
			m[kind+"_total"]++
			if r.Status == "running" {
				m[kind+"_running"]++
			} else {
				m[kind+"_stopped"]++
				stopped = append(stopped, guestLabel(r))
			}
		case "node":
			m["pve_nodes_total"]++
			if r.Status == "online" {
				m["pve_nodes_online"]++
			}
		case "storage":
			// Report the FULLEST storage rather than an average: the pool about
			// to fill is the one that takes backups or the whole host down, and
			// an average across a full pool and three empty ones hides it.
			if r.MaxDisk > 0 {
				if pct := 100 * r.Disk / r.MaxDisk; pct > worstPct {
					worstPct = pct
				}
			}
		}
	}
	// Make the counters exist at zero. A metric that only appears once
	// something is wrong cannot be graphed, and a threshold on a metric that
	// has never been seen is never evaluated — so "0 stopped VMs" has to be
	// reported as 0, not as absent.
	for _, k := range []string{
		"pve_qemu_total", "pve_qemu_running", "pve_qemu_stopped",
		"pve_lxc_total", "pve_lxc_running", "pve_lxc_stopped",
		"pve_nodes_total", "pve_nodes_online",
	} {
		if _, ok := m[k]; !ok {
			m[k] = 0
		}
	}
	m["pve_guests_stopped"] = m["pve_qemu_stopped"] + m["pve_lxc_stopped"]
	if worstPct >= 0 {
		m["pve_storage_pct"] = round1(worstPct)
	}
	if len(m) == 0 {
		return nil, nil
	}

	// The count tells you something is down; the sample tells you what. It
	// rides the existing [logwatch] sample channel, so it appears on the
	// monitor detail and inside the alert itself — "2 guests stopped" is a
	// puzzle, "db-01, mail-01" is an instruction.
	var samples map[string]string
	if len(stopped) > 0 {
		sort.Strings(stopped)
		if len(stopped) > 40 {
			stopped = append(stopped[:40], fmt.Sprintf("…and %d more", len(stopped)-40))
		}
		samples = map[string]string{"pve_guests_stopped": strings.Join(stopped, "\n")}
	}
	return m, samples
}

// guestLabel identifies a stopped guest the way a human would look it up:
// "vm/100 web-01", or just the vmid when Proxmox reports no name.
func guestLabel(r pveResource) string {
	kind := "vm"
	if r.Type == "lxc" {
		kind = "ct"
	}
	s := fmt.Sprintf("%s/%d", kind, r.VMID)
	if n := strings.TrimSpace(r.Name); n != "" {
		s += " " + n
	}
	if n := strings.TrimSpace(r.Node); n != "" {
		s += " on " + n
	}
	return s
}

// parseQuorate reads /cluster/status, whose "cluster" entry carries quorate.
// The bool is whether we could determine it at all.
func parseQuorate(raw []byte) (float64, bool) {
	var rs []struct {
		Type    string `json:"type"`
		Quorate int    `json:"quorate"`
	}
	if err := json.Unmarshal(raw, &rs); err != nil {
		return 0, false
	}
	for _, r := range rs {
		if r.Type == "cluster" {
			if r.Quorate == 1 {
				return 1, true
			}
			return 0, true
		}
	}
	return 0, false // standalone host: no cluster entry, nothing to report
}
