package compliance

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The check rounds each product before it adds it (float64), as Python does
// and as a build that doesn't fuse does: where the compiler fuses a multiply
// and an add into one multiply-add, which rounds once (arm64: CI's fused job,
// which sets KATALOG_TEST_FUSED, and proves its build fuses first), the
// check's verdicts are the same. Each pair of operands tells the two apart:
// fused, roundTo rounds 0.004999999999999999 to 2 places as 0, not 0.01, and
// priorEncode takes an encode at 10.000001 Mbps for one at a maxrate of 10e6.
func TestTheChecksArithmeticRoundsEachProductFirst(t *testing.T) {
	f, p, half, was, maxrate := fusedOperands()
	if bare := f*p + half; os.Getenv("KATALOG_TEST_FUSED") != "" && (bare != math.FMA(f, p, half) || bare == float64(f*p)+half) {
		t.Fatalf("%v*%v + %v: %.17g, math.FMA's %.17g: this build doesn't fuse", f, p, half, bare, math.FMA(f, p, half))
	}
	if got := roundTo(f, 2); got != 0.01 {
		t.Errorf("roundTo(%v, 2): %v; want 0.01", f, got)
	}
	if got := roundTo(-f, 2); got != -0.01 {
		t.Errorf("roundTo(%v, 2): %v; want -0.01", -f, got)
	}
	var pkg packageRecord
	if err := json.Unmarshal([]byte(`{"renditions": {"video": [{"codec": "avc1.640028", "bitrateBps": 9000000}]}}`), &pkg); err != nil {
		t.Fatal(err)
	}
	details := "maxrate=" + was + "Mbps"
	if rate, ok := priorEncode(title{Transcode: step{Details: &details}}, records{pkg: &pkg}, no, "h264", maxrate); ok {
		t.Errorf("an encode at %s Mbps, of a maxrate of %v: taken as one at it (%v); want none", was, maxrate, rate)
	}
}

// fusedOperands are the test's operands, out of the compiler's sight.
//
//go:noinline
func fusedOperands() (f, p, half float64, was string, maxrate float64) {
	return 0.004999999999999999, 100, 0.5, "10.000001", 10e6
}
