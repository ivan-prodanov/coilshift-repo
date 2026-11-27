// Package cmdrelay is the Direct commands relay (Phase 2 of direct mode, 2026-11-27): it lets a signed-in phone send
// Tesla's own signed vehicle-command messages (universalmessage.RoutableMessage, the bytes the phone would send over
// BLE) to the car's command-router, and returns the router's replies, without Tesla's servers.
//
// How (car-proven by the 2026-11-27 probes, tesla-dump/docs/direct-mode-no-pi-2026-11-27.md §7):
//   - in: a child process running as the authd user calls com.tesla.CommandRouter.RouteFromAuthd (the D-Bus policy
//     lets only authd call it). The router then treats the message like any other: the phone's key signature and
//     role are checked by the domain itself (VCSEC for closures, the infotainment dispatcher via authd for the rest);
//   - out: the router sends every reply for the authd source to com.tesla.Auth.RouteMessageBytes (authd logs
//     "Dropping message without registered handler" and nothing else). A root D-Bus monitor (BecomeMonitor) sees
//     those calls; the relay forwards the ones addressed to a routing address it handed out.
//
// Routing addresses are rewritten: each routing address a phone uses gets a fresh random car-side address before the
// message is injected, and replies are only ever matched against those, then given the phone's address back. The
// routing address is not covered by the message signatures, so neither end notices, and a phone can never receive
// a reply that is not for its own request (in particular never authd's own traffic). Only the VEHICLE_SECURITY and
// INFOTAINMENT domains are accepted.
package cmdrelay

import (
	"errors"
	"fmt"
)

// RoutableMessage field numbers (universal_message.proto) and Destination's.
const (
	fieldTo   = 6  // Destination to_destination
	fieldFrom = 7  // Destination from_destination
	fieldUUID = 51 // bytes uuid

	destDomain         = 1 // Domain (varint)
	destRoutingAddress = 2 // bytes

	DomainVehicleSecurity = 2
	DomainInfotainment    = 3
)

var errMsg = errors.New("cmdrelay: malformed message")

// field is one top-level protobuf field: its number, wire type and the byte range of the whole field (tag included)
// and of its value (for wire type 2, the payload without the length).
type field struct {
	num, wt          int
	start, end       int
	valStart, valEnd int
}

func uvarint(b []byte, i int) (uint64, int, error) {
	var v uint64
	for s := 0; s < 64; s += 7 {
		if i >= len(b) {
			return 0, 0, errMsg
		}
		c := b[i]
		i++
		v |= uint64(c&0x7f) << s
		if c < 0x80 {
			return v, i, nil
		}
	}
	return 0, 0, errMsg
}

// fields splits b into its top-level fields. Groups (wire types 3, 4) and anything malformed are refused.
func fields(b []byte) ([]field, error) {
	var out []field
	for i := 0; i < len(b); {
		start := i
		tag, j, err := uvarint(b, i)
		if err != nil {
			return nil, err
		}
		f := field{num: int(tag >> 3), wt: int(tag & 7), start: start}
		if f.num == 0 {
			return nil, errMsg
		}
		switch f.wt {
		case 0:
			f.valStart = j
			_, j, err = uvarint(b, j)
			if err != nil {
				return nil, err
			}
			f.valEnd = j
		case 1:
			j += 8
		case 5:
			j += 4
		case 2:
			n, k, err := uvarint(b, j)
			if err != nil || n > uint64(len(b)-k) {
				return nil, errMsg
			}
			f.valStart, f.valEnd = k, k+int(n)
			j = k + int(n)
		default:
			return nil, errMsg
		}
		if j > len(b) {
			return nil, errMsg
		}
		f.end = j
		out = append(out, f)
		i = j
	}
	return out, nil
}

// Dest is a parsed Destination: a domain or a routing address (the oneof); HasDomain/HasAddress say which.
type Dest struct {
	Domain         int
	RoutingAddress []byte
	HasDomain      bool
	HasAddress     bool
}

func parseDest(b []byte) (Dest, error) {
	fs, err := fields(b)
	if err != nil {
		return Dest{}, err
	}
	var d Dest
	for _, f := range fs {
		switch {
		case f.num == destDomain && f.wt == 0:
			v, _, err := uvarint(b, f.valStart)
			if err != nil || d.HasDomain || d.HasAddress {
				return Dest{}, errMsg
			}
			d.Domain, d.HasDomain = int(v), true
		case f.num == destRoutingAddress && f.wt == 2:
			if d.HasDomain || d.HasAddress {
				return Dest{}, errMsg
			}
			d.RoutingAddress, d.HasAddress = b[f.valStart:f.valEnd], true
		default:
			return Dest{}, errMsg
		}
	}
	return d, nil
}

// dest returns the one Destination field num of msg, and its field. Zero or several such fields is an error (protobuf
// would merge them; we refuse instead).
func dest(msg []byte, fs []field, num int) (Dest, field, error) {
	var found *field
	for i := range fs {
		if fs[i].num == num {
			if found != nil || fs[i].wt != 2 {
				return Dest{}, field{}, errMsg
			}
			found = &fs[i]
		}
	}
	if found == nil {
		return Dest{}, field{}, errMsg
	}
	d, err := parseDest(msg[found.valStart:found.valEnd])
	return d, *found, err
}

// encodeAddressDest is a Destination holding only routing_address = ra, as field num of a RoutableMessage.
func encodeAddressDest(num int, ra []byte) []byte {
	inner := append([]byte{destRoutingAddress<<3 | 2, byte(len(ra))}, ra...)
	return append([]byte{byte(num<<3 | 2), byte(len(inner))}, inner...)
}

// replaceField returns msg with field f replaced by repl (a complete encoded field).
func replaceField(msg []byte, f field, repl []byte) []byte {
	out := make([]byte, 0, len(msg)-(f.end-f.start)+len(repl))
	out = append(out, msg[:f.start]...)
	out = append(out, repl...)
	return append(out, msg[f.end:]...)
}

// MaxAddress is the longest routing address accepted from a phone (Tesla's are 16 bytes).
const MaxAddress = 32

// Outbound checks a phone's message and returns it with its from routing address replaced by carRA, and the phone's
// routing address. It refuses anything but one to_destination naming the VEHICLE_SECURITY or INFOTAINMENT domain and
// one from_destination holding a routing address of 1..MaxAddress bytes.
func Outbound(msg, carRA []byte) (out, phoneRA []byte, err error) {
	fs, err := fields(msg)
	if err != nil {
		return nil, nil, err
	}
	to, _, err := dest(msg, fs, fieldTo)
	if err != nil {
		return nil, nil, fmt.Errorf("to_destination: %w", err)
	}
	if !to.HasDomain || (to.Domain != DomainVehicleSecurity && to.Domain != DomainInfotainment) {
		return nil, nil, fmt.Errorf("cmdrelay: domain not allowed")
	}
	from, ff, err := dest(msg, fs, fieldFrom)
	if err != nil {
		return nil, nil, fmt.Errorf("from_destination: %w", err)
	}
	if !from.HasAddress || len(from.RoutingAddress) == 0 || len(from.RoutingAddress) > MaxAddress {
		return nil, nil, fmt.Errorf("cmdrelay: from_destination must be a routing address")
	}
	phoneRA = append([]byte(nil), from.RoutingAddress...)
	return replaceField(msg, ff, encodeAddressDest(fieldFrom, carRA)), phoneRA, nil
}

// ReplyAddress is the routing address a reply is for (its to_destination), or nil.
func ReplyAddress(msg []byte) []byte {
	fs, err := fields(msg)
	if err != nil {
		return nil
	}
	to, _, err := dest(msg, fs, fieldTo)
	if err != nil || !to.HasAddress {
		return nil
	}
	return to.RoutingAddress
}

// Inbound returns a reply with its to_destination routing address replaced by phoneRA.
func Inbound(msg, phoneRA []byte) ([]byte, error) {
	fs, err := fields(msg)
	if err != nil {
		return nil, err
	}
	to, tf, err := dest(msg, fs, fieldTo)
	if err != nil || !to.HasAddress {
		return nil, errMsg
	}
	return replaceField(msg, tf, encodeAddressDest(fieldTo, phoneRA)), nil
}

// MessageUUID is a message's uuid (RoutableMessage field 51), or nil.
func MessageUUID(msg []byte) []byte {
	fs, err := fields(msg)
	if err != nil {
		return nil
	}
	for _, f := range fs {
		if f.num == fieldUUID && f.wt == 2 && f.valEnd-f.valStart <= 64 {
			return msg[f.valStart:f.valEnd]
		}
	}
	return nil
}
