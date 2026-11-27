// Package client is the gateway side of the camera root helper protocol
// (camhelper/proto). One Client is one helper session: closing it makes the
// helper release everything the session acquired.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"airgapp/gateway/camhelper/proto"
)

const (
	dialTimeout  = time.Second
	writeTimeout = 2 * time.Second
	eventBuffer  = 64
)

// ErrClosed is returned once the session is gone.
var ErrClosed = errors.New("camhelper: session closed")

// Error is a helper refusal; Code is one of the proto.Err* codes.
type Error struct{ Code string }

func (e *Error) Error() string { return "camhelper: " + e.Code }

// Code returns the helper error code of err ("" if err is not a refusal).
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// AUFunc receives one AU body (12-byte header + Annex-B access unit). It is
// called on the client's reader goroutine, must not block, and owns body.
type AUFunc func(port uint16, flags byte, body []byte)

type call struct {
	resp  proto.Response
	data  []byte
	total uint64
	pages []proto.ListPage
	err   error
	done  chan struct{}
}

// Client is one helper session.
type Client struct {
	conn net.Conn
	onAU AUFunc

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  uint32
	pending map[uint32]*call
	closed  bool

	events    chan proto.Event
	done      chan struct{}
	closeOnce sync.Once
}

// Dial connects to the helper socket at path.
func Dial(path string, onAU AUFunc) (*Client, error) {
	c, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, err
	}
	return New(c, onAU), nil
}

// New starts a session on an established connection.
func New(conn net.Conn, onAU AUFunc) *Client {
	c := &Client{
		conn: conn, onAU: onAU,
		pending: map[uint32]*call{},
		events:  make(chan proto.Event, eventBuffer),
		done:    make(chan struct{}),
	}
	go c.reader()
	return c
}

// Events delivers helper events. Delivery is best effort: when the buffer is
// full an event is dropped (status repeats every second anyway).
func (c *Client) Events() <-chan proto.Event { return c.events }

// Done is closed when the session ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close ends the session; the helper releases its resources. Idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.conn.Close()
	})
	return nil
}

func (c *Client) reader() {
	defer c.finish()
	r := bufio.NewReaderSize(c.conn, 256<<10)
	for {
		t, body, err := proto.ReadFrame(r, proto.MaxHelperFrame)
		if err != nil {
			return
		}
		switch t {
		case proto.TAU:
			if port, flags, ok := proto.AUPort(body); ok && c.onAU != nil {
				c.onAU(port, flags, body)
			}
		case proto.TEvent:
			var ev proto.Event
			if json.Unmarshal(body, &ev) == nil {
				select {
				case c.events <- ev:
				default:
				}
			}
		case proto.TResp:
			var resp proto.Response
			if json.Unmarshal(body, &resp) != nil {
				return
			}
			if cl := c.take(resp.ID); cl != nil {
				cl.resp = resp
				close(cl.done)
			}
		case proto.TData:
			id, total, data, err := proto.ParseData(body)
			if err != nil {
				return
			}
			if cl := c.take(id); cl != nil {
				cl.resp = proto.Response{ID: id, OK: true}
				cl.data, cl.total = data, total
				close(cl.done)
			}
		case proto.TList:
			id, page, err := proto.ParseList(body)
			if err != nil {
				return
			}
			var p proto.ListPage
			if json.Unmarshal(page, &p) != nil {
				return
			}
			c.mu.Lock()
			if cl := c.pending[id]; cl != nil {
				cl.pages = append(cl.pages, p)
			}
			c.mu.Unlock()
		}
	}
}

func (c *Client) take(id uint32) *call {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.pending[id]
	delete(c.pending, id)
	return cl
}

func (c *Client) finish() {
	c.Close()
	c.mu.Lock()
	c.closed = true
	for id, cl := range c.pending {
		cl.err = ErrClosed
		close(cl.done)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) write(t byte, body []byte, timeout time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if err := proto.WriteFrame(c.conn, t, body); err != nil {
		c.Close()
		return ErrClosed
	}
	return nil
}

// do sends req and waits for its reply or ctx.
func (c *Client) do(ctx context.Context, req proto.Request) (*call, error) {
	cl := &call{done: make(chan struct{})}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.nextID++
	req.ID = c.nextID
	c.pending[req.ID] = cl
	c.mu.Unlock()
	body, err := json.Marshal(req)
	if err != nil {
		c.take(req.ID)
		return nil, err
	}
	if err := c.write(proto.TReq, body, writeTimeout); err != nil {
		c.take(req.ID)
		return nil, err
	}
	select {
	case <-cl.done:
	case <-ctx.Done():
		c.take(req.ID)
		return nil, ctx.Err()
	}
	if cl.err != nil {
		return nil, cl.err
	}
	if !cl.resp.OK {
		code := cl.resp.Err
		if code == "" {
			code = proto.ErrInternal
		}
		return cl, &Error{Code: code}
	}
	return cl, nil
}

// Call performs one request and returns the helper's response (also on a
// refusal, which carries the error code and possibly a status).
func (c *Client) Call(ctx context.Context, req proto.Request) (proto.Response, error) {
	cl, err := c.do(ctx, req)
	if cl == nil {
		return proto.Response{}, err
	}
	return cl.resp, err
}

// status returns the status the helper attached, also with a refusal (a
// gated hold, talk_arm or sel answers with the readings behind it).
func (c *Client) status(ctx context.Context, req proto.Request) (proto.Status, error) {
	r, err := c.Call(ctx, req)
	if r.Status == nil {
		return proto.Status{}, err
	}
	return *r.Status, err
}

// Hold acquires (refcounted) WEBCAM_webcamSessionActive for this session.
func (c *Client) Hold(ctx context.Context) (proto.Status, error) {
	return c.status(ctx, proto.Request{Op: proto.OpHold})
}

// Release drops this session's hold (and its talk).
func (c *Client) Release(ctx context.Context) error {
	_, err := c.Call(ctx, proto.Request{Op: proto.OpRelease})
	return err
}

// TalkArm arms the exterior talk route and attaches the writer.
func (c *Client) TalkArm(ctx context.Context) (proto.Status, error) {
	return c.status(ctx, proto.Request{Op: proto.OpTalkArm})
}

// TalkDisarm ends talk. With noEdge the helper skips the sessionActive
// 0 -> 1 edge it otherwise fires to un-route the exterior stream while the
// hold stays: for a caller that releases the hold right after (SUMMON).
func (c *Client) TalkDisarm(ctx context.Context, noEdge bool) error {
	_, err := c.Call(ctx, proto.Request{Op: proto.OpTalkDisarm, NoEdge: noEdge})
	return err
}

// Gain sets the push-to-talk boost (-6 dB) or the native -20 dB.
func (c *Client) Gain(ctx context.Context, boost bool) error {
	_, err := c.Call(ctx, proto.Request{Op: proto.OpGain, Boost: boost})
	return err
}

// Sel holds the selectable camera (1 interior, 3 front: the windshield
// ultrawide; in SUMMON mode also 11 Grid and 9 Backup) or releases it (0).
func (c *Client) Sel(ctx context.Context, sel int) (proto.Status, error) {
	return c.status(ctx, proto.Request{Op: proto.OpSel, Sel: sel})
}

// Summon turns this session's SUMMON mode on (refused with summon_unverified
// unless the car's Summon session is live) or off (the selectable released).
func (c *Client) Summon(ctx context.Context, on bool) (proto.Status, error) {
	return c.status(ctx, proto.Request{Op: proto.OpSummon, On: on})
}

// CaptureStart starts streaming the AUs of port.
func (c *Client) CaptureStart(ctx context.Context, port int) error {
	_, err := c.Call(ctx, proto.Request{Op: proto.OpCapStart, Port: port})
	return err
}

// CaptureStop stops the capture of port.
func (c *Client) CaptureStop(ctx context.Context, port int) error {
	_, err := c.Call(ctx, proto.Request{Op: proto.OpCapStop, Port: port})
	return err
}

// Status reads the car gates and this session's state.
func (c *Client) Status(ctx context.Context) (proto.Status, error) {
	return c.status(ctx, proto.Request{Op: proto.OpStatus})
}

// WritePCM sends raw PCM to the talk writer (no reply).
func (c *Client) WritePCM(b []byte) error {
	if len(b) > proto.MaxPCM {
		return fmt.Errorf("camhelper: PCM chunk of %d bytes", len(b))
	}
	return c.write(proto.TPCM, b, time.Second)
}

// DashList returns the TeslaCam inventory.
func (c *Client) DashList(ctx context.Context) (proto.ListPage, error) {
	cl, err := c.do(ctx, proto.Request{Op: proto.OpDashList})
	if err != nil || cl == nil {
		return proto.ListPage{}, err
	}
	var all proto.ListPage
	for _, p := range cl.pages {
		all.Dirs = append(all.Dirs, p.Dirs...)
		all.Recent = append(all.Recent, p.Recent...)
	}
	return all, nil
}

// DashRead reads up to length bytes at offset from a TeslaCam path, or from a
// cut when cut is not empty. It returns the data and the file's total size.
func (c *Client) DashRead(ctx context.Context, path, cut string, offset int64, length int) ([]byte, int64, error) {
	cl, err := c.do(ctx, proto.Request{Op: proto.OpDashRead, Path: path, Cut: cut, Offset: offset, Length: length})
	if err != nil || cl == nil {
		return nil, 0, err
	}
	return cl.data, int64(cl.total), nil
}

// DashCut cuts [ss, ss+t] of path into a helper temp file and returns its
// token and size.
func (c *Client) DashCut(ctx context.Context, path string, ss, t float64) (string, int64, error) {
	r, err := c.Call(ctx, proto.Request{Op: proto.OpDashCut, Path: path, SS: ss, T: t})
	if err != nil {
		return "", 0, err
	}
	return r.Cut, r.Size, nil
}
