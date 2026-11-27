package client

import "time"

// SetHeartbeatEvery overrides the beat interval for tests.
func SetHeartbeatEvery(d time.Duration) (restore func()) {
	old := heartbeatEvery
	heartbeatEvery = d
	return func() { heartbeatEvery = old }
}
