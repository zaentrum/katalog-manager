package captions

import (
	"math"
	"os"
	"testing"
)

// The differential runs once more where the compiler fuses a multiply and an
// add into one multiply-add, which rounds once where Python rounds twice:
// arm64 always, amd64 built for GOAMD64=v3 (CI's run of it so sets
// KATALOG_TEST_FUSED). There a bare a*b + c of values that tell the two
// apart is math.FMA's, not float64(a*b) + c - else the build doesn't fuse,
// and the run proves nothing. The gate's own arithmetic rounds each product
// before it adds it (float64), so the differential holds it to the record
// logic in such a build too.
func TestTheBuildFusesAMultiplyAndAnAdd(t *testing.T) {
	if os.Getenv("KATALOG_TEST_FUSED") == "" {
		t.Skip("KATALOG_TEST_FUSED not set: no build that fuses is asked for")
	}
	a, b, c := fusedOperands()
	if bare := a*b + c; bare != math.FMA(a, b, c) || bare == float64(a*b)+c {
		t.Fatalf("a*b + c of %v, %v, %v: %.17g, math.FMA's %.17g, rounded first %.17g: this build doesn't fuse", a, b, c,
			bare, math.FMA(a, b, c), float64(a*b)+c)
	}
}

// fusedOperands are 0.1, 600 and 0.08, out of the compiler's sight: the
// window at 10 % of a picture of 600 s that starts at 0.08 s, which a fused
// multiply-add places at 60.080000000000005 s and Python at 60.08.
//
//go:noinline
func fusedOperands() (a, b, c float64) { return 0.1, 600, 0.08 }
