// Package proto is the wire protocol between the gateway (uid 4950) and the
// camera root helper (camhelper) over its unix socket. See docs/CAMERA-HELPER.md.
//
// Framing, both directions: u32 big-endian N (1 <= N <= limit), then N bytes:
// one type byte and the body. A connection is one helper session: everything
// the session acquired is released, in the PLAN §4.9 order, when it closes.
//
// Client -> helper:
//
//	0x01 REQ   JSON Request
//	0x02 PCM   raw s16le mono 48 kHz for the talk writer (no reply; dropped
//	           unless this session armed talk)
//
// Helper -> client:
//
//	0x81 RESP  JSON Response (exactly one per REQ, except dash_read success)
//	0x82 EVENT JSON Event
//	0x83 AU    12-byte header (u16 port, u8 flags, u8 0, u64 capture µs) + one
//	           Annex-B access unit. The header has the size of the gateway's
//	           VIDEO header so the AU can be forwarded in place.
//	0x84 DATA  u32 id, u64 total, file bytes: the successful dash_read reply
//	0x85 LIST  u32 id, JSON ListPage: zero or more per dash_list, then RESP
package proto

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// DefaultSocket is where the helper listens on the car.
const DefaultSocket = "/run/airgapp-camhelper/camhelper.sock"

// SummonLivePath is summonhelper's live file: SUMMON mode's trust signal. summonhelper holds an exclusive flock on
// it while a Summon session is up whose phone opted into the presence lease (only a fresh presence echo keeps such
// a session alive); camhelper grants SUMMON mode only while that lock is held. Both sides use this one constant.
const SummonLivePath = "/run/airgapp-summon/live"

// Frame types.
const (
	TReq   byte = 0x01
	TPCM   byte = 0x02
	TResp  byte = 0x81
	TEvent byte = 0x82
	TAU    byte = 0x83
	TData  byte = 0x84
	TList  byte = 0x85
)

// Limits.
const (
	MaxClientFrame = 64 << 10       // client -> helper frame (type + body)
	MaxReqJSON     = 4 << 10        // REQ body
	MaxPCM         = 16 << 10       // PCM body
	AUHeader       = 12             // AU frame header (= the gateway's VIDEO header size)
	MaxAU          = 1<<20 - 12     // AU bytes: a VIDEO body (12 + AU) stays within 1 MiB
	MaxHelperFrame = 1 + 12 + MaxAU // helper -> client frame
	DataHeader     = 12             // u32 id + u64 total
	MaxRead        = 65536 - 20     // dash_read length: one DASHCAM CHUNK
)

// AU flags (byte 2 of the AU header; the same bits as the VIDEO header).
const (
	FlagIDR       byte = 0x01
	FlagParamSets byte = 0x02
)

// Ops.
const (
	OpHold       = "hold"        // hold WEBCAM_webcamSessionActive (refcounted)
	OpRelease    = "release"     // drop this session's hold (disarms its talk first)
	OpTalkArm    = "talk_arm"    // requestAudio 1, sessionActive edge, writer attached
	OpTalkDisarm = "talk_disarm" // requestAudio 0, edge (unless no_edge), gain restore, writer stopped
	OpGain       = "gain"        // boost true: -6 dB, false: -20 dB (talk owner only)
	OpSel        = "sel"         // GUI_selectableCameraRequest 1 (interior) or 3 (front) (parked), 0 = release; in SUMMON mode 11 (Grid), 9 (Backup) or 3 in any gear
	OpSummon     = "summon"      // on: SUMMON mode for this session (summonhelper's live file must be locked by it); off: back to the normal gates, the selectable released
	OpCapStart   = "cap_start"   // start the capture of an allowlisted port
	OpCapStop    = "cap_stop"
	OpStatus     = "status"
	OpDashList   = "dash_list"
	OpDashRead   = "dash_read" // path (or cut token), offset, length <= MaxRead
	OpDashCut    = "dash_cut"  // ffmpeg -c copy window [ss, ss+t] of path; returns a cut token
)

// Error codes in Response.Err.
const (
	ErrBadRequest   = "bad_request"
	ErrNotHeld      = "not_held"
	ErrNotArmed     = "not_armed"
	ErrBusy         = "busy"
	ErrGateGear     = "gate_gear"
	ErrGateSentry   = "gate_sentry"
	ErrGateCamera   = "gate_camera_access"
	ErrSelRefused   = "select_refused"
	ErrNoSummon     = "summon_unverified" // summon on: no live Summon session on the car (summonhelper's live file)
	ErrSummon       = "summon"            // talk_arm in SUMMON mode: talk never runs during SUMMON
	ErrExterior     = "exterior_busy"
	ErrPort         = "port_not_allowed"
	ErrSpawn        = "spawn_failed"
	ErrNotFound     = "not_found"
	ErrForbidden    = "forbidden_path"
	ErrCutFailed    = "cut_failed"
	ErrIO           = "io"
	ErrInternal     = "internal"
	ErrShuttingDown = "shutting_down"
)

// Event names.
const (
	EvStatus    = "status"     // periodic (1 Hz) to every session holding or owning something
	EvCapEnd    = "cap_end"    // a capture ended without cap_stop (Reason "exit" or "sel_end")
	EvHoldEnd   = "hold_end"   // the helper ended this session's hold (Reason: gate code)
	EvTalkEnd   = "talk_end"   // the helper ended this session's talk (Reason: gate code)
	EvSelEnd    = "sel_end"    // the helper ended this session's selectable hold
	EvRoute     = "route"      // the talk route came up ("ok") or went down ("down")
	EvSummonEnd = "summon_end" // the helper ended this session's SUMMON mode (Reason "live": the car's Summon session ended)
)

// Request is a REQ body. Fields not used by an op must be zero.
type Request struct {
	ID     uint32  `json:"id"`
	Op     string  `json:"op"`
	Port   int     `json:"port,omitempty"`
	Sel    int     `json:"sel,omitempty"`
	Boost  bool    `json:"boost,omitempty"`
	NoEdge bool    `json:"no_edge,omitempty"` // talk_disarm: no sessionActive 0->1 edge (the hold is released next)
	On     bool    `json:"on,omitempty"`      // summon: on or off
	Path   string  `json:"path,omitempty"`
	Cut    string  `json:"cut,omitempty"`
	Offset int64   `json:"offset,omitempty"`
	Length int     `json:"length,omitempty"`
	SS     float64 `json:"ss,omitempty"`
	T      float64 `json:"t,omitempty"`
}

// Response is a RESP body.
type Response struct {
	ID     uint32  `json:"id"`
	OK     bool    `json:"ok"`
	Err    string  `json:"err,omitempty"`
	Status *Status `json:"status,omitempty"`
	Cut    string  `json:"cut,omitempty"`
	Size   int64   `json:"size,omitempty"`
}

// Status is the helper's view of the car and of this session.
type Status struct {
	Gear      string `json:"gear"` // "P", "R", "N", "D" or "" (unknown)
	Driving   bool   `json:"driving"`
	Sentry    string `json:"sentry"`     // GUI_sentryModeState value, "" unknown
	Climate   string `json:"climate"`    // GUI_keepClimateOn value, "" unknown
	CamAccess string `json:"cam_access"` // GUI_mobileCameraAccessEnabled value, "" unknown
	Gate      string `json:"gate"`       // "" or the failing gate code
	Held      bool   `json:"held"`       // this session holds sessionActive
	Talk      bool   `json:"talk"`       // this session armed talk
	Route     string `json:"route"`      // "off" | "ok" | "down" (this session's talk route)
	Boost     bool   `json:"boost"`      // push-to-talk gain boost active
	GainDB    *int   `json:"gain_db"`    // last gain the helper set for this session's talk; nil = untouched
	Sel       int    `json:"sel"`        // selectable value this session holds, 0 = none
	Summon    bool   `json:"summon"`     // this session is in SUMMON mode (gear, Sentry and camera-access gates waived)
}

// Event is an EVENT body.
type Event struct {
	Ev     string  `json:"ev"`
	Port   int     `json:"port,omitempty"`
	Reason string  `json:"reason,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// DashFile is one regular file in a TeslaCam listing.
type DashFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// DashDir is one SentryClips/SavedClips event folder.
type DashDir struct {
	Type     string     `json:"type"`   // "SentryClips" | "SavedClips"
	Folder   string     `json:"folder"` // directory name
	HasEvent bool       `json:"has_event"`
	Event    []byte     `json:"event,omitempty"` // event.json bytes (<= 64 KiB)
	Files    []DashFile `json:"files"`           // *.mp4, sorted by name
}

// ListPage is one LIST frame's JSON.
type ListPage struct {
	Dirs   []DashDir  `json:"dirs,omitempty"`
	Recent []DashFile `json:"recent,omitempty"` // RecentClips/*.mp4, sorted by name
}

// ErrFrame is returned for a malformed or oversized frame.
var ErrFrame = errors.New("camhelper: bad frame")

// WriteFrame writes one frame with a single write call.
func WriteFrame(w io.Writer, t byte, body []byte) error {
	b := make([]byte, 5, 5+len(body))
	binary.BigEndian.PutUint32(b, uint32(1+len(body)))
	b[4] = t
	if len(body) <= 4096 {
		_, err := w.Write(append(b, body...))
		return err
	}
	bufs := net.Buffers{b, body}
	_, err := bufs.WriteTo(w)
	return err
}

// Encode returns a complete frame (length prefix included).
func Encode(t byte, body []byte) []byte {
	b := make([]byte, 5+len(body))
	binary.BigEndian.PutUint32(b, uint32(1+len(body)))
	b[4] = t
	copy(b[5:], body)
	return b
}

// ReadFrame reads one frame of at most max bytes (type + body). The body is
// freshly allocated and owned by the caller.
func ReadFrame(r *bufio.Reader, max int) (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > uint32(max) {
		return 0, nil, fmt.Errorf("%w: length %d", ErrFrame, n)
	}
	t, err := r.ReadByte()
	if err != nil {
		return 0, nil, unexpected(err)
	}
	body := make([]byte, n-1)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, unexpected(err)
	}
	return t, body, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// PutAUHeader fills the 12-byte AU header at the start of b.
func PutAUHeader(b []byte, port uint16, flags byte, captureUS uint64) {
	binary.BigEndian.PutUint16(b[0:2], port)
	b[2] = flags
	b[3] = 0
	binary.BigEndian.PutUint64(b[4:12], captureUS)
}

// AUPort returns the port of an AU body; ok is false for a short body.
func AUPort(body []byte) (port uint16, flags byte, ok bool) {
	if len(body) < AUHeader {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(body[0:2]), body[2], true
}

// PutDataHeader fills the 12-byte DATA header at the start of b.
func PutDataHeader(b []byte, id uint32, total uint64) {
	binary.BigEndian.PutUint32(b[0:4], id)
	binary.BigEndian.PutUint64(b[4:12], total)
}

// ParseData splits a DATA body.
func ParseData(body []byte) (id uint32, total uint64, data []byte, err error) {
	if len(body) < DataHeader {
		return 0, 0, nil, ErrFrame
	}
	return binary.BigEndian.Uint32(body[0:4]), binary.BigEndian.Uint64(body[4:12]), body[12:], nil
}

// ParseList splits a LIST body into the request id and the JSON page.
func ParseList(body []byte) (id uint32, page []byte, err error) {
	if len(body) < 4 {
		return 0, nil, ErrFrame
	}
	return binary.BigEndian.Uint32(body[0:4]), body[4:], nil
}

// ListBody builds a LIST body.
func ListBody(id uint32, page []byte) []byte {
	b := make([]byte, 4+len(page))
	binary.BigEndian.PutUint32(b, id)
	copy(b[4:], page)
	return b
}
