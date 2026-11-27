//go:build !linux

package helper

// DisableDumps does nothing off Linux: other systems are development hosts, and the helper runs on the car.
func DisableDumps() error { return nil }
