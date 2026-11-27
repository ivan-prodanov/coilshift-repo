// Package echo is the test-only ECHO capability: 0x7E in, same body back as 0xFE.
package echo

import (
	"context"
	"encoding/json"

	"airgapp/gateway/capability"
)

const (
	ID      byte = 0x7F
	TypeIn  byte = 0x7E
	TypeOut byte = 0xFE
)

type echo struct{}

// New returns the ECHO capability.
func New() capability.Capability { return echo{} }

func (echo) Spec() capability.Spec {
	return capability.Spec{
		Name:    "ECHO",
		ID:      ID,
		In:      map[byte]int{TypeIn: 1024},
		Out:     map[byte]int{TypeOut: 1024},
		LeaseMS: 20000,
		Bulk:    map[byte]bool{},
	}
}

func (echo) PreArm(context.Context, []byte) error { return nil }

func (echo) Open(_ context.Context, w capability.Window, _ json.RawMessage) (capability.Handler, error) {
	return handler{w}, nil
}

type handler struct{ w capability.Window }

func (h handler) Frame(t byte, body []byte) error {
	if t != TypeIn {
		return nil
	}
	// body is valid only during Frame and Send takes ownership: copy it.
	return h.w.Send(TypeOut, append([]byte(nil), body...))
}

func (handler) Teardown(string) {}
