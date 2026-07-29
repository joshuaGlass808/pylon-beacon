package main

import "testing"

// A realistic /cluster/resources response: two nodes, running and stopped
// guests of both kinds, a TEMPLATE, and storages at different fullness.
const pveResourcesFixture = `[
  {"type":"node","status":"online","node":"pve1","maxcpu":8},
  {"type":"node","status":"online","node":"pve2","maxcpu":8},
  {"type":"node","status":"offline","node":"pve3","maxcpu":4},

  {"type":"qemu","status":"running","name":"web-01","vmid":100,"node":"pve1"},
  {"type":"qemu","status":"running","name":"web-02","vmid":101,"node":"pve2"},
  {"type":"qemu","status":"stopped","name":"db-01","vmid":102,"node":"pve1"},
  {"type":"qemu","status":"stopped","name":"debian-12-tmpl","vmid":9000,"node":"pve1","template":1},

  {"type":"lxc","status":"running","name":"pihole","vmid":200,"node":"pve1"},
  {"type":"lxc","status":"stopped","name":"mail","vmid":201,"node":"pve2"},

  {"type":"storage","storage":"local","node":"pve1","disk":10737418240,"maxdisk":107374182400},
  {"type":"storage","storage":"tank","node":"pve1","disk":900000000000,"maxdisk":1000000000000},
  {"type":"storage","storage":"empty","node":"pve2","disk":0,"maxdisk":500000000000},
  {"type":"pool","poolid":"prod"}
]`

func TestParseClusterResources(t *testing.T) {
	m, samples := parseClusterResources([]byte(pveResourcesFixture))
	if m == nil {
		t.Fatal("no metrics parsed")
	}
	for _, tc := range []struct {
		key  string
		want float64
	}{
		// the template must NOT count as a stopped VM — it is stopped by
		// definition, and counting it pages every Proxmox user who has one
		{"pve_qemu_total", 3},
		{"pve_qemu_running", 2},
		{"pve_qemu_stopped", 1},

		{"pve_lxc_total", 2},
		{"pve_lxc_running", 1},
		{"pve_lxc_stopped", 1},

		{"pve_guests_stopped", 2}, // db-01 + mail, not the template

		{"pve_nodes_total", 3},
		{"pve_nodes_online", 2},

		// the FULLEST storage (tank at 90%), not an average — an average across
		// a full pool and an empty one hides the pool about to break
		{"pve_storage_pct", 90},
	} {
		if got, ok := m[tc.key]; !ok {
			t.Errorf("%s missing", tc.key)
		} else if got != tc.want {
			t.Errorf("%s = %v, want %v", tc.key, got, tc.want)
		}
	}

	// the count says something is down; the sample says what
	s := samples["pve_guests_stopped"]
	for _, want := range []string{"db-01", "mail", "vm/102", "ct/201"} {
		if !contains(s, want) {
			t.Errorf("sample %q does not mention %q", s, want)
		}
	}
	if contains(s, "debian-12-tmpl") {
		t.Error("the template is listed as a stopped guest")
	}
}

// TestCountersExistAtZero. A metric that only appears once something breaks
// cannot be graphed, and a threshold on a metric that has never been seen is
// never evaluated — so a healthy cluster must still report 0.
func TestCountersExistAtZero(t *testing.T) {
	healthy := `[
	  {"type":"node","status":"online","node":"pve1"},
	  {"type":"qemu","status":"running","name":"web","vmid":100,"node":"pve1"}
	]`
	m, samples := parseClusterResources([]byte(healthy))
	if m == nil {
		t.Fatal("no metrics")
	}
	for _, k := range []string{"pve_qemu_stopped", "pve_lxc_stopped", "pve_lxc_total", "pve_guests_stopped"} {
		v, ok := m[k]
		if !ok {
			t.Errorf("%s absent on a healthy cluster — a rule on it would never fire", k)
		} else if v != 0 {
			t.Errorf("%s = %v, want 0", k, v)
		}
	}
	if len(samples) != 0 {
		t.Errorf("healthy cluster produced samples: %v", samples)
	}
}

// A standalone Proxmox host is the common case for the audience this is for.
// It has no cluster, so quorum is unknowable — and reporting 0 ("not quorate")
// would be a lie that pages someone whose setup is entirely fine.
func TestQuorate(t *testing.T) {
	clustered := `[{"type":"cluster","name":"pve","quorate":1,"nodes":3},{"type":"node","name":"pve1","online":1}]`
	if v, ok := parseQuorate([]byte(clustered)); !ok || v != 1 {
		t.Errorf("quorate cluster: got (%v,%v), want (1,true)", v, ok)
	}

	lost := `[{"type":"cluster","name":"pve","quorate":0,"nodes":3}]`
	if v, ok := parseQuorate([]byte(lost)); !ok || v != 0 {
		t.Errorf("inquorate cluster: got (%v,%v), want (0,true)", v, ok)
	}

	standalone := `[{"type":"node","name":"pve1","online":1}]`
	if _, ok := parseQuorate([]byte(standalone)); ok {
		t.Error("standalone host reported a quorum value — it has no cluster, so this must be unreported rather than 0")
	}
}

// Garbage in must not produce metrics. A broken pvesh, a permission error, or
// a Proxmox version that changes the shape should cost the integration, never
// the host's check-in.
func TestParseClusterResourcesRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "not json", "{}", `{"error":"permission denied"}`, "null"} {
		if m, _ := parseClusterResources([]byte(in)); m != nil {
			t.Errorf("input %q produced metrics %v, want none", in, m)
		}
	}
	// valid JSON, nothing we recognise: no guests, no nodes, no storage
	if m, _ := parseClusterResources([]byte(`[{"type":"sdn","status":"ok"}]`)); m != nil {
		if m["pve_nodes_total"] != 0 || m["pve_qemu_total"] != 0 {
			t.Errorf("unrecognised resources produced non-zero counts: %v", m)
		}
	}
}

func TestProxmoxDisabledByDefault(t *testing.T) {
	if m, s := collectProxmox(nil); m != nil || s != nil {
		t.Error("nil config produced output")
	}
	if m, s := collectProxmox(&proxmoxConfig{}); m != nil || s != nil {
		t.Error("an unconfigured [proxmox] section ran anyway — it must be opt-in")
	}
}

// TestTruthy — a typo must turn a section OFF, never silently on.
func TestTruthy(t *testing.T) {
	for _, s := range []string{"true", "TRUE", "yes", "on", "1", " Enabled "} {
		if !truthy(s) {
			t.Errorf("truthy(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "false", "no", "0", "off", "ture", "maybe"} {
		if truthy(s) {
			t.Errorf("truthy(%q) = true, want false", s)
		}
	}
}

func contains(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
