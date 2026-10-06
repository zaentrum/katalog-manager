package library

import "testing"

// A name's id is the one the library's tools give it: uuid5 in the
// namespace uuid5(NAMESPACE_URL, "https://zaentrum.github.io/schemas/library"),
// as Python's uuid module computes them; a new id is a random lower-case
// UUID.
func TestIDs(t *testing.T) {
	if got := format(namespace); got != "814a4817-3398-5ec2-9f9b-cc503ecc501f" {
		t.Errorf("the namespace: %s", got)
	}
	if got := IDOf("package-superseded:a:b"); got != "c9402cf3-9079-5475-b6a1-3fdde89f37c8" {
		t.Errorf("IDOf: %s", got)
	}
	a, b := NewID(), NewID()
	if a == b || !ValidID(a) || a[14] != '4' {
		t.Errorf("NewID: %s %s", a, b)
	}
}
