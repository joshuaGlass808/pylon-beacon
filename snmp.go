package main

// SNMP collector — reads values off network gear (UniFi, switches, NAS, PDUs,
// printers, anything that speaks SNMP) and reports them as ordinary beacon
// metrics, so they flow into the same vital rules, thresholds and incidents as
// CPU or disk.
//
// WHY THE AGENT DOES THIS AND NOT PYLONMON: SNMP is UDP/161 and LAN-only in
// practice, and v1/v2c authentication is a cleartext community string. Polling
// it from off-site would mean asking customers to expose 161 to the internet —
// a protocol that is a well-known reflection/amplification vector. The beacon is
// already inside the network and already pushes outbound, so the poll happens
// on the LAN and only the results cross the internet. No open ports, same as
// everything else the beacon does.
//
// Stdlib only — SNMPv2c is BER/ASN.1 over UDP, implemented here rather than
// taking a dependency (this repo has none, deliberately).

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"
)

type snmpConfig struct {
	Target    string            // host or host:port (default port 161)
	Community string            // v2c community; default "public"
	TimeoutMS int               // per-request timeout, default 3000
	OIDs      map[string]string // metric name -> dotted OID
}

// ---------------------------------------------------------------- BER encode

func berLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var b []byte
	for x := n; x > 0; x >>= 8 {
		b = append([]byte{byte(x)}, b...)
	}
	return append([]byte{byte(0x80 | len(b))}, b...)
}

func berTLV(tag byte, body []byte) []byte {
	out := []byte{tag}
	out = append(out, berLen(len(body))...)
	return append(out, body...)
}

func berInt(v int) []byte {
	if v == 0 {
		return berTLV(0x02, []byte{0})
	}
	var b []byte
	for x := v; x != 0; x >>= 8 {
		b = append([]byte{byte(x & 0xff)}, b...)
	}
	if b[0]&0x80 != 0 { // keep it positive
		b = append([]byte{0}, b...)
	}
	return berTLV(0x02, b)
}

func berStr(s string) []byte { return berTLV(0x04, []byte(s)) }
func berNull() []byte        { return berTLV(0x05, nil) }

// berOID encodes a dotted OID. The first two arcs pack into a single byte as
// 40*a+b; each later arc is base-128 with the continuation bit set on all but
// its final byte.
func berOID(oid string) ([]byte, error) {
	parts := strings.Split(strings.TrimPrefix(oid, "."), ".")
	if len(parts) < 2 {
		return nil, errors.New("oid too short: " + oid)
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("bad oid arc %q in %s", p, oid)
		}
		nums[i] = n
	}
	body := []byte{byte(40*nums[0] + nums[1])}
	for _, n := range nums[2:] {
		var enc []byte
		if n == 0 {
			enc = []byte{0}
		} else {
			for x := n; x > 0; x >>= 7 {
				enc = append([]byte{byte(x & 0x7f)}, enc...)
			}
			for i := 0; i < len(enc)-1; i++ {
				enc[i] |= 0x80
			}
		}
		body = append(body, enc...)
	}
	return berTLV(0x06, body), nil
}

// ---------------------------------------------------------------- BER decode

type berVal struct {
	tag  byte
	body []byte
}

func berParse(b []byte) (berVal, []byte, error) {
	if len(b) < 2 {
		return berVal{}, nil, errors.New("truncated")
	}
	tag, i := b[0], 1
	n := int(b[i])
	i++
	if n&0x80 != 0 {
		cnt := n & 0x7f
		if cnt > 4 || len(b) < i+cnt {
			return berVal{}, nil, errors.New("bad length")
		}
		n = 0
		for j := 0; j < cnt; j++ {
			n = n<<8 | int(b[i+j])
		}
		i += cnt
	}
	if n < 0 || len(b) < i+n {
		return berVal{}, nil, errors.New("truncated value")
	}
	return berVal{tag, b[i : i+n]}, b[i+n:], nil
}

func berDecInt(b []byte) int64 {
	var v int64
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v
}

func berDecOID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	out := []string{strconv.Itoa(int(b[0]) / 40), strconv.Itoa(int(b[0]) % 40)}
	cur := 0
	for _, c := range b[1:] {
		cur = cur<<7 | int(c&0x7f)
		if c&0x80 == 0 {
			out = append(out, strconv.Itoa(cur))
			cur = 0
		}
	}
	return "." + strings.Join(out, ".")
}

// numericValue reports a varbind as a float when it is one of the numeric SNMP
// types. Everything else — strings, OIDs, and the v2c "not there" exceptions —
// is deliberately NOT a metric: reporting a missing OID as 0 would look like a
// real reading of zero, which is worse than reporting nothing.
//
//	0x02 INTEGER   0x41 Counter32  0x42 Gauge32/Unsigned32
//	0x43 TimeTicks 0x46 Counter64
//	0x80 noSuchObject  0x81 noSuchInstance  0x82 endOfMibView
func numericValue(v berVal) (float64, bool) {
	switch v.tag {
	case 0x02, 0x41, 0x42, 0x43, 0x46:
		return float64(berDecInt(v.body)), true
	}
	return 0, false
}

// ---------------------------------------------------------------- the request

// snmpGet performs one SNMPv2c GET carrying every requested OID, and returns
// the varbinds keyed by the OID the agent echoed back.
func snmpGet(target, community string, oids []string, timeout time.Duration) (map[string]berVal, error) {
	var vbs []byte
	for _, o := range oids {
		eo, err := berOID(o)
		if err != nil {
			return nil, err
		}
		vbs = append(vbs, berTLV(0x30, append(eo, berNull()...))...)
	}
	pdu := berTLV(0xA0, concatBytes( // 0xA0 = GetRequest
		berInt(int(rand.Int31n(0x7ffffff)+1)), // request-id
		berInt(0),                             // error-status
		berInt(0),                             // error-index
		berTLV(0x30, vbs),
	))
	msg := berTLV(0x30, concatBytes(
		berInt(1), // version 1 == SNMPv2c
		berStr(community),
		pdu,
	))

	addr := target
	if !strings.Contains(addr, ":") {
		addr += ":161"
	}
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, err
	}
	buf := make([]byte, 16384)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return parseResponse(buf[:n])
}

// parseResponse unwraps SEQUENCE{version, community, GetResponse{...}}.
// Split out from snmpGet so it can be tested against known wire bytes without
// a live agent.
func parseResponse(pkt []byte) (map[string]berVal, error) {
	top, _, err := berParse(pkt)
	if err != nil {
		return nil, err
	}
	rest := top.body
	for i := 0; i < 2; i++ { // version, community
		if _, rest, err = berParse(rest); err != nil {
			return nil, err
		}
	}
	pdu, _, err := berParse(rest)
	if err != nil {
		return nil, err
	}
	if pdu.tag != 0xA2 { // GetResponse
		return nil, fmt.Errorf("unexpected PDU tag 0x%02x", pdu.tag)
	}
	p := pdu.body
	if _, p, err = berParse(p); err != nil { // request-id
		return nil, err
	}
	esT, p, err := berParse(p) // error-status
	if err != nil {
		return nil, err
	}
	if _, p, err = berParse(p); err != nil { // error-index
		return nil, err
	}
	if es := berDecInt(esT.body); es != 0 {
		return nil, fmt.Errorf("snmp error-status %d", es)
	}
	vbl, _, err := berParse(p)
	if err != nil {
		return nil, err
	}
	out := map[string]berVal{}
	r := vbl.body
	for len(r) > 0 {
		var vb berVal
		if vb, r, err = berParse(r); err != nil {
			break
		}
		oidT, after, err := berParse(vb.body)
		if err != nil {
			break
		}
		valT, _, err := berParse(after)
		if err != nil {
			break
		}
		out[berDecOID(oidT.body)] = valT
	}
	return out, nil
}

func concatBytes(bs ...[]byte) []byte {
	var out []byte
	for _, b := range bs {
		out = append(out, b...)
	}
	return out
}

// ---------------------------------------------------------------- collection

// collectSNMP polls the configured OIDs and returns them as metrics.
//
// It ALWAYS reports snmp_up (1 or 0). That is load-bearing: PylonMon skips a
// vital rule whose metric is absent from a push, so a device that dies would
// otherwise just stop reporting and page nobody. Reporting an explicit 0 gives
// the operator something a rule can fire on — set `snmp_up < 1`.
func collectSNMP(c *snmpConfig) map[string]float64 {
	out := map[string]float64{}
	if c == nil || c.Target == "" || len(c.OIDs) == 0 {
		return out
	}
	names := make([]string, 0, len(c.OIDs))
	oids := make([]string, 0, len(c.OIDs))
	for name, oid := range c.OIDs {
		names = append(names, name)
		oids = append(oids, oid)
	}
	timeout := time.Duration(c.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	community := c.Community
	if community == "" {
		community = "public"
	}
	res, err := snmpGet(c.Target, community, oids, timeout)
	if err != nil {
		out["snmp_up"] = 0
		return out
	}
	out["snmp_up"] = 1
	for i, name := range names {
		want := oids[i]
		if !strings.HasPrefix(want, ".") {
			want = "." + want
		}
		v, ok := res[want]
		if !ok {
			continue // agent did not echo it back; not a reading
		}
		if f, isNum := numericValue(v); isNum {
			out[name] = f
		}
	}
	return out
}
