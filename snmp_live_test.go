package main

import (
	"os"
	"testing"
)

// Integration test against a REAL SNMP agent. Skipped unless a target is given,
// so it never runs in CI or on a developer machine that has no agent:
//
//	PYLON_SNMP_TARGET=192.168.0.245 go test -run TestLiveAgent -v
//
// The unit tests in snmp_test.go check our decoding against the wire format the
// RFCs mandate. This checks it against what an agent actually sends, which is
// the only way to catch an assumption both our encoder and our fixtures share.
func TestLiveAgent(t *testing.T) {
	target := os.Getenv("PYLON_SNMP_TARGET")
	if target == "" {
		t.Skip("set PYLON_SNMP_TARGET to run against a live agent")
	}
	community := os.Getenv("PYLON_SNMP_COMMUNITY")
	if community == "" {
		community = "public"
	}

	cfg := &snmpConfig{
		Target:    target,
		Community: community,
		TimeoutMS: 3000,
		OIDs: map[string]string{
			// standard MIB-II — implemented by essentially every SNMP device
			"uptime_ticks": ".1.3.6.1.2.1.1.3.0", // TimeTicks
			"if1_in":       ".1.3.6.1.2.1.2.2.1.10.1", // Counter32
			"if1_oper":     ".1.3.6.1.2.1.2.2.1.8.1",  // INTEGER
			// non-numeric: must be polled without error and then ignored
			"sys_name": ".1.3.6.1.2.1.1.5.0", // OCTET STRING
			// deliberately absent: must not appear, and must not read as 0
			"bogus": ".1.3.6.1.4.1.99999.1.2.3.0",
		},
	}

	m := collectSNMP(cfg)

	if m["snmp_up"] != 1 {
		t.Fatalf("snmp_up = %v, want 1 (is the agent reachable at %s?)", m["snmp_up"], target)
	}
	if _, ok := m["uptime_ticks"]; !ok {
		t.Error("sysUpTime (TimeTicks) did not decode to a metric")
	}
	if _, ok := m["sys_name"]; ok {
		t.Error("sysName is an OCTET STRING and must NOT become a metric")
	}
	if v, ok := m["bogus"]; ok {
		t.Errorf("a nonexistent OID produced a metric (%v) — a missing OID must report nothing, never 0", v)
	}
	for k, v := range m {
		t.Logf("  %-14s = %v", k, v)
	}
}
