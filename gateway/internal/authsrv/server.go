// Package authsrv is the camera root helper's sign-in socket (camhelper/
// authproto): it answers the gateway's owner and key questions from authd's
// KeyInfo, as root, so the gateway uid never needs the system bus, and passes
// authd's key-change notices on. It never touches the camera queue or a car
// DataValue.
package authsrv

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
	"sync"
	"time"

	"airgapp/gateway/camhelper/authproto"
	"airgapp/gateway/camhelper/proto"
	"airgapp/gateway/internal/authority"
	"airgapp/gateway/internal/camhelper"
)

// Oracle is the authd surface the helper asks: authority.NewDBus's oracle.
type Oracle interface {
	AuthOwnerIsAuthd(ctx context.Context) (bool, error)
	KeyEntry(ctx context.Context, pk []byte) (authority.KeyInfoEntry, error)
}

// Config. Zero values take the defaults.
type Config struct {
	Oracle   Oracle
	AllowUID int                         // the only peer uid served (4950 on the car)
	PeerUID  func(net.Conn) (int, error) // default camhelper.PeerUID (SO_PEERCRED)
	Budget   time.Duration               // per request, from receipt (default 400 ms)
	Burst    int                         // token bucket size (default 20)
	PerSec   float64                     // token refill rate (default 10/s)
	MaxConns int                         // default authproto.MaxConns
	EventBuf int                         // events queued per watcher before it is cut (default 16)
	Now      func() time.Time
	Log      *log.Logger
}

const writeTimeout = 2 * time.Second

// Server serves one listener.
type Server struct {
	cfg Config
	log *log.Logger

	mu       sync.Mutex
	ln       net.Listener
	closing  bool
	conns    map[*conn]struct{}
	watchers map[*conn]struct{}
	up       bool // the key-change subscription is live
	tokens   float64
	last     time.Time
	wg       sync.WaitGroup
}

// New returns a server; Serve starts it.
func New(cfg Config) *Server {
	if cfg.PeerUID == nil {
		cfg.PeerUID = camhelper.PeerUID
	}
	if cfg.Budget <= 0 {
		cfg.Budget = 400 * time.Millisecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 20
	}
	if cfg.PerSec <= 0 {
		cfg.PerSec = 10
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = authproto.MaxConns
	}
	if cfg.EventBuf <= 0 {
		cfg.EventBuf = 16
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	lg := cfg.Log
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Server{cfg: cfg, log: lg, conns: map[*conn]struct{}{}, watchers: map[*conn]struct{}{},
		tokens: float64(cfg.Burst), last: cfg.Now()}
}

type conn struct {
	c      net.Conn
	wmu    sync.Mutex
	events chan authproto.Event // watchers only
	once   sync.Once
	gone   chan struct{}
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.gone)
		c.c.Close()
	})
}

// Serve accepts connections on ln until Shutdown.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		uid, err := s.cfg.PeerUID(c)
		if err != nil || uid != s.cfg.AllowUID {
			s.log.Printf("auth: refused peer uid=%d err=%v", uid, err)
			c.Close()
			continue
		}
		cn := &conn{c: c, gone: make(chan struct{})}
		s.mu.Lock()
		if s.closing || len(s.conns) >= s.cfg.MaxConns {
			s.mu.Unlock()
			s.log.Printf("auth: refused a connection: closing or %d open", s.cfg.MaxConns)
			c.Close()
			continue
		}
		s.conns[cn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.run(cn)
	}
}

// Shutdown closes the listener and every connection, and waits for them.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.closing = true
	ln := s.ln
	all := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, c := range all {
		c.close()
	}
	s.wg.Wait()
}

// Notify passes one key-change event to every watcher (the subscription's
// owner calls it). watch_up and watch_down also set the state a new watcher
// is told first. A watcher whose queue is full is closed: it must reconnect
// and treat that as a change.
func (s *Server) Notify(e authproto.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Ev {
	case authproto.EvWatchUp:
		s.up = true
	case authproto.EvWatchDown:
		s.up = false
	case authproto.EvKeysChanged:
	default:
		s.log.Printf("auth: dropping unknown event %q", e.Ev)
		return
	}
	for w := range s.watchers {
		select {
		case w.events <- e:
		default:
			s.log.Print("auth: watcher too slow (event queue full), closing it")
			delete(s.watchers, w)
			w.close()
		}
	}
}

func (s *Server) run(c *conn) {
	defer func() {
		c.close()
		s.mu.Lock()
		delete(s.conns, c)
		delete(s.watchers, c)
		s.mu.Unlock()
		s.wg.Done()
	}()
	r := bufio.NewReaderSize(c.c, 2*authproto.MaxClientFrame)
	served := false
	for {
		t, body, err := proto.ReadFrame(r, authproto.MaxClientFrame)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Printf("auth: closing a connection: %v", err)
			}
			return
		}
		if t != authproto.TReq {
			s.log.Printf("auth: closing a connection: frame type %#x", t)
			return
		}
		req, err := parse(body)
		if err != nil {
			s.reply(c, authproto.Response{ID: req.ID, Err: authproto.ErrBadRequest, Msg: err.Error()})
			s.log.Printf("auth: bad request (closing): %v", err)
			return
		}
		if req.Op == authproto.OpWatch {
			if served {
				s.reply(c, authproto.Response{ID: req.ID, Err: authproto.ErrBadRequest, Msg: "watch must be a connection's first request"})
				return
			}
			s.watch(c, r, req.ID)
			return
		}
		served = true
		if !s.reply(c, s.answer(req)) {
			return
		}
	}
}

// parse is strict: exactly one JSON object whose keys are the lowercase
// fields below, each at most once and none null; v, id and op present with
// their exact JSON types; version V; a known op; pk present exactly when the
// op is key, and well-formed. Go's own decoder would accept other cases and
// duplicate keys, so the object is walked token by token.
func parse(body []byte) (authproto.Request, error) {
	var req authproto.Request
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return req, errors.New("malformed request: not a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return req, fmt.Errorf("malformed request: %v", err)
		}
		k, _ := tok.(string)
		switch k {
		case "v", "id", "op", "pk":
		default:
			return req, fmt.Errorf("unknown field %q", k)
		}
		if _, dup := fields[k]; dup {
			return req, fmt.Errorf("duplicate field %q", k)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return req, fmt.Errorf("malformed request: %v", err)
		}
		if string(raw) == "null" {
			return req, fmt.Errorf("field %q is null", k)
		}
		fields[k] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return req, errors.New("malformed request: unterminated object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return req, errors.New("malformed request: data after the object")
	}
	if raw, ok := fields["id"]; !ok || json.Unmarshal(raw, &req.ID) != nil {
		return req, errors.New("id must be a uint32")
	}
	if raw, ok := fields["v"]; !ok || json.Unmarshal(raw, &req.V) != nil || req.V != authproto.V {
		return req, fmt.Errorf("v must be %d", authproto.V)
	}
	if raw, ok := fields["op"]; !ok || json.Unmarshal(raw, &req.Op) != nil {
		return req, errors.New("op must be a string")
	}
	raw, hasPK := fields["pk"]
	if hasPK && json.Unmarshal(raw, &req.PK) != nil {
		return req, errors.New("pk must be a string")
	}
	switch req.Op {
	case authproto.OpOwner, authproto.OpWatch:
		if hasPK {
			return req, fmt.Errorf("pk is only for %s", authproto.OpKey)
		}
	case authproto.OpKey:
		if _, err := authproto.ParsePK(req.PK); err != nil {
			return req, err
		}
	default:
		return req, fmt.Errorf("unknown op %q", req.Op)
	}
	return req, nil
}

// answer runs one owner or key request within the budget.
func (s *Server) answer(req authproto.Request) authproto.Response {
	resp := authproto.Response{ID: req.ID}
	var pk []byte
	who := "owner"
	switch req.Op {
	case authproto.OpOwner:
	case authproto.OpKey:
		var err error
		if pk, err = authproto.ParsePK(req.PK); err != nil {
			resp.Err, resp.Msg = authproto.ErrBadRequest, err.Error()
			return resp
		}
		who = "kid=" + hex.EncodeToString(authority.KeyID(pk))
	default:
		resp.Err, resp.Msg = authproto.ErrBadRequest, "unknown op"
		return resp
	}
	if !s.take() {
		resp.Err = authproto.ErrRateLimited
		s.log.Printf("auth: %s %s: rate limited", req.Op, who)
		return resp
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Budget)
	defer cancel()
	var err error
	if req.Op == authproto.OpOwner {
		var ok bool
		if ok, err = s.cfg.Oracle.AuthOwnerIsAuthd(ctx); err == nil && !ok {
			resp.Err = authproto.ErrAuthOwner
			resp.Msg = "com.tesla.Auth is not owned by authd"
		}
	} else {
		var e authority.KeyInfoEntry
		if e, err = s.cfg.Oracle.KeyEntry(ctx, pk); err == nil {
			if !bytes.Equal(e.PublicKey, pk) {
				err = errors.New("the oracle answered for another key")
			} else {
				resp.Role, resp.FF, resp.Secs, resp.KID = e.Role, e.FormFactor, e.SecondsLeft, hex.EncodeToString(e.ID)
			}
		}
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err() // an answer after the budget is no answer
	}
	switch {
	case err != nil:
		resp.Err, resp.Msg = classify(err), err.Error()
		resp.Role, resp.FF, resp.Secs, resp.KID = 0, 0, 0, ""
	case resp.Err == "":
		resp.OK = true
	}
	if resp.OK {
		s.log.Printf("auth: %s %s: ok role=%d", req.Op, who, resp.Role)
	} else {
		s.log.Printf("auth: %s %s: %s: %s", req.Op, who, resp.Err, resp.Msg)
	}
	return resp
}

// classify maps an authority error to a code: the request budget first (a
// bus call cut by it wraps both), then the class internal/authority wraps,
// and auth_failed for an error that wraps none (Msg has the text). Every
// code denies.
func classify(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return authproto.ErrTimeout
	}
	for _, c := range []struct {
		err  error
		code string
	}{
		{authority.ErrAuthdChanged, authproto.ErrAuthdChange},
		{authority.ErrAuthOwner, authproto.ErrAuthOwner},
		{authority.ErrKeyAmbiguous, authproto.ErrKeyAmbig},
		{authority.ErrKeyUnknown, authproto.ErrKeyUnknown},
		{authority.ErrShape, authproto.ErrShape},
		{authority.ErrBus, authproto.ErrBus},
	} {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return authproto.ErrAuth
}

// take spends one token of the shared bucket.
func (s *Server) take() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	if d := now.Sub(s.last); d > 0 {
		s.tokens += d.Seconds() * s.cfg.PerSec
		if s.tokens > float64(s.cfg.Burst) {
			s.tokens = float64(s.cfg.Burst)
		}
	}
	s.last = now
	if s.tokens < 1 {
		return false
	}
	s.tokens--
	return true
}

// reply writes one RESP. A Msg too long for MaxResp is cut.
func (s *Server) reply(c *conn, resp authproto.Response) bool {
	b, err := json.Marshal(resp)
	for err == nil && len(b) > authproto.MaxResp && resp.Msg != "" {
		resp.Msg = resp.Msg[:len(resp.Msg)/2]
		b, err = json.Marshal(resp)
	}
	if err != nil || len(b) > authproto.MaxResp {
		s.log.Printf("auth: cannot encode a response: %v", err)
		return false
	}
	return s.write(c, authproto.TResp, b)
}

func (s *Server) write(c *conn, t byte, b []byte) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := proto.WriteFrame(c.c, t, b); err != nil {
		c.close()
		return false
	}
	return true
}

// watch turns c into a watcher: the RESP, then the current state, then every
// Notify. Any frame from the client, or EOF, ends it.
//
// The watcher is registered before the RESP is written, so no Notify falls
// between the two, and it first gets the state and keys_changed
// "resubscribed": anything may have changed before it attached.
func (s *Server) watch(c *conn, r *bufio.Reader, id uint32) {
	c.events = make(chan authproto.Event, s.cfg.EventBuf+2)
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	first := authproto.Event{Ev: authproto.EvWatchDown}
	if s.up {
		first.Ev = authproto.EvWatchUp
	}
	c.events <- first
	c.events <- authproto.Event{Ev: authproto.EvKeysChanged, Reason: authproto.ReasonResubscribed}
	s.watchers[c] = struct{}{}
	s.mu.Unlock()
	if !s.reply(c, authproto.Response{ID: id, OK: true}) {
		return
	}
	s.log.Printf("auth: watcher attached (%s)", first.Ev)

	go func() { // the client may only close
		if _, _, err := proto.ReadFrame(r, authproto.MaxClientFrame); err == nil {
			s.log.Print("auth: a watcher sent a frame, closing it")
		}
		c.close()
	}()
	for {
		select {
		case e := <-c.events:
			b, _ := json.Marshal(e)
			if !s.write(c, authproto.TEvent, b) {
				return
			}
		case <-c.gone:
			return
		}
	}
}
