// Package vcmd is the VCMD capability (Direct commands, direct mode Phase 2): it carries Tesla's own signed
// vehicle-command messages (universalmessage.RoutableMessage, the bytes the phone would send over BLE) between the
// phone and camhelper's command relay (internal/cmdrelay), which hands them to the car's command-router and returns
// the replies. agw never parses or changes them: the relay validates and rewrites routing addresses, and the car
// checks every message's signature and the key's role itself.
//
//	in  0x50 MSG    one RoutableMessage (at most MaxMsg bytes)
//	out 0xD0 MSG    one reply
//	    0xD1 ERR    body: a reason, then 0x00 and the message uuid: refused, rate, monitor, busy (not injected) or
//	                unknown (handed to the router, no answer: it may have run)
//	    0xD2 READY  the relay is up (its bus monitor sees replies); sent once
package vcmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/capability"
	"airgapp/gateway/internal/cmdrelay"
	"airgapp/gateway/internal/peercred"
)

const (
	ID        byte = 0x04
	TypeMsg   byte = 0x50
	TypeOut   byte = 0xD0
	TypeErr   byte = 0xD1
	TypeUp    byte = 0xD2
	MaxMsg         = cmdrelay.MaxMsg
	maxErr         = 64
	dialWait       = 500 * time.Millisecond
	writeWait      = time.Second
)

// Config is for tests; the zero value is the car's.
type Config struct {
	Socket    string // default cmdrelay.DefaultSocket
	HelperUID int    // the relay's server uid; default 0 (camhelper runs as root)
	Log       *log.Logger
}

type vcmd struct{ cfg Config }

// New returns the VCMD capability.
func New() capability.Capability { return NewWithConfig(Config{}) }

// NewWithConfig returns the VCMD capability with cfg.
func NewWithConfig(cfg Config) capability.Capability {
	if cfg.Socket == "" {
		cfg.Socket = cmdrelay.DefaultSocket
	}
	if cfg.Log == nil {
		cfg.Log = log.Default()
	}
	return &vcmd{cfg}
}

func (*vcmd) Spec() capability.Spec {
	return capability.Spec{
		Name:    "VCMD",
		ID:      ID,
		In:      map[byte]int{TypeMsg: MaxMsg},
		Out:     map[byte]int{TypeOut: MaxMsg, TypeErr: maxErr, TypeUp: 0},
		LeaseMS: 20000,
		Bulk:    map[byte]bool{},
	}
}

func (*vcmd) PreArm(context.Context, []byte) error { return nil }

// Open connects to the relay. The relay's READY (forwarded as 0xD2) comes once its monitor is up; MSGs sent before
// then wait in the socket.
func (v *vcmd) Open(ctx context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	d := net.Dialer{Timeout: dialWait}
	c, err := d.DialContext(ctx, "unix", v.cfg.Socket)
	if err != nil {
		return nil, fmt.Errorf("vcmd: relay: %w", err)
	}
	uid, err := peercred.PeerUID(c)
	if err != nil || uid != v.cfg.HelperUID {
		c.Close()
		return nil, fmt.Errorf("vcmd: relay server uid %d (want %d): %v", uid, v.cfg.HelperUID, err)
	}
	h := &handler{w: w, c: c, log: v.cfg.Log}
	go h.read()
	return h, nil
}

type handler struct {
	w   capability.Window
	c   net.Conn
	log *log.Logger

	wmu sync.Mutex
}

func (h *handler) Frame(t byte, body []byte) error {
	if t != TypeMsg {
		return nil
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	h.c.SetWriteDeadline(time.Now().Add(writeWait))
	if err := proto.WriteFrame(h.c, cmdrelay.TMsg, body); err != nil {
		return fmt.Errorf("vcmd: relay write: %w", err)
	}
	return nil
}

func (h *handler) Teardown(string) { h.c.Close() }

// read forwards the relay's frames to the phone until the relay or the window ends.
func (h *handler) read() {
	defer func() {
		if r := recover(); r != nil {
			h.log.Printf("vcmd: reader panic: %v", r)
			h.w.Close("error")
		}
	}()
	r := bufio.NewReader(h.c)
	for {
		t, body, err := proto.ReadFrame(r, 1+MaxMsg)
		if err != nil {
			select {
			case <-h.w.Done():
			default:
				if !errors.Is(err, net.ErrClosed) {
					h.log.Printf("vcmd: relay ended: %v", err)
				}
				h.w.Close("relay")
			}
			return
		}
		var out byte
		switch t {
		case cmdrelay.TReply:
			out = TypeOut
		case cmdrelay.TErr:
			out = TypeErr
			if len(body) > maxErr {
				body = body[:maxErr]
			}
		case cmdrelay.TReady:
			out = TypeUp
		default:
			continue
		}
		// A full priority lane ends the window rather than dropping a reply silently ("error": "drop" means the
		// connection was lost).
		if !h.w.TrySend(out, body) {
			h.w.Close("error")
			return
		}
	}
}
