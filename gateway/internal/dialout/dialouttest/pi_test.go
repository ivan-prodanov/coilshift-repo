package dialouttest

import (
	"errors"
	"io"
	"log"
	"net"
	"testing"
	"time"

	"airgapp/gateway/internal/dialout"
)

// dialCar runs the real dialling listener against p until the test ends.
func dialCar(t *testing.T, p *Pi) *dialout.Listener {
	t.Helper()
	l, err := dialout.Listen(dialout.Config{Addr: p.CarAddr(), Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// waitIdle waits up to d for p to hold n idle car connections.
func waitIdle(t *testing.T, p *Pi, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for p.Idle() != n {
		if time.Now().After(deadline) {
			t.Fatalf("Idle() = %d after %v, want %d", p.Idle(), d, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readExactly(t *testing.T, c net.Conn, want string) {
	t.Helper()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != want {
		t.Fatalf("read %q, %v; want %q", got, err, want)
	}
}

func TestPhonesPairWithDialledCars(t *testing.T) {
	p := Start(t)
	l := dialCar(t, p)
	waitIdle(t, p, 3, time.Second)
	for i := range 2 { // a second phone gets a car connection of its own
		phone, err := net.Dial("tcp", p.PhoneAddr())
		if err != nil {
			t.Fatal(err)
		}
		phone.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := phone.Write([]byte("hi")); err != nil {
			t.Fatal(err)
		}
		acc := make(chan net.Conn, 1)
		go func() {
			c, _ := l.Accept() // nil after Close
			acc <- c
		}()
		var gw net.Conn
		select {
		case gw = <-acc:
		case <-time.After(2 * time.Second):
		}
		if gw == nil {
			t.Fatalf("phone %d: no paired connection reached the gateway", i)
		}
		gw.SetDeadline(time.Now().Add(2 * time.Second))
		readExactly(t, gw, "hi")
		if _, err := gw.Write([]byte("yo")); err != nil {
			t.Fatal(err)
		}
		readExactly(t, phone, "yo")
		phone.Close()
		if n, err := gw.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("phone %d closed: the gateway read %d bytes, %v; want EOF", i, n, err)
		}
		gw.Close()
	}
	waitIdle(t, p, 3, time.Second)
}

func TestPhoneClosedWithoutCar(t *testing.T) {
	p := Start(t)
	p.CarWait = 100 * time.Millisecond
	phone, err := net.Dial("tcp", p.PhoneAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	began := time.Now()
	phone.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := phone.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("the phone read %d bytes, %v; want EOF", n, err)
	}
	if d := time.Since(began); d < 80*time.Millisecond || d > 500*time.Millisecond {
		t.Fatalf("the phone was closed after %v, want about CarWait (100 ms)", d)
	}
}

func TestCarNeedsTheHello(t *testing.T) {
	p := Start(t)
	bad, err := net.Dial("tcp", p.CarAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if _, err := bad.Write([]byte("WRONGHEL")); err != nil {
		t.Fatal(err)
	}
	bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := bad.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after a wrong hello the car read %d bytes, %v; want EOF", n, err)
	}
	if n := p.Idle(); n != 0 {
		t.Fatalf("Idle() = %d after a wrong hello, want 0", n)
	}

	good, err := net.Dial("tcp", p.CarAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	if _, err := good.Write([]byte(dialout.Hello)); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, p, 1, time.Second)
}
