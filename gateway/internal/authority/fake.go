package authority

import (
	"context"
	"time"
)

// Fake is an Oracle for tests and the bench flag.
type Fake struct {
	Role       int32
	FormFactor uint32 // only KeyEntry reports it
	Owner      bool
	Delay      time.Duration
	Err        error
}

var _ KeyEntryOracle = Fake{}

// KeyEntry is the KeyInfo row Fake stands for: pk with Role and FormFactor.
func (f Fake) KeyEntry(ctx context.Context, pk []byte) (KeyInfoEntry, error) {
	if err := f.wait(ctx); err != nil {
		return KeyInfoEntry{}, err
	}
	return KeyInfoEntry{ID: KeyID(pk), PublicKey: pk, Role: uint32(f.Role), FormFactor: f.FormFactor}, nil
}

func (f Fake) wait(ctx context.Context) error {
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.Err
}

func (f Fake) KeyRole(ctx context.Context, pk []byte) (int32, error) {
	if err := f.wait(ctx); err != nil {
		return 0, err
	}
	return f.Role, nil
}

func (f Fake) AuthOwnerIsAuthd(ctx context.Context) (bool, error) {
	if err := f.wait(ctx); err != nil {
		return false, err
	}
	return f.Owner, nil
}
