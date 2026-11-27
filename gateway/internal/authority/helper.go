package authority

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/camhelper/proto"
)

// ErrHelper: the sign-in helper gave no usable answer (it could not be
// reached, its end of the socket is not HelperUID, it did not answer within
// the budget, or it broke the protocol), or it answered with an error code
// that is none of the six classes: bad_request, rate_limited, timeout,
// internal, auth_failed, or a code this gateway does not know. It denies.
var ErrHelper = errors.New("sign-in helper")

// HelperError is the sign-in helper's refusal: Code is its error code
// (camhelper/authproto), Msg its text, for logs. It unwraps to the code's
// class (ErrAuthOwner, ErrKeyUnknown, ErrKeyAmbiguous, ErrShape, ErrBus,
// ErrAuthdChanged) or to ErrHelper.
type HelperError struct {
	Op   string // owner or key
	Code string
	Msg  string
}

func (e *HelperError) Error() string {
	s := "authority: sign-in helper: " + e.Op + ": " + e.Code
	if e.Msg != "" {
		s += ": " + strconv.Quote(e.Msg)
	}
	return s
}

func (e *HelperError) Unwrap() error {
	if c, ok := helperClasses[e.Code]; ok {
		return c
	}
	return ErrHelper
}

// helperClasses maps the helper's codes to the authority classes.
var helperClasses = map[string]error{
	authproto.ErrAuthOwner:   ErrAuthOwner,
	authproto.ErrKeyUnknown:  ErrKeyUnknown,
	authproto.ErrKeyAmbig:    ErrKeyAmbiguous,
	authproto.ErrShape:       ErrShape,
	authproto.ErrBus:         ErrBus,
	authproto.ErrAuthdChange: ErrAuthdChanged,
}

// helperKeeps reports whether the helper keeps serving a connection after
// answering with code. After bad_request it closes it (so do we), after
// internal it is shutting down or broken, and an unknown code means the
// stream cannot be trusted.
func helperKeeps(code string) bool {
	if _, ok := helperClasses[code]; ok {
		return true
	}
	switch code {
	case authproto.ErrRateLimited, authproto.ErrTimeout, authproto.ErrAuth:
		return true
	}
	return false
}

// HelperConfig tunes NewHelper. The zero value is the car's.
type HelperConfig struct {
	// HelperUID is the uid the helper's end of the socket must have: 0, the
	// root helper, on the car. Desk tests set their own.
	HelperUID int
	// PeerUID returns the uid at the other end of a unix connection
	// (default: the kernel's peer credentials, SO_PEERCRED/LOCAL_PEERCRED).
	PeerUID func(net.Conn) (int, error)
	// Budget caps each request, from the call (default 400 ms); a caller's
	// earlier deadline wins. It also bounds the watch's attach.
	Budget time.Duration
	// RedialEvery: at most one request-connection dial per this (default 1 s).
	RedialEvery time.Duration
	// MinBackoff and MaxBackoff: the watch's reconnect backoff (default 1 s
	// doubling to 10 s); StableAfter: a watch attached this long resets it
	// (default 30 s).
	MinBackoff, MaxBackoff, StableAfter time.Duration
	// Now is the clock of the re-dial limit (default time.Now).
	Now func() time.Time
	// Log gets connection trouble and protocol violations (default: none).
	// Keys appear in it only as kid (authd's 4-byte Id).
	Log *log.Logger
}

// Helper is the Oracle agw uses: the camera root helper's sign-in socket
// (camhelper/authproto), which answers from authd's KeyInfo as root, so agw
// itself never touches the system bus. It is lazy: nothing is dialled until a
// check. One persistent request connection carries every owner and key
// request, one at a time, each within Budget; any I/O error, timeout or
// protocol violation denies and closes it, and it is dialled again at most
// once per RedialEvery. Watch keeps its own connection.
//
// It never decides on the helper's word alone: a key answer must carry kid =
// SHA-1(pk)[:4], SecondsLeft 0 and a known role, and Check (not the helper)
// decides which roles pass.
type Helper struct {
	sock string
	cfg  HelperConfig
	log  *log.Logger

	lock chan struct{} // held for a whole request (a mutex a context can give up on)

	// Guarded by lock.
	hc      *helperConn
	nextID  uint32
	tried   time.Time // start of the last dial
	lastErr error     // why there is no connection
	closed  bool
}

var _ KeyEntryOracle = (*Helper)(nil)

// NewHelper returns the Oracle that asks the sign-in helper at socket
// (authproto.DefaultSocket on the car). It connects nothing.
func NewHelper(socket string, cfg HelperConfig) *Helper {
	if cfg.PeerUID == nil {
		cfg.PeerUID = peerUID
	}
	if cfg.Budget <= 0 {
		cfg.Budget = 400 * time.Millisecond
	}
	if cfg.RedialEvery <= 0 {
		cfg.RedialEvery = time.Second
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(10*time.Second, cfg.MinBackoff)
	}
	if cfg.StableAfter <= 0 {
		cfg.StableAfter = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	lg := cfg.Log
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Helper{sock: socket, cfg: cfg, log: lg, lock: make(chan struct{}, 1)}
}

// AuthOwnerIsAuthd asks op owner: true when the helper answers ok, false
// (and no error) for auth_owner, an error for anything else.
func (h *Helper) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	_, err := h.do(ctx, authproto.OpOwner, nil)
	var he *HelperError
	if errors.As(err, &he) && he.Code == authproto.ErrAuthOwner {
		return false, nil
	}
	return err == nil, err
}

// keyRoleChecksOwner: op key runs the owner check before it reads pk's row,
// on the unique name it then asks, and refuses without it (camhelper/
// authproto). So Check sends one request, not owner and then key: half the
// round trips, and half the wait for a check queued behind another.
func (*Helper) keyRoleChecksOwner() {}

// KeyRole is pk's role in authd's KeyInfo list, through the helper (op key,
// which runs the owner check first).
func (h *Helper) KeyRole(ctx context.Context, pk []byte) (int32, error) {
	e, err := h.KeyEntry(ctx, pk)
	if err != nil {
		return 0, err
	}
	return int32(e.Role), nil // at most 11
}

// KeyEntry is pk's KeyInfo row, through the helper (op key): the owner
// check, then exactly one row for the full 65-byte key. The answer must name
// pk's own kid, SecondsLeft 0 and a role of at most 11.
func (h *Helper) KeyEntry(ctx context.Context, pk []byte) (KeyInfoEntry, error) {
	if err := checkKey(pk); err != nil {
		return KeyInfoEntry{}, err
	}
	r, err := h.do(ctx, authproto.OpKey, pk)
	if err != nil {
		return KeyInfoEntry{}, err
	}
	return KeyInfoEntry{ID: KeyID(pk), PublicKey: append([]byte(nil), pk...), Role: r.Role, FormFactor: r.FF, SecondsLeft: r.Secs}, nil
}

// Close closes the request connection; later requests fail. Watch ends with
// its own context.
func (h *Helper) Close() error {
	h.lock <- struct{}{} // requests hold it at most Budget
	defer h.release()
	h.closed = true
	if h.hc != nil {
		h.hc.close(errors.New("closed"))
		h.hc = nil
	}
	return nil
}

func (h *Helper) acquire(ctx context.Context) error {
	select {
	case h.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Helper) release() { <-h.lock }

// helperFail is an ErrHelper error: no usable answer.
func helperFail(cause error, format string, a ...any) error {
	return fmt.Errorf("authority: %w: %s: %w", ErrHelper, fmt.Sprintf(format, a...), cause)
}

// who names a request in logs: the op, and for key the kid only.
func who(op string, pk []byte) string {
	if pk == nil {
		return op
	}
	return op + " kid=" + hex.EncodeToString(KeyID(pk))
}

// do sends one request and returns its answer, checked. A refusal is a
// *HelperError; anything else that is not a valid answer within the budget
// is an ErrHelper error and closes the connection.
func (h *Helper) do(ctx context.Context, op string, pk []byte) (authproto.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.Budget)
	defer cancel()
	if err := h.acquire(ctx); err != nil {
		return authproto.Response{}, helperFail(err, "%s: waiting for the request in flight", op)
	}
	defer h.release()
	if err := ctx.Err(); err != nil { // acquire may take the lock over a done ctx: touch nothing
		return authproto.Response{}, helperFail(err, "%s: no time left", op)
	}
	if h.closed {
		return authproto.Response{}, helperFail(errors.New("closed"), "%s", op)
	}
	hc, err := h.conn(ctx)
	if err != nil {
		return authproto.Response{}, helperFail(err, "%s", op)
	}
	h.nextID++
	req := authproto.Request{V: authproto.V, ID: h.nextID, Op: op}
	if pk != nil {
		req.PK = hex.EncodeToString(pk)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return authproto.Response{}, helperFail(err, "%s", op)
	}
	reply := hc.expect()
	deadline, _ := ctx.Deadline()
	hc.c.SetWriteDeadline(deadline)
	if err := proto.WriteFrame(hc.c, authproto.TReq, body); err != nil {
		h.drop(err)
		return authproto.Response{}, helperFail(err, "%s: sending", op)
	}
	var f helperFrame
	select {
	case f = <-reply:
	case <-hc.dead:
		select {
		case f = <-reply: // answered, then closed (as after bad_request)
		default:
			err := hc.cause()
			h.drop(err)
			return authproto.Response{}, helperFail(err, "%s: connection lost", op)
		}
	case <-ctx.Done():
		h.drop(ctx.Err())
		return authproto.Response{}, helperFail(ctx.Err(), "%s: no answer in time", op)
	}
	if ctx.Err() != nil { // an answer after the budget is no answer
		h.drop(ctx.Err())
		return authproto.Response{}, helperFail(ctx.Err(), "%s: answered late", op)
	}
	r, err := checkAnswer(f, req, pk)
	if err != nil {
		h.log.Printf("authority: sign-in helper broke the protocol (closing): %s: %v", who(op, pk), err)
		h.drop(err)
		return authproto.Response{}, helperFail(err, "%s: protocol", op)
	}
	if !r.OK {
		if !helperKeeps(r.Err) {
			h.drop(fmt.Errorf("answered %s", r.Err))
		}
		return r, &HelperError{Op: op, Code: r.Err, Msg: r.Msg}
	}
	return r, nil
}

// checkAnswer checks one RESP frame for req: well formed, req's id, and the
// fields its op allows. A refusal comes back with OK false and no error.
func checkAnswer(f helperFrame, req authproto.Request, pk []byte) (authproto.Response, error) {
	if f.t != authproto.TResp {
		return authproto.Response{}, fmt.Errorf("frame type %#x, want RESP", f.t)
	}
	r, err := parseResponse(f.body)
	if err != nil {
		return r, err
	}
	if r.ID != req.ID {
		return r, fmt.Errorf("RESP id %d, want %d", r.ID, req.ID)
	}
	if !r.OK {
		if !isCode(r.Err) {
			return r, fmt.Errorf("an error without a valid code (%q)", r.Err)
		}
		if r.Role != 0 || r.FF != 0 || r.Secs != 0 || r.KID != "" {
			return r, errors.New("an error with a KeyInfo row")
		}
		return r, nil
	}
	if r.Err != "" || r.Msg != "" {
		return r, errors.New("ok with an error code or text")
	}
	if req.Op != authproto.OpKey {
		if r.Role != 0 || r.FF != 0 || r.Secs != 0 || r.KID != "" {
			return r, fmt.Errorf("%s ok with a KeyInfo row", req.Op)
		}
		return r, nil
	}
	if want := hex.EncodeToString(KeyID(pk)); r.KID != want {
		return r, fmt.Errorf("kid %q, want %s (SHA-1 of the key asked)", r.KID, want)
	}
	if r.Secs != 0 {
		return r, fmt.Errorf("SecondsLeft %d, want 0", r.Secs)
	}
	if r.Role > maxRole {
		return r, fmt.Errorf("role %d unknown", r.Role)
	}
	return r, nil
}

// isCode accepts an error code's shape: 1 to 32 of [a-z_].
func isCode(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && c != '_' {
			return false
		}
	}
	return true
}

// drop closes the request connection (it is dialled again on demand).
func (h *Helper) drop(why error) {
	if h.hc != nil {
		h.hc.close(why)
		h.hc = nil
	}
	h.lastErr = why
}

// minDialTime: a request with less time left does not dial. It could not get
// its answer in time, and the one dial per RedialEvery is kept for a request
// that can use it.
const minDialTime = 50 * time.Millisecond

// conn returns the request connection, dialling one when there is none: at
// most once per RedialEvery, and only with minDialTime left. A connection the
// helper closed is noticed by its reader, so it is never written to.
func (h *Helper) conn(ctx context.Context) (*helperConn, error) {
	if h.hc != nil {
		select {
		case <-h.hc.dead:
			h.lastErr = fmt.Errorf("connection lost: %w", h.hc.cause())
			h.hc = nil
		default:
			return h.hc, nil
		}
	}
	if now := h.cfg.Now(); h.tried.IsZero() || now.Sub(h.tried) >= h.cfg.RedialEvery {
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < minDialTime {
			return nil, fmt.Errorf("not connected, and %v left is too little to dial", time.Until(dl).Round(time.Millisecond))
		}
		h.tried = now
		c, err := h.dial(ctx)
		if err != nil {
			h.lastErr = err
			return nil, err
		}
		h.hc = newHelperConn(c, h.log)
		return h.hc, nil
	}
	why := h.lastErr
	if why == nil {
		why = errors.New("no connection")
	}
	return nil, fmt.Errorf("not connected, dialling at most once per %v: %w", h.cfg.RedialEvery, why)
}

// dial connects to the helper and checks that its end runs as HelperUID.
func (h *Helper) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", h.sock)
	if err != nil {
		return nil, err
	}
	uid, err := h.cfg.PeerUID(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("peer credentials of %s: %w", h.sock, err)
	}
	if uid != h.cfg.HelperUID {
		c.Close()
		h.log.Printf("authority: REFUSING the sign-in helper at %s: it runs as uid %d, not %d", h.sock, uid, h.cfg.HelperUID)
		return nil, fmt.Errorf("%s is served by uid %d, not %d", h.sock, uid, h.cfg.HelperUID)
	}
	return c, nil
}

type helperFrame struct {
	t    byte
	body []byte
}

// helperConn is the request connection and its reader. The reader hands the
// one frame expected to the request in flight; a frame with no request in
// flight, a read error or EOF closes the connection at once.
type helperConn struct {
	c    net.Conn
	log  *log.Logger
	dead chan struct{} // closed when the reader has stopped
	once sync.Once

	mu   sync.Mutex
	want chan helperFrame // the request in flight's reply slot (buffered 1)
	err  error            // why it closed
}

func newHelperConn(c net.Conn, lg *log.Logger) *helperConn {
	hc := &helperConn{c: c, log: lg, dead: make(chan struct{})}
	go hc.read()
	return hc
}

func (hc *helperConn) read() {
	defer close(hc.dead)
	r := bufio.NewReader(hc.c)
	for {
		t, body, err := proto.ReadFrame(r, authproto.MaxHelperFrame) // RESP <= MaxResp
		if err != nil {
			hc.close(err)
			return
		}
		hc.mu.Lock()
		want := hc.want
		hc.want = nil
		hc.mu.Unlock()
		if want == nil {
			err := fmt.Errorf("a frame (type %#x) with no request in flight", t)
			hc.log.Printf("authority: sign-in helper broke the protocol (closing): %v", err)
			hc.close(err)
			return
		}
		want <- helperFrame{t, body}
	}
}

// expect makes the next frame the reply of the request about to be sent.
func (hc *helperConn) expect() chan helperFrame {
	ch := make(chan helperFrame, 1)
	hc.mu.Lock()
	hc.want = ch
	hc.mu.Unlock()
	return ch
}

func (hc *helperConn) close(why error) {
	hc.once.Do(func() {
		hc.mu.Lock()
		hc.err = why
		hc.mu.Unlock()
		hc.c.Close()
	})
}

func (hc *helperConn) cause() error {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.err == nil {
		return errors.New("closed")
	}
	return hc.err
}

// WatchHandlers receive what the helper's watch reports. Watch calls them on
// its own goroutine, one at a time; they must not block.
type WatchHandlers struct {
	// KeysChanged: re-check every open window (authproto keys_changed;
	// reason keys_updated, owner_changed or resubscribed).
	KeysChanged func(reason string)
	// State: true on watch_up (the helper's key-change subscription is live);
	// false on watch_down and whenever the watch connection is lost or cannot
	// be made, when changes can no longer be pushed.
	State func(up bool)
}

// Watch keeps a watch connection to the helper until ctx ends: op watch,
// then its events, strictly; anything else drops the connection. A lost
// connection reports State(false) and is made again after a backoff (MinBackoff
// doubling to MaxBackoff; reset by a connection that held StableAfter). Each
// attach starts with the helper's state and keys_changed "resubscribed".
// No handler runs after Watch returns.
func (h *Helper) Watch(ctx context.Context, wh WatchHandlers) {
	keys, state := wh.KeysChanged, wh.State
	if keys == nil {
		keys = func(string) {}
	}
	if state == nil {
		state = func(bool) {}
	}
	backoff := h.cfg.MinBackoff
	for {
		held, err := h.watchOnce(ctx, keys, state)
		if ctx.Err() != nil {
			return
		}
		state(false)
		if held >= h.cfg.StableAfter {
			backoff = h.cfg.MinBackoff // only a watch that held resets it: a flapping helper backs off
		}
		h.log.Printf("authority: sign-in watch down (%v); retrying in %v", err, backoff)
		t := time.NewTimer(backoff)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
		backoff = min(2*backoff, h.cfg.MaxBackoff)
	}
}

// watchOnce is one watch connection; held is how long it was attached.
func (h *Helper) watchOnce(ctx context.Context, keys func(string), state func(bool)) (held time.Duration, err error) {
	actx, cancel := context.WithTimeout(ctx, h.cfg.Budget)
	defer cancel()
	c, err := h.dial(actx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	deadline, _ := actx.Deadline()
	c.SetDeadline(deadline)
	b, _ := json.Marshal(authproto.Request{V: authproto.V, ID: 1, Op: authproto.OpWatch})
	if err := proto.WriteFrame(c, authproto.TReq, b); err != nil {
		return 0, fmt.Errorf("attach: %w", err)
	}
	r := bufio.NewReader(c)
	t, body, err := proto.ReadFrame(r, authproto.MaxHelperFrame)
	if err != nil {
		return 0, fmt.Errorf("attach: %w", err)
	}
	resp, err := checkAnswer(helperFrame{t, body}, authproto.Request{ID: 1, Op: authproto.OpWatch}, nil)
	if err == nil && !resp.OK {
		err = fmt.Errorf("refused: %s %q", resp.Err, resp.Msg)
	}
	if err != nil {
		h.log.Printf("authority: sign-in watch attach failed (closing): %v", err)
		return 0, fmt.Errorf("attach: %w", err)
	}
	c.SetDeadline(time.Time{})
	since := time.Now()
	for {
		t, body, err := proto.ReadFrame(r, 1+authproto.MaxEvent)
		if err != nil {
			return time.Since(since), err
		}
		ev, err := parseEvent(t, body)
		if err != nil {
			h.log.Printf("authority: sign-in watch broke the protocol (closing): %v", err)
			return time.Since(since), err
		}
		switch ev.Ev {
		case authproto.EvWatchUp:
			state(true)
		case authproto.EvWatchDown:
			state(false)
		case authproto.EvKeysChanged:
			keys(ev.Reason)
		}
	}
}

// parseEvent is strict: an EVENT frame whose JSON is a known event, a known
// reason with keys_changed and none with watch_up or watch_down.
func parseEvent(t byte, body []byte) (authproto.Event, error) {
	var ev authproto.Event
	if t != authproto.TEvent {
		return ev, fmt.Errorf("frame type %#x, want EVENT", t)
	}
	f, err := strictFields(body, "ev", "reason")
	if err != nil {
		return ev, err
	}
	raw, ok := f["ev"]
	if !ok || json.Unmarshal(raw, &ev.Ev) != nil {
		return ev, errors.New("ev must be a string")
	}
	if raw, ok := f["reason"]; ok && json.Unmarshal(raw, &ev.Reason) != nil {
		return ev, errors.New("reason must be a string")
	}
	switch ev.Ev {
	case authproto.EvWatchUp, authproto.EvWatchDown:
		if ev.Reason != "" {
			return ev, fmt.Errorf("%s with a reason", ev.Ev)
		}
	case authproto.EvKeysChanged:
		switch ev.Reason {
		case authproto.ReasonKeysUpdated, authproto.ReasonOwnerChanged, authproto.ReasonResubscribed:
		default:
			return ev, fmt.Errorf("keys_changed reason %q", ev.Reason)
		}
	default:
		return ev, fmt.Errorf("unknown event %q", ev.Ev)
	}
	return ev, nil
}

// parseResponse is strict: one JSON object with only Response's lowercase
// fields, each at most once and none null, id and ok present, every value of
// its exact JSON type (Go's decoder alone would accept other cases of a key,
// duplicates and nulls).
func parseResponse(body []byte) (authproto.Response, error) {
	var r authproto.Response
	f, err := strictFields(body, "id", "ok", "err", "msg", "role", "ff", "secs", "kid")
	if err != nil {
		return r, err
	}
	for _, k := range []string{"id", "ok"} {
		if _, ok := f[k]; !ok {
			return r, fmt.Errorf("RESP without %s", k)
		}
	}
	for k, dst := range map[string]any{"id": &r.ID, "ok": &r.OK, "err": &r.Err, "msg": &r.Msg,
		"role": &r.Role, "ff": &r.FF, "secs": &r.Secs, "kid": &r.KID} {
		if raw, ok := f[k]; ok {
			if err := json.Unmarshal(raw, dst); err != nil {
				return r, fmt.Errorf("RESP %s: %v", k, err)
			}
		}
	}
	return r, nil
}

// strictFields walks one JSON object: every key one of allowed, each at most
// once, no null value, nothing after the object.
func strictFields(body []byte, allowed ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	f := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("malformed JSON: %v", err)
		}
		k, _ := tok.(string)
		known := false
		for _, a := range allowed {
			known = known || k == a
		}
		if !known {
			return nil, fmt.Errorf("unknown field %q", k)
		}
		if _, dup := f[k]; dup {
			return nil, fmt.Errorf("duplicate field %q", k)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("malformed JSON: %v", err)
		}
		if string(raw) == "null" {
			return nil, fmt.Errorf("field %q is null", k)
		}
		f[k] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errors.New("unterminated JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return f, nil
}
