package app_test

import (
	"os"
	"testing"

	"github.com/rishabh-yadav11/tallow/internal/config"
)

// TestMain opts the whole package in to private base_url hosts.
//
// M14 added a default-on SSRF guard that refuses to dial a base_url resolving
// to loopback, private, or link-local address space. The guard is correct and
// the tests in this package deliberately point base_url at an httptest server on
// 127.0.0.1, so without this every end-to-end test would fail at the dialer.
//
// The opt-in is set once, process-wide, here rather than sprinkled through the
// tests because:
//
//   - it is a property of the TEST DEPLOYMENT, not of any individual test. These
//     tests are the "private upstream is intended" case the guard's opt-out
//     exists for, and a real vLLM-on-10.0.0.5 deployment looks exactly like this.
//   - doing it in one place makes the exception auditable. A reader can see that
//     this one package runs with the guard relaxed, and can see that no other
//     package does.
//   - it avoids a per-test toggle that could leak into another test through
//     ordering, which is exactly the class of bug the guard is for.
//
// The guard's DEFAULT posture is still covered, in two places that do not need
// an upstream at all: internal/config's H9/M14 unit tests assert the address
// policy directly, and TestM14DefaultRefusesLoopbackUpstream in this package
// re-tightens the guard for its own duration and restores it afterwards.
//
// A production binary has no equivalent of this file, so the default posture
// there is unchanged: private upstreams are refused unless an embedding program
// explicitly calls config.SetAllowPrivateBaseURL(true).
func TestMain(m *testing.M) {
	config.SetAllowPrivateBaseURL(true)
	os.Exit(m.Run())
}
