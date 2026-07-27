package main

import "testing"

// These tests validate against the SNMP WIRE FORMAT, not against our own
// encoder. Every packet below is hand-built from the type tags RFC 3416 and
// RFC 2578 require agents to emit, which is what net-snmp, UniFi and everything
// else actually put on the wire. Round-tripping our encoder through our decoder
// would prove nothing about real devices; this does.

// helper: build a GetResponse carrying one varbind with the given value bytes.
func respWith(oid []byte, val []byte) []byte {
	vb := berTLV(0x30, append(append([]byte{}, oid...), val...))
	pdu := berTLV(0xA2, concatBytes(
		berInt(1), // request-id
		berInt(0), // error-status
		berInt(0), // error-index
		berTLV(0x30, vb),
	))
	return berTLV(0x30, concatBytes(berInt(1), berStr("public"), pdu))
}

// sysUpTime.0 encoded per BER: 06 08 2b 06 01 02 01 01 03 00
var oidSysUpTime = []byte{0x06, 0x08, 0x2b, 0x06, 0x01, 0x02, 0x01, 0x01, 0x03, 0x00}

func TestOIDEncodingMatchesSpec(t *testing.T) {
	// .1.3.6.1.2.1.1.1.0 must encode as 06 08 2b 06 01 02 01 01 01 00.
	// 0x2b = 43 = 40*1 + 3 — the packed first two arcs.
	got, err := berOID(".1.3.6.1.2.1.1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x06, 0x08, 0x2b, 0x06, 0x01, 0x02, 0x01, 0x01, 0x01, 0x00}
	if string(got) != string(want) {
		t.Fatalf("sysDescr.0 encoding\n got % x\nwant % x", got, want)
	}
}

func TestOIDEncodingMultiByteArc(t *testing.T) {
	// Ubiquiti's enterprise arc is 41112, which exceeds 127 and so must use
	// base-128 continuation bytes. Worked by hand:
	//   41112 = 2*128^2 + 65*128 + 24   (2*16384 + 8320 + 24)
	//   digits 2, 65, 24 -> set the continuation bit on all but the last:
	//   0x82, 0xC1, 0x18
	// Check: 0x82&0x7f=2, 0xC1&0x7f=65, 0x18=24 -> 41112. ✓
	got, err := berOID(".1.3.6.1.4.1.41112")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x06, 0x08, 0x2b, 0x06, 0x01, 0x04, 0x01, 0x82, 0xC1, 0x18}
	if string(got) != string(want) {
		t.Fatalf("enterprise arc encoding\n got % x\nwant % x", got, want)
	}
	// and it must survive a round trip through the decoder
	if back := berDecOID(got[2:]); back != ".1.3.6.1.4.1.41112" {
		t.Fatalf("round trip gave %q", back)
	}
}

func TestDecodeNumericTypes(t *testing.T) {
	cases := []struct {
		name string
		tag  byte
		body []byte
		want float64
	}{
		{"INTEGER", 0x02, []byte{0x2a}, 42},
		{"Counter32", 0x41, []byte{0x00, 0xbc, 0x61, 0x4e}, 12345678},
		{"Gauge32", 0x42, []byte{0x03, 0xe8}, 1000},
		{"TimeTicks", 0x43, []byte{0x00, 0x98, 0x96, 0x80}, 10000000},
		{"Counter64", 0x46, []byte{0x01, 0x00, 0x00, 0x00, 0x00}, 4294967296},
	}
	for _, c := range cases {
		pkt := respWith(oidSysUpTime, berTLV(c.tag, c.body))
		res, err := parseResponse(pkt)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		v, ok := res[".1.3.6.1.2.1.1.3.0"]
		if !ok {
			t.Fatalf("%s: varbind missing; got %v", c.name, res)
		}
		f, isNum := numericValue(v)
		if !isNum || f != c.want {
			t.Fatalf("%s: got %v (numeric=%v), want %v", c.name, f, isNum, c.want)
		}
	}
}

// A missing OID must NOT read as a metric. If noSuchInstance decoded to 0 it
// would look like a genuine reading of zero — e.g. "0 clients connected" when
// the truth is "that OID does not exist on this device".
func TestExceptionTagsAreNotMetrics(t *testing.T) {
	for _, tag := range []byte{0x80, 0x81, 0x82} { // noSuchObject/Instance, endOfMibView
		pkt := respWith(oidSysUpTime, berTLV(tag, nil))
		res, err := parseResponse(pkt)
		if err != nil {
			t.Fatalf("tag 0x%02x: %v", tag, err)
		}
		if _, isNum := numericValue(res[".1.3.6.1.2.1.1.3.0"]); isNum {
			t.Fatalf("tag 0x%02x decoded as a numeric metric — a missing OID must not look like a reading", tag)
		}
	}
}

func TestOctetStringIsNotAMetric(t *testing.T) {
	pkt := respWith(oidSysUpTime, berTLV(0x04, []byte("UniFi Dream Machine")))
	res, _ := parseResponse(pkt)
	if _, isNum := numericValue(res[".1.3.6.1.2.1.1.3.0"]); isNum {
		t.Fatal("OCTET STRING must not become a numeric metric")
	}
}

func TestErrorStatusSurfaces(t *testing.T) {
	// error-status 2 == noSuchName. Must be an error, not silently empty.
	pdu := berTLV(0xA2, concatBytes(berInt(1), berInt(2), berInt(1), berTLV(0x30, nil)))
	pkt := berTLV(0x30, concatBytes(berInt(1), berStr("public"), pdu))
	if _, err := parseResponse(pkt); err == nil {
		t.Fatal("a non-zero error-status must surface as an error")
	}
}

func TestMultiByteLengthForm(t *testing.T) {
	// Responses larger than 127 bytes use the long length form (0x81 nn).
	// A sysDescr of 200 chars exercises it.
	long := make([]byte, 200)
	for i := range long {
		long[i] = 'x'
	}
	pkt := respWith(oidSysUpTime, berTLV(0x04, long))
	if _, err := parseResponse(pkt); err != nil {
		t.Fatalf("long-form length failed to parse: %v", err)
	}
}

func TestTruncatedPacketDoesNotPanic(t *testing.T) {
	pkt := respWith(oidSysUpTime, berTLV(0x43, []byte{0x00, 0x98, 0x96, 0x80}))
	for i := 1; i < len(pkt); i++ {
		_, _ = parseResponse(pkt[:i]) // must not panic
	}
}

// collectSNMP must ALWAYS emit snmp_up, because PylonMon skips a vital rule
// whose metric is absent — a dead device that simply stopped reporting would
// otherwise page nobody.
func TestUnreachableTargetReportsDown(t *testing.T) {
	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737) — guaranteed not routable.
	c := &snmpConfig{Target: "203.0.113.1", Community: "public", TimeoutMS: 300,
		OIDs: map[string]string{"whatever": ".1.3.6.1.2.1.1.3.0"}}
	m := collectSNMP(c)
	if v, ok := m["snmp_up"]; !ok || v != 0 {
		t.Fatalf("unreachable target must report snmp_up=0, got %v (present=%v)", v, ok)
	}
	if _, leaked := m["whatever"]; leaked {
		t.Fatal("no reading should be reported when the target is unreachable")
	}
}

func TestEmptyConfigIsInert(t *testing.T) {
	if len(collectSNMP(nil)) != 0 {
		t.Fatal("nil config must produce no metrics — not even snmp_up")
	}
	if len(collectSNMP(&snmpConfig{Target: "1.2.3.4"})) != 0 {
		t.Fatal("a target with no OIDs must produce no metrics")
	}
}
