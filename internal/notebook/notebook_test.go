package notebook

import (
	"bytes"
	"os"
	"testing"
)

// A notebook written by nbformat should come back byte-for-byte identical.
func TestRoundTrip(t *testing.T) {
	want, err := os.ReadFile("testdata/sample.ipynb")
	if err != nil {
		t.Fatal(err)
	}
	nb, err := Parse(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := nb.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		os.WriteFile("testdata/sample.got.ipynb", got, 0o644)
		t.Fatalf("round trip differs, see testdata/sample.got.ipynb")
	}
}
