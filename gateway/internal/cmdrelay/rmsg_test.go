package cmdrelay

import (
	"bytes"
	"testing"
)

// pb builds protobuf bytes for tests.
func varint(v uint64) []byte {
	var b []byte
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}
func fBytes(num int, v []byte) []byte {
	return append(append(varint(uint64(num<<3|2)), varint(uint64(len(v)))...), v...)
}
func fVarint(num int, v uint64) []byte { return append(varint(uint64(num<<3)), varint(v)...) }

func domainDest(d int) []byte     { return fVarint(destDomain, uint64(d)) }
func addressDest(a []byte) []byte { return fBytes(destRoutingAddress, a) }

// request is the shape of a phone message: to a domain, from a routing address, a payload, a uuid (like the
// 2026-11-27 probes: SessionInfoRequest field 14, uuid field 51).
func request(domain int, from []byte) []byte {
	var m []byte
	m = append(m, fBytes(fieldTo, domainDest(domain))...)
	m = append(m, fBytes(fieldFrom, addressDest(from))...)
	m = append(m, fBytes(14, append(fBytes(1, bytes.Repeat([]byte{4}, 65)), fBytes(2, make([]byte, 16))...))...)
	return append(m, fBytes(51, bytes.Repeat([]byte{0xAB}, 16))...)
}

// reply is the shape of the router's reply: to a routing address, from a domain, status, request_uuid.
func reply(to []byte, domain int) []byte {
	var m []byte
	m = append(m, fBytes(fieldTo, addressDest(to))...)
	m = append(m, fBytes(fieldFrom, domainDest(domain))...)
	m = append(m, fBytes(12, fVarint(1, 1))...)
	return append(m, fBytes(50, bytes.Repeat([]byte{0xAB}, 16))...)
}

func TestOutboundRewritesOnlyTheFromAddress(t *testing.T) {
	phone := bytes.Repeat([]byte{0x11}, 16)
	car := bytes.Repeat([]byte{0x22}, 16)
	for _, dom := range []int{DomainVehicleSecurity, DomainInfotainment} {
		in := request(dom, phone)
		out, got, err := Outbound(in, car)
		if err != nil {
			t.Fatalf("domain %d: %v", dom, err)
		}
		if !bytes.Equal(got, phone) {
			t.Fatalf("phone address %x", got)
		}
		if !bytes.Equal(out, request(dom, car)) {
			t.Fatalf("domain %d: rewritten message differs:\n got %x\nwant %x", dom, out, request(dom, car))
		}
		// The input is left alone.
		if !bytes.Equal(in, request(dom, phone)) {
			t.Fatal("Outbound modified its input")
		}
	}
}

func TestOutboundRefuses(t *testing.T) {
	phone := bytes.Repeat([]byte{1}, 16)
	car := bytes.Repeat([]byte{2}, 16)
	bad := map[string][]byte{
		"empty":           nil,
		"authd domain":    request(5, phone),
		"broadcast":       request(0, phone),
		"energy":          request(7, phone),
		"to an address":   append(fBytes(fieldTo, addressDest(phone)), fBytes(fieldFrom, addressDest(phone))...),
		"from a domain":   append(fBytes(fieldTo, domainDest(3)), fBytes(fieldFrom, domainDest(2))...),
		"no from":         fBytes(fieldTo, domainDest(3)),
		"no to":           fBytes(fieldFrom, addressDest(phone)),
		"two to fields":   append(request(3, phone), fBytes(fieldTo, domainDest(2))...),
		"two from fields": append(request(3, phone), fBytes(fieldFrom, addressDest(phone))...),
		"empty address":   request(3, nil),
		"long address":    request(3, make([]byte, MaxAddress+1)),
		"truncated":       request(3, phone)[:20],
		"bad length":      append(request(3, phone), 0x0a, 0x7f),
		"group wire type": append(request(3, phone), 0x0b),
		"dest with both":  append(fBytes(fieldTo, append(domainDest(3), addressDest(phone)...)), fBytes(fieldFrom, addressDest(phone))...),
		"field zero":      append([]byte{0x00, 0x01}, request(3, phone)...),
	}
	for name, msg := range bad {
		if _, _, err := Outbound(msg, car); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReplyAddressAndInbound(t *testing.T) {
	car := bytes.Repeat([]byte{0x22}, 16)
	phone := bytes.Repeat([]byte{0x11}, 16)
	r := reply(car, DomainInfotainment)
	if got := ReplyAddress(r); !bytes.Equal(got, car) {
		t.Fatalf("ReplyAddress %x", got)
	}
	back, err := Inbound(r, phone)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, reply(phone, DomainInfotainment)) {
		t.Fatalf("Inbound:\n got %x\nwant %x", back, reply(phone, DomainInfotainment))
	}
	// A reply to a domain (not a routing address) or garbage has no address.
	for _, m := range [][]byte{nil, {0xff}, fBytes(fieldTo, domainDest(3))} {
		if ReplyAddress(m) != nil {
			t.Errorf("ReplyAddress(%x) not nil", m)
		}
		if _, err := Inbound(m, phone); err == nil {
			t.Errorf("Inbound(%x) accepted", m)
		}
	}
}

// The car's real reply from the 2026-11-27 probe had this field layout (to, from, status, request_uuid); a multi-byte
// tag (field 50, 51) must not confuse the parser.
func TestMultiByteTags(t *testing.T) {
	r := reply(bytes.Repeat([]byte{9}, 16), 2)
	fs, err := fields(r)
	if err != nil {
		t.Fatal(err)
	}
	var nums []int
	for _, f := range fs {
		nums = append(nums, f.num)
	}
	if len(nums) != 4 || nums[0] != 6 || nums[1] != 7 || nums[2] != 12 || nums[3] != 50 {
		t.Fatalf("fields %v", nums)
	}
}
