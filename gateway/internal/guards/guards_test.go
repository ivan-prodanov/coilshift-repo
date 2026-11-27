package guards

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sockServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return p
}

func prefs(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/prefs" || r.Method != "GET" {
			http.Error(w, "bad", 400)
			return
		}
		w.Write([]byte(body))
	}
}

func TestRouteOffClean(t *testing.T) {
	for _, body := range []string{
		`{"AdvertiseRoutes":[]}`,
		`{"AdvertiseRoutes":null}`,
		`{"AdvertiseRoutes":["0.0.0.0/0","::/0"]}`,
	} {
		s := sockServer(t, prefs(body))
		if err := RouteOff(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
	}
}

func TestRouteOnRefused(t *testing.T) {
	s := sockServer(t, prefs(`{"LoggedOutLogin":"owner@example.com","AdvertiseRoutes":["192.168.90.0/24"]}`))
	err := RouteOff(context.Background(), s)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "owner@example.com") || strings.Contains(err.Error(), "192.168.90") {
		t.Fatalf("leaks body: %v", err)
	}
}

func TestRouteOtherPrefixRefused(t *testing.T) {
	for _, body := range []string{
		`{"AdvertiseRoutes":["10.0.0.0/8"]}`,
		`{"AdvertiseRoutes":["0.0.0.0/0","10.0.0.0/8"]}`,
		`{"AdvertiseRoutes":["0.0.0.0/1"]}`,
		`{"AdvertiseRoutes":[5]}`,
		`{"AdvertiseRoutes":"0.0.0.0/0"}`,
	} {
		s := sockServer(t, prefs(body))
		if RouteOff(context.Background(), s) == nil {
			t.Fatalf("%s: want error", body)
		}
	}
}

func TestRouteSocketMissing(t *testing.T) {
	if RouteOff(context.Background(), "/nonexistent/x.sock") == nil {
		t.Fatal("want error")
	}
}

func TestRouteBadJSON(t *testing.T) {
	s := sockServer(t, prefs(`{not json`))
	if RouteOff(context.Background(), s) == nil {
		t.Fatal("want error")
	}
}

func TestRouteHTTP500(t *testing.T) {
	s := sockServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"AdvertiseRoutes":[]}`, 500)
	})
	if RouteOff(context.Background(), s) == nil {
		t.Fatal("want error")
	}
}

func TestRouteRedirectRefused(t *testing.T) {
	s := sockServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", 302)
	})
	if RouteOff(context.Background(), s) == nil {
		t.Fatal("want error")
	}
}

func TestRouteSlow(t *testing.T) {
	s := sockServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(600 * time.Millisecond):
		case <-r.Context().Done():
		}
		w.Write([]byte(`{"AdvertiseRoutes":[]}`))
	})
	start := time.Now()
	if RouteOff(context.Background(), s) == nil {
		t.Fatal("want error")
	}
	if d := time.Since(start); d > 580*time.Millisecond {
		t.Fatalf("took %v", d)
	}
}

func TestRouteOversizeBody(t *testing.T) {
	s := sockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"AdvertiseRoutes":[],"x":"`))
		w.Write([]byte(strings.Repeat("a", 2<<20)))
		w.Write([]byte(`"}`))
	})
	if RouteOff(context.Background(), s) == nil {
		t.Fatal("want error")
	}
}

// north offsets a point by m metres.
func north(p LatLon, m float64) LatLon {
	return LatLon{Lat: p.Lat + m/6371000*180/math.Pi, Lon: p.Lon}
}

var car = LatLon{Lat: 52.5, Lon: 13.4}

func TestDistance(t *testing.T) {
	d := DistanceM(52.5, 13.4, north(car, 100).Lat, 13.4)
	if math.Abs(d-100) > 0.01 {
		t.Fatalf("got %f", d)
	}
}

func TestValidLatLon(t *testing.T) {
	bad := [][2]float64{{math.NaN(), 1}, {1, math.NaN()}, {math.Inf(1), 1}, {1, math.Inf(-1)}, {91, 1}, {-91, 1}, {1, 181}, {1, -181}, {0, 0}}
	for _, b := range bad {
		if ValidLatLon(b[0], b[1]) {
			t.Fatalf("%v should be invalid", b)
		}
	}
	for _, g := range [][2]float64{{52.5, 13.4}, {90, 180}, {-90, -180}, {0, 1}, {1, 0}} {
		if !ValidLatLon(g[0], g[1]) {
			t.Fatalf("%v should be valid", g)
		}
	}
}

func TestRouteMissingKeyRefused(t *testing.T) {
	s := sockServer(t, prefs(`{"Hostname":"x"}`))
	if RouteOff(context.Background(), s) == nil {
		t.Fatal("missing AdvertiseRoutes must refuse")
	}
}
