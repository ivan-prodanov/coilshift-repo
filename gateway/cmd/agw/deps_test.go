package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// agw can never drive the car (design §3, §6.2): it has no bus access and no helper key, and its SUMMON capability
// only relays the inner session's bytes. The binary-level half of that is its import set (SEC-3): agw's build must
// not reach the code that talks to com.tesla.Webrtc (internal/carlink), runs the Summon engine (internal/summon),
// serves or supervises the helper (internal/summon/helper), probes the car (internal/summon/probe), or plays the
// phone's end of an inner session (client). This fails as soon as an import, direct or not, brings one in, for the
// car's build (linux/amd64) and the Mac's (darwin).
//
// go list runs offline (GOPROXY=off, GOFLAGS=-mod=readonly, the local toolchain): it only reads the module cache that
// built this test.
func TestAgwCannotReachTheCar(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command on PATH (go test puts its own first): %v", err)
	}
	forbidden := []struct{ pkg, why string }{
		{"airgapp/gateway/internal/carlink", "the D-Bus link to com.tesla.Webrtc"},
		{"airgapp/gateway/internal/summon", "the Summon engine"},
		{"airgapp/gateway/internal/summon/helper", "the helper's serve, supervisor and finish"},
		{"airgapp/gateway/internal/summon/probe", "the car probe"},
		{"airgapp/gateway/client", "the phone's end of a session"},
	}
	for _, target := range []struct{ goos, goarch string }{{"linux", "amd64"}, {"darwin", "arm64"}} {
		cmd := exec.Command(goBin, "list", "-deps", ".")
		cmd.Env = append(os.Environ(), "GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=0",
			"GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local")
		out, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("%s/%s: go list -deps: %v\n%s", target.goos, target.goarch, err, stderr)
		}
		deps := strings.Fields(string(out))
		// The list is agw's own: it ends with agw and holds its SUMMON relay.
		if len(deps) == 0 || deps[len(deps)-1] != "airgapp/gateway/cmd/agw" ||
			!slices.Contains(deps, "airgapp/gateway/capability/summonrelay") {
			t.Fatalf("%s/%s: go list -deps does not list agw's own packages:\n%s", target.goos, target.goarch, out)
		}
		for _, d := range deps {
			for _, f := range forbidden {
				// The package itself, or one under it (internal/carlink/mockcar, internal/summon/helper/...). A
				// sibling such as internal/summon/relay, the record format agw needs, is not under internal/summon's
				// engine: only the exact package is.
				under := strings.HasPrefix(d, f.pkg+"/") && f.pkg != "airgapp/gateway/internal/summon"
				if d == f.pkg || under {
					t.Errorf("%s/%s: agw's build reaches %s (%s): agw must never be able to drive the car",
						target.goos, target.goarch, d, f.why)
				}
			}
		}
	}
}
