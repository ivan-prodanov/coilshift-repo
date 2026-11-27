package carlink

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

const (
	fakeUnique = ":fake.1"   // a FakeBus's own unique name
	fakeCar    = ":fake.car" // the Sender of a FakeBus's car calls
)

// Emission is one signal emitted on a FakeBus.
type Emission struct {
	Member string
	Body   map[string]any
}

// FakeBus is an in-memory Bus for one connection, for tests and the bench. It records emissions in order, lets a
// test (or a mock QtCarServer) make the car's calls, and takes or loses names. It cannot queue for a name:
// RequestName(name, true) is an error, and queueing is tested on a real bus (bustest).
type FakeBus struct {
	mu        sync.Mutex
	owned     map[string]bool
	taken     map[string]bool
	handlers  *Handlers
	lost      map[string][]func()
	acquired  map[string][]func()
	emissions []Emission
	observers []func(string, map[string]any)
	pending   []Emission
	draining  bool
	closed    bool
	emitErr   error
	stall     <-chan struct{}
}

// NewFakeBus returns an empty FakeBus.
func NewFakeBus() *FakeBus {
	return &FakeBus{
		owned:    map[string]bool{},
		taken:    map[string]bool{},
		lost:     map[string][]func(){},
		acquired: map[string][]func(){},
	}
}

func copyBody(b map[string]any) map[string]any {
	c := make(map[string]any, len(b))
	for k, v := range b {
		c[k] = v
	}
	return c
}

func runAll(cbs []func()) {
	for _, cb := range cbs {
		cb()
	}
}

// RequestName grants name unless TakeName marked it as held elsewhere (DO_NOT_QUEUE: no queueing, no replacement).
// Getting the name delivers NameAcquired first, as the real bus does.
func (f *FakeBus) RequestName(name string, queue bool) (bool, error) {
	if queue {
		return false, errors.New("carlink: a FakeBus cannot queue for a name; test queueing on bustest")
	}
	f.mu.Lock()
	switch {
	case f.closed:
		f.mu.Unlock()
		return false, ErrBusClosed
	case f.owned[name]:
		f.mu.Unlock()
		return true, nil
	case f.taken[name]:
		f.mu.Unlock()
		return false, nil
	}
	f.owned[name] = true
	cbs := slices.Clone(f.acquired[name])
	f.mu.Unlock()
	runAll(cbs)
	return true, nil
}

// ReleaseName drops name and, as the real bus does, delivers NameLost.
func (f *FakeBus) ReleaseName(name string) error {
	f.mu.Lock()
	had := f.owned[name]
	delete(f.owned, name)
	cbs := slices.Clone(f.lost[name])
	f.mu.Unlock()
	if had {
		runAll(cbs)
	}
	return nil
}

// Emit records the emission and delivers it to OnEmit observers. After StallEmits, it first waits for the release.
//
// Observers run synchronously, one emission at a time, in emission order. An emission made while another is being
// delivered (re-entrantly from an observer, or from another goroutine) is queued and delivered by the goroutine
// already delivering, right after the current observers return; in that case Emit returns before its own observers
// have run.
func (f *FakeBus) Emit(member string, body map[string]any) error {
	if err := checkBody(body); err != nil {
		return err
	}
	f.mu.Lock()
	stall := f.stall
	f.mu.Unlock()
	if stall != nil {
		<-stall
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return ErrBusClosed
	}
	if f.emitErr != nil {
		err := f.emitErr
		f.mu.Unlock()
		return err
	}
	f.emissions = append(f.emissions, Emission{Member: member, Body: copyBody(body)})
	f.pending = append(f.pending, Emission{Member: member, Body: copyBody(body)})
	if f.draining {
		f.mu.Unlock()
		return nil
	}
	f.draining = true
	for len(f.pending) > 0 {
		next := f.pending[0]
		f.pending = f.pending[1:]
		obs := slices.Clone(f.observers)
		f.mu.Unlock()
		for _, o := range obs {
			o(next.Member, copyBody(next.Body))
		}
		f.mu.Lock()
	}
	f.draining = false
	f.mu.Unlock()
	return nil
}

// Serve installs h for CallCarserverToPeer and CallRemoteToLocal, once.
func (f *FakeBus) Serve(h Handlers) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handlers != nil {
		return errServedTwice
	}
	f.handlers = &h
	return nil
}

// OnNameLost registers cb for LoseName, DeliverNameLost, ReleaseName and Close.
func (f *FakeBus) OnNameLost(name string, cb func()) error { return f.register(f.lost, name, cb) }

// OnNameAcquired registers cb for RequestName getting name.
func (f *FakeBus) OnNameAcquired(name string, cb func()) error {
	return f.register(f.acquired, name, cb)
}

func (f *FakeBus) register(m map[string][]func(), name string, cb func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrBusClosed
	}
	m[name] = append(m[name], cb)
	return nil
}

// NameOwner returns the FakeBus's unique name only while it owns name.
func (f *FakeBus) NameOwner(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.owned[name] {
		return "", fmt.Errorf("carlink: %s has no owner on the FakeBus", name)
	}
	return fakeUnique, nil
}

// UniqueName is the FakeBus's unique name.
func (f *FakeBus) UniqueName() string { return fakeUnique }

// Close simulates losing the bus connection: every owned name is dropped, every OnNameLost callback fires once, and
// later RequestName and Emit fail.
func (f *FakeBus) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.owned = map[string]bool{}
	var cbs []func()
	for _, l := range f.lost {
		cbs = append(cbs, l...)
	}
	f.mu.Unlock()
	runAll(cbs)
	return nil
}

// SetEmitErr makes every Emit fail with err (nothing recorded) until it is called again with nil.
func (f *FakeBus) SetEmitErr(err error) {
	f.mu.Lock()
	f.emitErr = err
	f.mu.Unlock()
}

// StallEmits makes every Emit that starts from now on block until release is closed, as a stuck write does.
func (f *FakeBus) StallEmits(release <-chan struct{}) {
	f.mu.Lock()
	f.stall = release
	f.mu.Unlock()
}

// OnEmit adds a synchronous observer of every emission (see Emit for the ordering guarantee). A mock QtCarServer uses
// it to react to the helper's signals.
func (f *FakeBus) OnEmit(cb func(member string, body map[string]any)) {
	f.mu.Lock()
	f.observers = append(f.observers, cb)
	f.mu.Unlock()
}

// Emissions returns a copy of everything emitted so far, in order.
func (f *FakeBus) Emissions() []Emission {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Emission, len(f.emissions))
	for i, e := range f.emissions {
		out[i] = Emission{Member: e.Member, Body: copyBody(e.Body)}
	}
	return out
}

// TakeName marks name as held by another process (for example stock webrtc-comms): later RequestName calls for it
// return owned = false. It does not affect a current owner.
func (f *FakeBus) TakeName(name string) {
	f.mu.Lock()
	f.taken[name] = true
	f.mu.Unlock()
}

// FreeName undoes TakeName: the other holder is gone, so RequestName succeeds again.
func (f *FakeBus) FreeName(name string) {
	f.mu.Lock()
	delete(f.taken, name)
	f.mu.Unlock()
}

// LoseName removes our ownership of name and fires its OnNameLost callbacks.
func (f *FakeBus) LoseName(name string) {
	f.mu.Lock()
	delete(f.owned, name)
	cbs := slices.Clone(f.lost[name])
	f.mu.Unlock()
	runAll(cbs)
}

// DeliverNameLost fires name's OnNameLost callbacks without changing ownership, as a NameLost the real bus delivers
// late does (for example one from a Release, arriving after a new acquisition).
func (f *FakeBus) DeliverNameLost(name string) {
	f.mu.Lock()
	cbs := slices.Clone(f.lost[name])
	f.mu.Unlock()
	runAll(cbs)
}

// CallCarserverToPeer makes the car's CarserverToPeerCommsData call, from sender ":fake.car": it runs
// Handlers.CarFrame synchronously (unless the frame is dropped, as DBusBus drops it), then replies. nil means no
// reply: nothing is served.
func (f *FakeBus) CallCarserverToPeer(in map[string]any) map[string]any {
	f.mu.Lock()
	h := f.handlers
	f.mu.Unlock()
	if h == nil {
		return nil
	}
	if fr, drop, _ := frameOf(in, fakeCar); drop == "" && h.CarFrame != nil {
		h.CarFrame(fr)
	}
	return map[string]any{ReplyKey: h.reply()}
}

// CallRemoteToLocal makes the car's RemoteToLocalSignaling call: Handlers.Signaling runs synchronously, and the reply
// is 0 (also when nothing is served).
func (f *FakeBus) CallRemoteToLocal(in map[string]any) uint32 {
	f.mu.Lock()
	h := f.handlers
	f.mu.Unlock()
	if h != nil && h.Signaling != nil {
		h.Signaling(copyBody(in))
	}
	return 0
}
