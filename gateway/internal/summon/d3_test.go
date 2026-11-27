package summon

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"airgapp/gateway/internal/carlink"
	"airgapp/gateway/internal/guards"
)

// The D3 tests: the 3 m floor and the car position's staleness (design §5.3, plan call 2). startRig places the car at
// (carLat, carLon), in P at speed 0. Positions around it are built by bearing and distance with destination, the
// spherical destination formula, which is not the haversine the engine measures with.

const (
	wantTooClose = "location_params_invalid"  // within 3 m of the car
	wantNoCarGPS = "invalid_vehicle_location" // no car position, or a stale one

	earthR = 6371000.0 // metres, as guards.DistanceM

	// absurdLat, absurdLon: finite, so Parse accepts them, yet guards.DistanceM from the car is NaN (the haversine
	// term is -0.43). The pre-flight ruling's example, (5.16e15, 193.4), is 546352.7 m from this car, not NaN.
	absurdLat = 5.65e17
	absurdLon = 193.4
)

// destination is the point metres from (lat, lon) on bearing, in degrees clockwise from north, on a sphere of
// radius earthR.
func destination(lat, lon, bearing, metres float64) (float64, float64) {
	const rad = math.Pi / 180
	delta := metres / earthR
	phi1, lam1, theta := lat*rad, lon*rad, bearing*rad
	phi2 := math.Asin(math.Sin(phi1)*math.Cos(delta) + math.Cos(phi1)*math.Sin(delta)*math.Cos(theta))
	lam2 := lam1 + math.Atan2(math.Sin(theta)*math.Sin(delta)*math.Cos(phi1),
		math.Cos(delta)-math.Sin(phi1)*math.Sin(phi2))
	return phi2 / rad, lam2 / rad
}

// nearCar is the point metres from the test car on bearing. It checks itself against guards.DistanceM (tested in
// guards), so a broken helper cannot make a floor test pass vacuously.
func nearCar(t *testing.T, bearing, metres float64) (float64, float64) {
	t.Helper()
	lat, lon := destination(carLat, carLon, bearing, metres)
	if d := guards.DistanceM(carLat, carLon, lat, lon); math.Abs(d-metres) > 1e-6 {
		t.Fatalf("test helper: the point %v m from the car on bearing %v is %v m from it", metres, bearing, d)
	}
	return lat, lon
}

// pinDrop is Tesla's cmd_find_me in Go to Target: the phone at (lat, lon), the goal at (glat, glon).
func pinDrop(t *testing.T, lat, lon, glat, glon float64) Message {
	t.Helper()
	return parseMsg(t, KindVerb, fmt.Sprintf(
		`{"msg_type":"autopark:cmd_find_me","latitude":%v,"longitude":%v,"accuracy":5,"goal_latitude":%v,"goal_longitude":%v,"client_version":2,"summon_type":"pin_drop","created:timestamp":1700000000123}`,
		lat, lon, glat, glon))
}

// deviceLocationAt is Tesla's device_location with the phone, and the goal, at (lat, lon).
func deviceLocationAt(t *testing.T, lat, lon float64) Message {
	t.Helper()
	return parseMsg(t, KindMsg, fmt.Sprintf(
		`{"msg_type":"autopark:device_location","latitude":%v,"longitude":%v,"accuracy":5,"altitude":-1,"altitude_accuracy":-1,"goal_latitude":%v,"goal_longitude":%v,"summon_type":"find_me","created:timestamp":1700000000300}`,
		lat, lon, lat, lon))
}

// startRigNoLocation starts the engine without the car's first location: none has arrived yet.
func startRigNoLocation(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	if err := r.e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.reset()
	return r
}

// idle advances the clock by ms (a multiple of 10) in 10 ms steps, and the D2 loop ticks at each. Nothing arrives.
func (r *rig) idle(ms uint64) {
	for range ms / 10 {
		r.step(10)
	}
}

// holdFor keeps a hold going for ms: the clock advances in steps of at most 10 ms, a fresh LIVE arrives every 50 ms
// (so D2 never trips), car runs at each step with the time since holdFor began (nil: the car sends nothing), and the
// D2 loop ticks.
func (r *rig) holdFor(ms uint64, car func(at uint64)) {
	for at := uint64(0); at < ms; {
		step := min(10, ms-at)
		at += step
		r.clk.Advance(step)
		if at%50 == 0 {
			r.e.Live(20*time.Millisecond, true)
		}
		if car != nil {
			car(at)
		}
		r.e.Tick()
	}
}

// echoes is a holdFor car that sends its heartbeat echo every 100 ms, and no location.
func (r *rig) echoes(at uint64) {
	if at%100 == 0 {
		r.e.CarFrame(carlink.ChannelUDP, carEcho)
	}
}

// release is the phone's release: Tesla's cmd_abort.
func (r *rig) release(t *testing.T) {
	t.Helper()
	r.e.Msg(abortMsg(t))
}

// pressForwarded presses m with a fresh tick, and fails the test unless the verb went to the car.
func (r *rig) pressForwarded(t *testing.T, m Message) {
	t.Helper()
	r.car.reset()
	r.phone.reset()
	r.e.Verb(m, 20*time.Millisecond, true, 1)
	if got := r.car.sends(); !slices.Equal(got, []string{string(m.Canonical)}) {
		t.Fatalf("car got %q, want the verb %s forwarded", got, m.Type)
	}
	if got := r.phone.got(); len(got) != 0 {
		t.Fatalf("phone got %v for a forwarded verb", got)
	}
}

// pressRefused presses m with a fresh tick, and fails the test unless the helper refused it with reason.
func (r *rig) pressRefused(t *testing.T, m Message, reason string) {
	t.Helper()
	r.car.reset()
	r.phone.reset()
	wasActive := r.e.VerbActive()
	r.e.Verb(m, 20*time.Millisecond, true, 1)
	if got := r.car.sends(); len(got) != 0 {
		t.Fatalf("car got %q, want the verb refused with %s", got, reason)
	}
	got := r.phone.got()
	if len(got) != 1 {
		t.Fatalf("phone got %v, want one refusal with %s", got, reason)
	}
	assertRefusal(t, got[0], m.Type, reason)
	if !wasActive && r.e.VerbActive() {
		t.Fatal("a refused verb set verbActive")
	}
}

func TestFloorPhoneWithin3m(t *testing.T) {
	shapes := []struct {
		name string
		at   func(t *testing.T, lat, lon float64) Message
	}{
		{"cmd_find_me", func(t *testing.T, lat, lon float64) Message {
			return verbAt(t, findMeType, lat, lon) // Come to Me: the goal is the phone
		}},
		{"cmd_find_me pin_drop with the goal 20 m away", func(t *testing.T, lat, lon float64) Message {
			glat, glon := nearCar(t, 300, 20)
			return pinDrop(t, lat, lon, glat, glon) // only the phone is near
		}},
		{"cmd_forward", func(t *testing.T, lat, lon float64) Message { return verbAt(t, forwardType, lat, lon) }},
		{"cmd_reverse", func(t *testing.T, lat, lon float64) Message { return verbAt(t, reverseType, lat, lon) }},
	}
	for _, s := range shapes {
		for _, bearing := range []float64{0, 90, 225} {
			t.Run(fmt.Sprintf("%s/bearing %v", s.name, bearing), func(t *testing.T) {
				r := startRig(t)
				lat, lon := nearCar(t, bearing, 2.99)
				r.pressRefused(t, s.at(t, lat, lon), wantTooClose)
				lat, lon = nearCar(t, bearing, 3.01)
				r.pressForwarded(t, s.at(t, lat, lon))
			})
		}
	}
}

func TestFloorGoalWithin3m(t *testing.T) {
	// Go to Target: the phone 20 m from the car, the goal near it.
	cases := []struct {
		goalM   float64
		refused bool
	}{{2, true}, {2.99, true}, {3.01, false}}
	for _, c := range cases {
		for _, bearing := range []float64{0, 90, 225} {
			t.Run(fmt.Sprintf("goal %v m/bearing %v", c.goalM, bearing), func(t *testing.T) {
				r := startRig(t)
				plat, plon := nearCar(t, bearing+90, 20)
				glat, glon := nearCar(t, bearing, c.goalM)
				m := pinDrop(t, plat, plon, glat, glon)
				if c.refused {
					r.pressRefused(t, m, wantTooClose)
				} else {
					r.pressForwarded(t, m)
				}
			})
		}
	}
}

func TestFloorUsesPhoneWhenNoGoal(t *testing.T) {
	// The arrows carry no goal keys, so the car takes the phone position as the goal (Appendix A §3.5).
	t.Run("goalOf", func(t *testing.T) {
		for _, typ := range []string{forwardType, reverseType} {
			if lat, lon := goalOf(verbAt(t, typ, 52.5201, 13.4051)); lat != 52.5201 || lon != 13.4051 {
				t.Fatalf("goalOf(%s) = (%v, %v), want the phone's (52.5201, 13.4051)", typ, lat, lon)
			}
		}
		if lat, lon := goalOf(pinDrop(t, 52.5201, 13.4051, 52.5202, 13.4052)); lat != 52.5202 || lon != 13.4052 {
			t.Fatalf("goalOf(pin_drop) = (%v, %v), want the goal (52.5202, 13.4052)", lat, lon)
		}
	})
	t.Run("cmd_forward with the car at (0, 0)", func(t *testing.T) {
		// An arrow's goal fields are 0. A floor that read them would put a car at (0, 0), the mock car's default
		// pose, 0 m from every arrow's goal.
		r := startRig(t)
		r.e.CarFrame(carlink.ChannelTCP, locationJSON(0, 0, "P", 0))
		lat, lon := destination(0, 0, 45, 20)
		for _, typ := range []string{forwardType, reverseType} {
			r.pressForwarded(t, verbAt(t, typ, lat, lon))
			r.release(t)
		}
		lat, lon = destination(0, 0, 45, 2)
		r.pressRefused(t, verbAt(t, forwardType, lat, lon), wantTooClose)
	})
}

func TestNoCarPositionRefused(t *testing.T) {
	for _, typ := range verbTypes {
		t.Run(typ, func(t *testing.T) {
			r := startRigNoLocation(t)
			// The car pushes its style and status on H1, and withholds its location while the heading is invalid
			// (Appendix A §8.6). An echo is no position either.
			r.e.CarFrame(carlink.ChannelTCP, []byte(`{"autopark_style":"dead_man","msg_type":"autopark:style"}`))
			r.e.CarFrame(carlink.ChannelTCP, []byte(`{"autopark_state":"standby","msg_type":"autopark:status","timestamp":1700000000000}`))
			r.e.CarFrame(carlink.ChannelUDP, carEcho)
			r.pressRefused(t, verb10m(t, typ), wantNoCarGPS)
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0))
			r.pressForwarded(t, verb10m(t, typ))
		})
	}
}

func TestNoCarPositionFromUnreadableLocation(t *testing.T) {
	// The car sends a location only when something changed, so a location the engine cannot read leaves it with no
	// car position. Qt writes a non-finite double as null.
	frames := []struct{ name, data string }{
		{"latitude null", `{"blinker_state":"off","heading":0,"latitude":null,"longitude":13.405,"msg_type":"vehicle_data:location","shift_state":"P","speed":0,"timestamp":1700000000000}`},
		{"no longitude", `{"blinker_state":"off","heading":0,"latitude":52.52,"msg_type":"vehicle_data:location","shift_state":"P","speed":0,"timestamp":1700000000000}`},
		{"speed null", `{"blinker_state":"off","heading":0,"latitude":52.52,"longitude":13.405,"msg_type":"vehicle_data:location","shift_state":"P","speed":null,"timestamp":1700000000000}`},
		{"shift_state a number", `{"blinker_state":"off","heading":0,"latitude":52.52,"longitude":13.405,"msg_type":"vehicle_data:location","shift_state":4,"speed":0,"timestamp":1700000000000}`},
	}
	for _, f := range frames {
		t.Run(f.name, func(t *testing.T) {
			r := startRig(t)
			r.e.CarFrame(carlink.ChannelTCP, []byte(f.data))
			r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0))
			r.pressForwarded(t, verb10m(t, forwardType))
		})
	}
}

func TestStaleWhileActiveNoFrames(t *testing.T) {
	cases := []struct {
		name  string
		ms    uint64
		stale bool
	}{{"1000 ms", 1000, false}, {"1001 ms", 1001, true}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			r.idle(5000) // silence before the press never counts
			r.pressForwarded(t, verb10m(t, forwardType))
			r.holdFor(c.ms, nil) // the car sends nothing: the research freeze
			r.release(t)
			// The car answers the release. Only a location frame makes a stale position current again.
			r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS))
			r.e.CarFrame(carlink.ChannelUDP, carEcho)
			r.idle(500)
			if !c.stale {
				r.pressForwarded(t, verb10m(t, forwardType))
				return
			}
			r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0))
			r.pressForwarded(t, verb10m(t, forwardType))
		})
	}
}

func TestStaleAfterMotionNoLocation(t *testing.T) {
	// The car rolls in N, so only the speed shows motion. It keeps echoing every 100 ms but sends no new location.
	cases := []struct {
		name  string
		speed float64
		ms    uint64
		stale bool
	}{
		{"speed 2, 1000 ms", 2, 1000, false},
		{"speed 2, 1001 ms", 2, 1001, true},
		{"speed -2, 1001 ms", -2, 1001, true}, // rolling backwards: the firmware's speed may be signed
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			r.pressForwarded(t, verb10m(t, findMeType))
			r.holdFor(100, r.echoes)
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "N", c.speed))
			r.holdFor(c.ms, r.echoes)
			r.release(t)
			r.idle(100)
			if c.stale {
				r.pressRefused(t, verb10m(t, findMeType), wantNoCarGPS)
			} else {
				r.pressForwarded(t, verb10m(t, findMeType))
			}
		})
	}
}

func TestNotStaleOutsideRun(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig)
	}{
		{"in P, never pressed", func(*testing.T, *rig) {}},
		{"in D, never pressed", func(t *testing.T, r *rig) {
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "D", 0))
		}},
		{"after a run", func(t *testing.T, r *rig) {
			r.pressForwarded(t, verb10m(t, forwardType))
			r.holdFor(200, r.echoes)
			r.release(t)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			c.setup(t, r)
			r.idle(60_000) // no verb active, and no car frame
			r.pressForwarded(t, verb10m(t, forwardType))
		})
	}
}

func TestSilenceBeforePressDoesNotCount(t *testing.T) {
	// This car sends nothing between holds. Both windows start at the press: D pins the location window too.
	for _, gear := range []string{"P", "D"} {
		t.Run("in "+gear, func(t *testing.T) {
			r := startRig(t)
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, gear, 0))
			r.idle(60_000)
			r.pressForwarded(t, verb10m(t, findMeType))
			r.holdFor(1000, nil) // within 1 s of the verb, with no car frame
			r.release(t)
			r.idle(500)
			r.pressForwarded(t, verb10m(t, findMeType))
		})
	}
}

func TestRepressAfterCarRefusal(t *testing.T) {
	r := startRig(t)
	r.pressForwarded(t, verb10m(t, findMeType))
	r.holdFor(50, nil)
	r.e.CarFrame(carlink.ChannelTCP, resultJSON(findMeType, false, "plugged_in", wallMS+50))
	r.holdFor(10, nil)
	r.release(t) // Tesla's app answers a refusal with its own abort
	r.idle(2000) // the car stands still: no location frame
	r.pressForwarded(t, verb10m(t, findMeType))
}

func TestStandingInDriveDuringRunGoesStale(t *testing.T) {
	// Plan call 2's remaining case: the car stands still in D or R, so it sends no new location. The position goes
	// stale 1 s after the last one, and stays stale after the release.
	for _, gear := range []string{"D", "R"} {
		t.Run(gear, func(t *testing.T) {
			r := startRig(t)
			r.pressForwarded(t, verb10m(t, forwardType))
			r.holdFor(50, r.echoes)
			r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, gear, 0))
			r.holdFor(1500, r.echoes)
			r.release(t)
			r.idle(100)
			r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
		})
	}
}

func TestStandingStillInPOrNStaysCurrent(t *testing.T) {
	// The car stands still in P or N at speed 0, so it sends no new location; its echoes keep the position current.
	cases := []struct {
		name string
		locs [][]byte
	}{
		{"P", [][]byte{locationJSON(carLat, carLon, "P", 0)}},
		{"N", [][]byte{locationJSON(carLat, carLon, "N", 0)}},
		{"after motion stops", [][]byte{locationJSON(carLat, carLon, "D", 2), locationJSON(carLat, carLon, "P", 0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			r.pressForwarded(t, verb10m(t, forwardType))
			for _, loc := range c.locs {
				r.holdFor(50, r.echoes)
				r.e.CarFrame(carlink.ChannelTCP, loc)
			}
			r.holdFor(1500, r.echoes)
			r.release(t)
			r.idle(100)
			r.pressForwarded(t, verb10m(t, forwardType))
		})
	}
}

func TestStaleIgnoresDroppedFrames(t *testing.T) {
	// Frames on a channel other than dcTcp and dcUdp are dropped: they keep no position current.
	r := startRig(t)
	r.pressForwarded(t, verb10m(t, forwardType))
	r.holdFor(1001, func(at uint64) {
		if at%100 == 0 {
			r.e.CarFrame("x", carEcho)
			r.e.CarFrame("", locationJSON(carLat, carLon, "P", 0))
		}
	})
	r.release(t)
	r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
}

func TestFloorIgnoresDroppedLocation(t *testing.T) {
	// A location on another channel is dropped: it does not move the car next to the phone.
	r := startRig(t)
	plat, plon := nearCar(t, 0, 10)
	clat, clon := destination(plat, plon, 0, 1)
	r.e.CarFrame("x", locationJSON(clat, clon, "P", 0))
	r.pressForwarded(t, verbAt(t, forwardType, plat, plon))
}

func TestFloorRunsAfterD2(t *testing.T) {
	// D3 runs after D2's checks: a verb that D2 refuses is refused with action_invalid.
	refusedOnce := func(t *testing.T, r *rig, typ, reason string) {
		t.Helper()
		got := r.phone.got()
		if len(got) != 1 {
			t.Fatalf("phone got %v, want one refusal", got)
		}
		assertRefusal(t, got[0], typ, reason)
	}
	t.Run("a stale tick from 1 m", func(t *testing.T) {
		r := startRig(t)
		lat, lon := nearCar(t, 0, 1)
		r.e.Verb(verbAt(t, forwardType, lat, lon), staleAge, true, 1)
		refusedOnce(t, r, forwardType, "action_invalid")
	})
	t.Run("a stale tick with no car position", func(t *testing.T) {
		r := startRigNoLocation(t)
		r.e.Verb(verb10m(t, forwardType), staleAge, true, 1)
		refusedOnce(t, r, forwardType, "action_invalid")
	})
	t.Run("from 1 m while a verb is active", func(t *testing.T) {
		r := startRig(t)
		r.pressForwarded(t, verb10m(t, findMeType))
		lat, lon := nearCar(t, 0, 1)
		r.pressRefused(t, verbAt(t, reverseType, lat, lon), "action_invalid")
	})
}

func TestFloorRefusesNaNDistance(t *testing.T) {
	// R27: Parse checks only that numbers are finite, and NaN < 3.0 is false. The floor fails closed. Since R28 an
	// absurd phone position or goal is refused for its range first; an absurd car position still reaches the distance.
	if d := guards.DistanceM(carLat, carLon, absurdLat, absurdLon); !math.IsNaN(d) {
		t.Fatalf("precondition: DistanceM from the car to (%v, %v) is %v, want NaN", absurdLat, absurdLon, d)
	}
	t.Run("the car position", func(t *testing.T) {
		if d := guards.DistanceM(absurdLat, absurdLon, tenMLat, carLon); !math.IsNaN(d) {
			t.Fatalf("precondition: DistanceM from (%v, %v) to the phone is %v, want NaN", absurdLat, absurdLon, d)
		}
		r := startRig(t)
		r.e.CarFrame(carlink.ChannelTCP, locationJSON(absurdLat, absurdLon, "P", 0))
		r.pressRefused(t, verb10m(t, forwardType), wantTooClose)
	})
	cases := []struct {
		name string
		m    func(t *testing.T) Message
	}{
		{"cmd_find_me", func(t *testing.T) Message { return verbAt(t, findMeType, absurdLat, absurdLon) }},
		{"cmd_find_me pin_drop, the phone", func(t *testing.T) Message {
			glat, glon := nearCar(t, 0, 20)
			return pinDrop(t, absurdLat, absurdLon, glat, glon)
		}},
		{"cmd_find_me pin_drop, the goal", func(t *testing.T) Message {
			plat, plon := nearCar(t, 0, 20)
			return pinDrop(t, plat, plon, absurdLat, absurdLon)
		}},
		{"cmd_forward", func(t *testing.T) Message { return verbAt(t, forwardType, absurdLat, absurdLon) }},
		{"cmd_reverse", func(t *testing.T) Message { return verbAt(t, reverseType, absurdLat, absurdLon) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			r.pressRefused(t, c.m(t), wantTooClose)
		})
	}
}

func TestDeviceLocationWithin3mForwarded(t *testing.T) {
	// device_location goes to the car as in Tesla's app, with no 3 m check and no car-position check (design §5.3).
	states := []struct {
		name  string
		start func(t *testing.T) *rig
	}{
		{"between runs", startRig},
		{"during a run", func(t *testing.T) *rig {
			r := startRig(t)
			r.pressForwarded(t, verb10m(t, findMeType))
			return r
		}},
		{"no car position", startRigNoLocation},
		{"a stale car position", func(t *testing.T) *rig {
			r := startRig(t)
			r.pressForwarded(t, verb10m(t, findMeType))
			r.holdFor(1001, nil)
			r.release(t)
			r.pressRefused(t, verb10m(t, findMeType), wantNoCarGPS) // it is stale
			return r
		}},
	}
	for _, s := range states {
		t.Run(s.name, func(t *testing.T) {
			r := s.start(t)
			lat1, lon1 := nearCar(t, 90, 1)
			lat2, lon2 := nearCar(t, 200, 2.99)
			msgs := []Message{
				deviceLocationAt(t, carLat, carLon), // exactly the car's GPS
				deviceLocationAt(t, lat1, lon1),
				deviceLocationAt(t, lat2, lon2),
			}
			r.car.reset()
			r.phone.reset()
			for _, m := range msgs {
				r.e.Msg(m)
			}
			want := []string{string(msgs[0].Canonical), string(msgs[1].Canonical), string(msgs[2].Canonical)}
			if got := r.car.sends(); !slices.Equal(got, want) {
				t.Fatalf("car got %q, want the three device_locations", got)
			}
			if got := r.phone.got(); len(got) != 0 {
				t.Fatalf("phone got %v", got)
			}
		})
	}
}

func TestFloorLogsCarryNoPositions(t *testing.T) {
	r := startRig(t)
	lat, lon := nearCar(t, 0, 1)
	r.pressRefused(t, verbAt(t, forwardType, lat, lon), wantTooClose)
	r.pressForwarded(t, verb10m(t, forwardType))
	r.holdFor(1001, nil) // stale: no car frame
	r.release(t)
	r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
	r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "D", 0)) // current again
	r.pressForwarded(t, verb10m(t, forwardType))
	r.holdFor(1001, r.echoes) // stale: standing in D
	r.release(t)
	r.e.CarFrame(carlink.ChannelTCP, []byte(`{"blinker_state":"off","heading":0,"latitude":null,"longitude":13.405,"msg_type":"vehicle_data:location","shift_state":"P","speed":0,"timestamp":1700000000000}`))
	r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
	lines := r.logLines()
	if len(lines) == 0 {
		t.Fatal("no log lines")
	}
	for _, l := range lines {
		for _, bad := range []string{"52.52", "13.40", "latitude", "longitude", "1700000000"} {
			if strings.Contains(l, bad) {
				t.Fatalf("log line %q contains %q", l, bad)
			}
		}
	}
}

// Fix round 1:
//   - the quiet time of holds adds up across holds, replacing call 2's per-hold windows (gear D or R at speed 0 keeps
//     its per-hold window);
//   - a release is checked as a tick is;
//   - a stale position stays stale through every phone input;
//   - positions outside WGS84's ranges are refused (R28).

func TestStaleCarriesAcrossFrozenHolds(t *testing.T) {
	// The stream froze before the car's first cmd_result, and Tesla's app keeps pressing: the frameless time of short
	// holds adds up. The 500 ms between holds never counts.
	cases := []struct {
		name  string
		holds []uint64
		stale bool
	}{
		{"400, 400 and 400 ms", []uint64{400, 400, 400}, true},
		{"400, 400 and 200 ms", []uint64{400, 400, 200}, false},
		{"400, 400 and 201 ms", []uint64{400, 400, 201}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			for _, ms := range c.holds {
				r.pressForwarded(t, verb10m(t, findMeType))
				r.holdFor(ms, nil) // no car frame
				r.release(t)
				r.idle(500)
			}
			if c.stale {
				r.pressRefused(t, verb10m(t, findMeType), wantNoCarGPS)
			} else {
				r.pressForwarded(t, verb10m(t, findMeType))
			}
		})
	}
}

func TestStaleCarriesAcrossTrips(t *testing.T) {
	// D2's trip ends a hold as the release does, and adds its frameless time: TripAfter + 1 ms per hold here, with no LIVE
	// after the press. The phone's own release after the trip adds nothing. fit trips stay within StaleAfter, one more
	// goes over it.
	fit := int(staleMS / (tripMS + 1))
	cases := []struct {
		trips int
		stale bool
	}{{fit, false}, {fit + 1, true}}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d trips", c.trips), func(t *testing.T) {
			r := startRig(t)
			for range c.trips {
				r.pressForwarded(t, verb10m(t, forwardType))
				r.idle(tripMS)
				r.step(1) // the trip
				if r.e.VerbActive() {
					t.Fatal("no trip TripAfter + 1 ms after the press")
				}
				r.release(t)
				r.idle(500)
			}
			if c.stale {
				r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
			} else {
				r.pressForwarded(t, verb10m(t, forwardType))
			}
		})
	}
}

func TestStaleCarryResetByCarFrame(t *testing.T) {
	// Any car frame resets the summed frameless time: a live stream never adds up.
	t.Run("the abort's cmd_result after each of 8 holds of 400 ms", func(t *testing.T) {
		r := startRig(t)
		for range 8 {
			r.pressForwarded(t, verb10m(t, findMeType))
			r.holdFor(400, nil)
			r.release(t)
			r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS))
			r.idle(100)
		}
		r.pressForwarded(t, verb10m(t, findMeType))
	})
	t.Run("4 holds ended by D2's trip, each answered by the swallowed cmd_result", func(t *testing.T) {
		r := startRig(t)
		for range 4 {
			r.pressForwarded(t, verb10m(t, forwardType))
			r.idle(tripMS)
			r.step(1) // the trip injects an abort: TripAfter + 1 ms carried
			// The car answers the injected abort: swallowed, but still a car frame. Its answer to the phone's own
			// abort has not come yet.
			r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS))
			r.release(t)
			r.idle(500)
		}
		r.pressForwarded(t, verb10m(t, forwardType))
	})
	t.Run("20 quick taps whose cmd_results arrive after the release", func(t *testing.T) {
		r := startRig(t)
		for range 20 {
			r.pressForwarded(t, verb10m(t, findMeType))
			r.holdFor(80, nil)
			r.release(t)
			r.idle(20)
			r.e.CarFrame(carlink.ChannelTCP, resultJSON(findMeType, true, "", wallMS))
			r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS))
		}
		r.pressForwarded(t, verb10m(t, findMeType))
	})
}

func TestStaleMotionCarriesAcrossHolds(t *testing.T) {
	// The last location showed motion, and the car echoes during each hold but sends no new location. At a speed other
	// than 0 the location-less time of holds adds up, until a location frame resets it. In D or R at speed 0 the
	// window stays per hold (plan call 2's remaining case).
	cases := []struct {
		name     string
		shift    string
		speed    float64
		holds    []uint64
		locAfter int // a new location frame arrives after this hold (1-based); 0: none
		stale    bool
	}{
		{"speed 2 in D: 400, 400 and 400 ms", "D", 2, []uint64{400, 400, 400}, 0, true},
		{"speed 2 in D: 400, 400 and 200 ms", "D", 2, []uint64{400, 400, 200}, 0, false},
		{"speed 2 in D: 400, 400 and 201 ms", "D", 2, []uint64{400, 400, 201}, 0, true},
		{"speed 2 in N: 400, 400 and 400 ms", "N", 2, []uint64{400, 400, 400}, 0, true},
		// The location after the 2nd hold resets the sum: 800 ms before it, 800 ms after it, and no location after
		// the last hold, so an unreset sum would show at the last press.
		{"speed 2 in D: a location after the 2nd of 4 holds", "D", 2, slices.Repeat([]uint64{400}, 4), 2, false},
		{"D at speed 0: 8 holds of 400 ms", "D", 0, slices.Repeat([]uint64{400}, 8), 0, false},
		{"R at speed 0: 8 holds of 400 ms", "R", 0, slices.Repeat([]uint64{400}, 8), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			loc := locationJSON(carLat, carLon, c.shift, c.speed)
			r.e.CarFrame(carlink.ChannelTCP, loc)
			for i, ms := range c.holds {
				r.pressForwarded(t, verb10m(t, forwardType))
				r.holdFor(ms, r.echoes)
				r.release(t)
				r.idle(500)
				if i+1 == c.locAfter {
					r.e.CarFrame(carlink.ChannelTCP, loc)
				}
			}
			if c.stale {
				r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
			} else {
				r.pressForwarded(t, verb10m(t, forwardType))
			}
		})
	}
}

func TestStaleCheckedAtRelease(t *testing.T) {
	// A release that lands between two D2 ticks, after a window expired, is checked as a tick would be.
	cases := []struct {
		name   string
		loc    []byte // sent right after the press, if any
		echoes bool   // the car echoes during the hold
	}{
		{"no car frame", nil, false},
		{"speed 2, echoes only", locationJSON(carLat, carLon, "N", 2), true},
		{"gear D at speed 0, echoes only", locationJSON(carLat, carLon, "D", 0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := startRig(t)
			r.pressForwarded(t, verb10m(t, forwardType))
			if c.loc != nil {
				r.e.CarFrame(carlink.ChannelTCP, c.loc)
			}
			var car func(uint64)
			if c.echoes {
				car = r.echoes
			}
			r.holdFor(1000, car) // the last tick, at 1000 ms, finds the position current
			r.clk.Advance(5)     // no tick before the release, at 1005 ms
			r.release(t)
			r.idle(100)
			r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
		})
	}
}

func TestStaleStaysUntilLocation(t *testing.T) {
	// Only a newer location frame makes a stale position current. No phone input between the refusal and the
	// re-press does: not the refusal itself, the abort, a heartbeat, a MSG, a LIVE, nor a verb that D2 refuses.
	r := startRig(t)
	r.pressForwarded(t, verb10m(t, findMeType))
	r.holdFor(1001, nil) // stale: no car frame
	r.release(t)
	r.e.CarFrame(carlink.ChannelTCP, resultJSON(abortType, true, "", wallMS)) // the car answers the release
	r.pressRefused(t, verb10m(t, findMeType), wantNoCarGPS)
	r.release(t) // Tesla's app answers the refusal with its own abort
	r.e.Heartbeat(heartbeatMsg(t), 20*time.Millisecond, true, 2)
	r.e.Msg(deviceLocationMsg(t))
	r.e.Msg(keepaliveMsg(t))
	r.e.Msg(infoMsg(t))
	r.e.Live(0, false)
	r.phone.reset()
	r.e.Verb(verb10m(t, findMeType), staleAge, true, 3) // a late tick: D2 refuses it
	got := r.phone.got()
	if len(got) != 1 {
		t.Fatalf("phone got %v, want D2's refusal", got)
	}
	assertRefusal(t, got[0], findMeType, "action_invalid")
	r.idle(200)
	r.e.Live(20*time.Millisecond, true) // the next press: native's LIVE can come before JavaScript's verb
	r.pressRefused(t, verb10m(t, findMeType), wantNoCarGPS)
	r.e.CarFrame(carlink.ChannelTCP, locationJSON(carLat, carLon, "P", 0))
	r.pressForwarded(t, verb10m(t, findMeType))
}

func TestFloorRefusesOutOfRange(t *testing.T) {
	// R28: a phone position or goal outside WGS84's ranges is refused whatever its distance, for every verb. Such
	// values could wrap onto the car's own position in the car's encoding, and Tesla's app never presses without a fix.
	out := []struct {
		name     string
		lat, lon float64
	}{
		{"latitude 90.000001", 90.000001, carLon},
		{"latitude -90.000001", -90.000001, carLon},
		{"longitude 180.000001", carLat, 180.000001},
		{"longitude -180.000001", carLat, -180.000001},
	}
	for _, o := range out {
		t.Run(o.name, func(t *testing.T) {
			r := startRig(t)
			flat, flon := nearCar(t, 0, 20)
			for _, m := range []Message{
				verbAt(t, findMeType, o.lat, o.lon), // the phone, and so the goal
				verbAt(t, forwardType, o.lat, o.lon),
				verbAt(t, reverseType, o.lat, o.lon),
				pinDrop(t, o.lat, o.lon, flat, flon), // the phone only
				pinDrop(t, flat, flon, o.lat, o.lon), // the goal only
			} {
				r.pressRefused(t, m, wantTooClose)
			}
		})
	}
}

func TestFloorAcceptsRangeEdges(t *testing.T) {
	// The edges themselves are in range: far from the car, they are forwarded.
	edges := []struct {
		name     string
		lat, lon float64
	}{
		{"latitude 90", 90, carLon},
		{"latitude -90", -90, carLon},
		{"longitude 180", carLat, 180},
		{"longitude -180", carLat, -180},
	}
	for _, e := range edges {
		t.Run(e.name, func(t *testing.T) {
			r := startRig(t)
			flat, flon := nearCar(t, 0, 20)
			for _, m := range []Message{
				verbAt(t, findMeType, e.lat, e.lon),
				verbAt(t, forwardType, e.lat, e.lon),
				verbAt(t, reverseType, e.lat, e.lon),
				pinDrop(t, e.lat, e.lon, flat, flon),
				pinDrop(t, flat, flon, e.lat, e.lon),
			} {
				r.pressForwarded(t, m)
				r.release(t)
			}
		})
	}
}

func TestStaleCarrySaturates(t *testing.T) {
	// A clock that ran backwards wraps a difference around. Added to a carry, a plain sum would wrap back under 1 s;
	// the sum saturates instead, so the position goes stale: fail closed.
	r := startRig(t)
	r.pressForwarded(t, verb10m(t, forwardType))
	r.holdFor(400, nil)
	r.release(t) // 400 ms carried
	r.idle(500)
	r.pressForwarded(t, verb10m(t, forwardType))
	r.clk.Advance(math.MaxUint64 - 4) // BOOTTIME goes 5 ms back
	r.e.Tick()
	r.release(t)
	r.pressRefused(t, verb10m(t, forwardType), wantNoCarGPS)
}
