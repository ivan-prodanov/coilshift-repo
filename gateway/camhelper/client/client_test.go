package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"airgapp/gateway/camhelper/proto"
)

// A reply that arrives after its call timed out is dropped by request id; the
// session stays up and the next call gets its own reply.
func TestLateReplyIsDroppedByID(t *testing.T) {
	cs, ss := net.Pipe()
	c := New(cs, nil)
	defer c.Close()
	reqs := make(chan proto.Request, 4)
	go func() {
		r := bufio.NewReader(ss)
		for {
			typ, body, err := proto.ReadFrame(r, proto.MaxClientFrame)
			if err != nil {
				return
			}
			if typ == proto.TReq {
				var q proto.Request
				json.Unmarshal(body, &q)
				reqs <- q
			}
		}
	}()
	reply := func(id uint32, sel int) {
		b, _ := json.Marshal(proto.Response{ID: id, OK: true, Status: &proto.Status{Sel: sel}})
		proto.WriteFrame(ss, proto.TResp, b)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := c.Sel(ctx, 1)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a timeout, got %v", err)
	}
	first := <-reqs

	done := make(chan proto.Status, 1)
	go func() {
		st, err := c.Sel(context.Background(), 3)
		if err != nil {
			t.Error(err)
		}
		done <- st
	}()
	second := <-reqs
	reply(first.ID, 1)  // late: nobody waits for it
	reply(second.ID, 3) // the live call's own reply
	select {
	case st := <-done:
		if st.Sel != 3 {
			t.Fatalf("got the late reply: %+v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply")
	}
	select {
	case <-c.Done():
		t.Fatal("a late reply closed the session")
	default:
	}
}

func TestClosedSessionFailsPendingCalls(t *testing.T) {
	cs, ss := net.Pipe()
	c := New(cs, nil)
	go func() {
		r := bufio.NewReader(ss)
		proto.ReadFrame(r, proto.MaxClientFrame)
		ss.Close()
	}()
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	if _, err := c.Hold(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
}
