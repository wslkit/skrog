package main

import (
	"strings"
	"testing"
)

// Every call site used to be humanBytes(int64(x)) over a uint64 the engine
// reported. Above 2^63 that wraps, and `skrog stats` printed "-8.0 EiB" for a
// disk usage figure -- a negative size, from a number that cannot be negative.
//
// The engine is not expected to report this. The point is that the format
// permits it and nothing between the wire and the console said no.
func TestHumanBytesDoesNotWrapOnLargeUnsignedValues(t *testing.T) {
	for _, in := range []uint64{
		1 << 63,          // the first value the old conversion turned negative
		(1 << 63) + 1234, // and one just past it
		^uint64(0),       // the largest the field can hold
	} {
		got := humanBytes(in)
		if strings.HasPrefix(got, "-") {
			t.Errorf("humanBytes(%d) = %q: a byte count rendered negative", in, got)
		}
		if !strings.HasSuffix(got, "EiB") {
			t.Errorf("humanBytes(%d) = %q, want exbibytes", in, got)
		}
	}
}

// ...while the signed width keeps working unchanged: prune reports int64, and
// the generic must not have quietly changed what it prints.
func TestHumanBytesFormatsBothWidthsIdentically(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{8 << 30, "8.0 GiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(int64 %d) = %q, want %q", tc.in, got, tc.want)
		}
		if got := humanBytes(uint64(tc.in)); got != tc.want {
			t.Errorf("humanBytes(uint64 %d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
