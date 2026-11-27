// Package carlink is the helper's side of the car's com.tesla.Webrtc D-Bus name.
//
// Bus is the seam: DBusBus is godbus on a real bus (the car's system bus, or a private one in tests and the bench),
// FakeBus is the in-memory double. Link owns the name on a Bus and makes the emits the helper may originate (H1, the
// phone's data, H4), each bounded by EmitDeadline.
package carlink

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The com.tesla.Webrtc contract (Appendix A §1.2) and the Link's limits.
const (
	WebrtcName      = "com.tesla.Webrtc"
	WebrtcPath      = "/Webrtc"
	WebrtcIface     = "com.tesla.Webrtc"
	PhoneUID        = "test_phone" // stock webrtc-comms' session uid
	ReplyKey        = "CarserverToPeerCommsData_Reply"
	MaxCarData      = 65536 // longest car frame data accepted, in bytes
	ChannelTCP      = "dcTcp"
	ChannelUDP      = "dcUdp"
	EmitDeadline    = 100 * time.Millisecond
	ExitEmitOverrun = 3 // the exit code after an emit overran EmitDeadline
)

// The members of com.tesla.Webrtc.
const (
	MethodCarserverToPeerCommsData = "CarserverToPeerCommsData" // (a{sv}) → a{sv}: QtCarServer calls it
	MethodRemoteToLocalSignaling   = "RemoteToLocalSignaling"   // (a{sv}) → u: QtCarServer calls it
	SignalLocalToRemoteSignaling   = "LocalToRemoteSignaling"   // a{sv}: H1 and H4
	SignalPeerCommsToCarserverData = "PeerCommsToCarserverData" // a{sv}: the phone's data
)

// authd's key-set change signal.
const (
	AuthName          = "com.tesla.Auth"
	AuthPath          = "/Auth"
	AuthIface         = "com.tesla.Auth"
	SignalKeysUpdated = "KeysUpdated"
)

// CarFrame is one CarserverToPeerCommsData call: the car's channel and uid, the caller's unique bus name, and data.
type CarFrame struct {
	Channel, UID, Sender string
	Data                 []byte
}

// Handlers receive the car's calls. Each may be nil.
type Handlers struct {
	CarFrame  func(CarFrame)            // in the car's call order; must not block
	Signaling func(body map[string]any) // RemoteToLocalSignaling, in order; must not block
	Reply     func() int32              // the CarserverToPeerCommsData_Reply value
}

// reply is the CarserverToPeerCommsData_Reply value: 0 without a Reply handler.
func (h Handlers) reply() int32 {
	if h.Reply == nil {
		return 0
	}
	return h.Reply()
}

// Bus is one connection to a message bus, as carlink uses it. Callbacks registered with OnNameLost and
// OnNameAcquired run one at a time, in the bus's order, and must not block.
type Bus interface {
	RequestName(name string, queue bool) (owned bool, err error) // queue=false: DO_NOT_QUEUE; true: flags 0
	ReleaseName(name string) error
	Emit(member string, body map[string]any) error
	Serve(h Handlers) error // exports both methods on /Webrtc once
	OnNameLost(name string, f func()) error
	OnNameAcquired(name string, f func()) error
	NameOwner(ctx context.Context, name string) (string, error) // org.freedesktop.DBus.GetNameOwner
	UniqueName() string
	Close() error
}

var (
	// ErrBusClosed: the Bus was closed.
	ErrBusClosed = errors.New("carlink: bus closed")

	errServedTwice = errors.New("carlink: Serve called twice")
)

// checkBody enforces the signal body contract every Bus shares: each value is a string or a bool, the only variant
// types QtCarServer gets from us.
func checkBody(body map[string]any) error {
	for k, v := range body {
		switch v.(type) {
		case string, bool:
		default:
			return fmt.Errorf("carlink: signal value %q has type %T, want string or bool", k, v)
		}
	}
	return nil
}

// frameOf checks one CarserverToPeerCommsData argument: uid, channel and data must be strings, and data at most
// MaxCarData bytes. On a refusal it says why, and gives the data's size (0 when it is not a string) for the log.
func frameOf(in map[string]any, sender string) (f CarFrame, drop string, size int) {
	data, ok := in["data"].(string)
	if !ok {
		return CarFrame{}, "no string data", 0
	}
	if len(data) > MaxCarData {
		return CarFrame{}, "data over MaxCarData", len(data)
	}
	channel, ok := in["channel"].(string)
	uid, ok2 := in["uid"].(string)
	if !ok || !ok2 {
		return CarFrame{}, "no string channel or uid", len(data)
	}
	return CarFrame{Channel: channel, UID: uid, Sender: sender, Data: []byte(data)}, "", len(data)
}
