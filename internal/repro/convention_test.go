package repro

import (
	"testing"
)

// These tests pin the transform contract. If any pipeline detail changes
// without bumping AlgorithmVersion, this test fails, which is the signal to
// update the version and re-derive the golden digests.

func TestAlgorithmDigestIsStable(t *testing.T) {
	const golden = "b1e2c7137875937f5a91f797e7f0f391e0187c3979092623a32afdb35dd38e55"
	if got := AlgorithmDigest(); got != golden {
		t.Fatalf("algorithm digest changed: %s\nIf the transform contract really changed, bump AlgorithmVersion and update this golden value.", got)
	}
}

func TestVariantDigestDiffersFromBaseline(t *testing.T) {
	if VariantAlgorithmDigest() == AlgorithmDigest() {
		t.Fatal("the drill variant must not share the baseline contract digest")
	}
}
