// Package authproto is the camera root helper's sign-in socket: the gateway
// (uid 4950, which has no system bus access) asks the helper (root) for a
// phone key's KeyInfo row and for key-change notices. See
// docs/CAMERA-HELPER.md, "Sign-in socket".
//
// Framing is camhelper/proto's (u32 big-endian N, then one type byte and the
// body), on its own socket:
//
//	client -> helper  0x01 REQ    JSON Request (body at most MaxReq bytes)
//	helper -> client  0x81 RESP   JSON Response (body at most MaxResp), exactly one per REQ, in order
//	helper -> client  0x82 EVENT  JSON Event (body at most MaxEvent; watch connections only)
//
// Both sides are strict: JSON with an unknown or extra field, a version other
// than V, an unknown op or event, an oversized frame or an unknown frame type
// ends the exchange (the helper answers bad_request where it can, then closes;
// the gateway closes). The gateway also refuses a RESP whose id is not the
// next one it expects, re-checks kid == SHA-1(pk)[:4] and decides the role
// itself, so a helper bug cannot widen access.
//
// Neither side logs a full key: only kid or the key fingerprint
// (SHA-256(pk)[:8]).
//
// A connection is either a request connection (owner and key, any number,
// answered in order) or a watch connection (its first REQ is watch; nothing
// else is read from it). Only the gateway uid is served; the gateway should
// check that the helper's end is uid 0.
//
// Nothing the gateway sends reaches the bus: KeyInfo takes no arguments, and
// the key is only compared with authd's rows.
package authproto

import (
	"encoding/hex"
	"errors"

	"airgapp/gateway/camhelper/proto"
)

// DefaultSocket is where the helper serves sign-in on the car, beside the
// camera socket (same directory: root:<gw-gid> 0750; socket 0660).
const DefaultSocket = "/run/airgapp-camhelper/auth.sock"

// Frame types (camhelper/proto's).
const (
	TReq   = proto.TReq
	TResp  = proto.TResp
	TEvent = proto.TEvent
)

// V is the protocol version every Request carries.
const V = 1

// Limits (bodies; a frame is one type byte more).
const (
	MaxReq         = 1 << 10 // REQ
	MaxResp        = 512     // RESP
	MaxEvent       = 256     // EVENT
	MaxClientFrame = 1 + MaxReq
	MaxHelperFrame = 1 + MaxResp // the larger of RESP and EVENT
	MaxConns       = 8           // open connections, request and watch together
)

// Ops.
const (
	OpOwner = "owner" // com.tesla.Auth's unique owner runs as authd's uid
	OpKey   = "key"   // the owner check, then pk's KeyInfo row
	OpWatch = "watch" // key-change notices on this connection from now on
)

// Error codes in Response.Err. Every error denies.
const (
	ErrBadRequest  = "bad_request"   // malformed REQ, unknown op, bad pk, or a REQ after watch
	ErrRateLimited = "rate_limited"  // the helper's request budget is spent; retry later
	ErrTimeout     = "timeout"       // no answer within the helper's per-request budget
	ErrInternal    = "internal"      // the helper is shutting down or broken
	ErrAuth        = "auth_failed"   // an authority failure not classified below (Msg says which)
	ErrAuthOwner   = "auth_owner"    // com.tesla.Auth is not owned by authd's uid
	ErrKeyUnknown  = "key_unknown"   // pk is not in authd's KeyInfo list
	ErrKeyAmbig    = "key_ambiguous" // pk is listed more than once
	ErrShape       = "shape"         // KeyInfo's reply or row is not what the verified authd sends
	ErrBus         = "bus"           // no system bus connection, or a bus call failed
	ErrAuthdChange = "authd_changed" // /usr/bin/authd is not the verified build (re-verify KeyInfo)
)

// Event names.
const (
	EvKeysChanged = "keys_changed" // re-check every open window (Reason says why); also sent right after the state on attach
	EvWatchUp     = "watch_up"     // the helper's key-change subscription is live
	EvWatchDown   = "watch_down"   // it is not: changes cannot be pushed until watch_up
)

// keys_changed reasons. All three mean the same: changes may have happened.
const (
	ReasonKeysUpdated  = "keys_updated"  // authd's KeysUpdated signal
	ReasonOwnerChanged = "owner_changed" // com.tesla.Auth changed owner (authd restarted)
	ReasonResubscribed = "resubscribed"  // the subscription came back, or this watcher just attached; changes may have been missed
)

// Request is a REQ body.
type Request struct {
	V  int    `json:"v"` // always V
	ID uint32 `json:"id"`
	Op string `json:"op"`
	PK string `json:"pk,omitempty"` // key only (and required): 130 lowercase hex digits, 0x04||X||Y
}

// Response is a RESP body. Role, FF, Secs and KID are set for a key success.
type Response struct {
	ID   uint32 `json:"id"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
	Msg  string `json:"msg,omitempty"`  // the error's text, for logs
	Role uint32 `json:"role,omitempty"` // KeyInfo role (2 OWNER, 3 DRIVER, ...); the gateway decides
	FF   uint32 `json:"ff,omitempty"`   // KeyInfo form factor (6 iOS)
	Secs uint64 `json:"secs,omitempty"` // KeyInfo SecondsLeft (always 0 in the verified authd)
	KID  string `json:"kid,omitempty"`  // authd's 4-byte key Id, hex
}

// Event is an EVENT body.
type Event struct {
	Ev     string `json:"ev"`
	Reason string `json:"reason,omitempty"`
}

// ParsePK decodes a Request's pk: exactly 130 lowercase hex digits, 65
// bytes 0x04||X||Y.
func ParsePK(s string) ([]byte, error) {
	if len(s) != 130 {
		return nil, errors.New("pk must be 130 hex digits")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, errors.New("pk must be lowercase hex")
		}
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, errors.New("pk is not hex")
	}
	if b[0] != 0x04 {
		return nil, errors.New("pk must start with 04 (uncompressed P-256)")
	}
	return b, nil
}
