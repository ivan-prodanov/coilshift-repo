package summon

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"
)

// The inner SUMMON session's frame types (Global Constraints). The phone sends LIVE, VERB, HB, MSG and PRESENCE; the
// helper sends CAR. They differ from the relay's own types (0x10, 0x90), so a frame sent to the wrong session is
// refused as grammar instead of being misread (plan call 4).
const (
	TypeLive     byte = 0x14 // tick id (8 bytes)
	TypeVerb     byte = 0x15 // tick id (8) ‖ press id (u32 BE) ‖ Tesla's cmd_find_me, cmd_forward or cmd_reverse
	TypeHB       byte = 0x16 // tick id (8) ‖ press id (u32 BE) ‖ Tesla's heartbeat_app_webrtc
	TypeMsg      byte = 0x17 // Tesla's cmd_abort, device_location, info or control:keepalive
	TypePresence byte = 0x18 // tick id (8 bytes): the phone's presence, about once a second, held or not (F5r)
	TypeCar      byte = 0x91 // channel (1 byte: ChanTCP for dcTcp, ChanUDP for dcUdp) ‖ the car's JSON, verbatim

	LoopEvery   = 10 * time.Millisecond  // the D2 loop's period
	MaxProgress = 250 * time.Millisecond // the helper is unhealthy once the D2 loop has not stamped for longer
	CarQueue    = 1024                   // car frames queued per session; a frame that finds the queue full ends it
	MaxCarJSON  = 16383                  // the largest car JSON forwarded; a larger one is dropped and logged

	// The presence lease (F5r), for a session whose inner ARM params opted in with {"presence":1}.
	PresenceFresh = 3 * time.Second  // the largest tick age whose echo stamps presence
	PresenceAfter = 10 * time.Second // the session ends once its newest stamp is older than this
)

const (
	tickLen   = 8           // a tick id
	motionLen = tickLen + 4 // a VERB or HB before its JSON: the tick id and the press id

	presenceAfterMS = uint64(PresenceAfter / time.Millisecond)
)

// decodeTick reads a LIVE or PRESENCE body (frame type t): exactly one tick id.
func decodeTick(t byte, body []byte) (tick [tickLen]byte, err error) {
	if len(body) != tickLen {
		return tick, fmt.Errorf("summon: a %s of %d bytes, want %d", frameName(t), len(body), tickLen)
	}
	copy(tick[:], body)
	return tick, nil
}

// decodeMotion reads a VERB or HB body (frame type t): the tick id, the press id, then Tesla's JSON, which aliases
// body.
func decodeMotion(t byte, body []byte) (tick [tickLen]byte, press uint32, msg []byte, err error) {
	if len(body) < motionLen {
		return tick, 0, nil, fmt.Errorf("summon: a %s of %d bytes, want at least %d", frameName(t), len(body), motionLen)
	}
	copy(tick[:], body)
	return tick, binary.BigEndian.Uint32(body[tickLen:motionLen]), body[motionLen:], nil
}

// encodeCar is a CAR body: the channel byte, then the car's JSON verbatim, in a buffer of its own.
func encodeCar(ch byte, data []byte) []byte {
	b := make([]byte, 1+len(data))
	b[0] = ch
	copy(b[1:], data)
	return b
}

// frameName names a phone frame type for logs and errors.
func frameName(t byte) string {
	switch t {
	case TypeLive:
		return "LIVE"
	case TypeVerb:
		return "VERB"
	case TypeHB:
		return "HB"
	case TypeMsg:
		return "MSG"
	case TypePresence:
		return "PRESENCE"
	}
	return fmt.Sprintf("frame 0x%02x", t)
}

// msgTypeOf is the msg_type of a JSON object, for a log line: "" when it has no string msg_type. Callers quote it
// with a bound (%.64q), since the sender chose it.
func msgTypeOf(data []byte) string {
	var head struct {
		MsgType any `json:"msg_type"`
	}
	if json.Unmarshal(data, &head) != nil {
		return ""
	}
	s, _ := head.MsgType.(string)
	return s
}
