package buffer

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
	"unsafe"
)

func toByteSlice(p unsafe.Pointer, n int) []byte {
	return unsafe.Slice((*byte)(p), n)
}

// fillWithGarbage writes random data to [p, p+n).
func fillWithGarbage(p unsafe.Pointer, n int) error {
	b := toByteSlice(p, n)
	_, err := io.ReadFull(rand.Reader, b)
	return err
}

// findNonZero finds the offset of the first non-zero byte in [p, p+n). If
// none, it returns n.
func findNonZero(p unsafe.Pointer, n int) int {
	b := toByteSlice(p, n)
	for i, x := range b {
		if x != 0 {
			return i
		}
	}

	return n
}

func TestOutMessageShrinkTo(t *testing.T) {
	// Set up a buffer with some payload.
	var om OutMessage
	om.Reset()
	om.AppendString("taco")
	om.AppendString("burrito")

	// Shrink it.
	om.ShrinkTo(OutMessageHeaderSize + len("taco"))

	// The result should be a zeroed header followed by "taco".
	const wantLen = OutMessageHeaderSize + len("taco")

	if got, want := om.Len(), wantLen; got != want {
		t.Errorf("om.Len() = %d, want %d", got, want)
	}

	b := []byte(nil)
	for i := 0; i < len(om.Sglist); i++ {
		b = append(b, om.Sglist[i]...)
	}
	if got, want := len(b), wantLen; got != want {
		t.Fatalf("len(om.OutHeaderBytes()) = %d, want %d", got, want)
	}

	want := append(
		make([]byte, OutMessageHeaderSize),
		"taco"...)

	if !bytes.Equal(b, want) {
		t.Error("messages differ")
	}
}

func TestOutMessageGrowReusesScratchAfterReset(t *testing.T) {
	om := NewOutMessage(4096)
	const size = 1234
	p := om.Grow(size)
	if err := fillWithGarbage(p, size); err != nil {
		t.Fatalf("fillWithGarbage: %v", err)
	}
	om.Reset()

	if got := om.Grow(size); got != p {
		t.Errorf("Grow after Reset = %p, want the scratch at %p", got, p)
	}
	if i := findNonZero(p, size); i != size {
		t.Errorf("non-zero byte at offset %d of a reused segment", i)
	}

	allocs := testing.AllocsPerRun(100, func() {
		om.Reset()
		om.Grow(size)
		om.Grow(100)
	})
	if allocs != 0 {
		t.Errorf("allocations per Reset and two Grows = %v, want 0", allocs)
	}
}

func TestOutMessageScratchStaysWithinCap(t *testing.T) {
	const scratchCap = 1024
	om := NewOutMessage(scratchCap)
	for _, sizes := range [][]int{{600, 600}, {scratchCap}, {scratchCap + 1}, {1, scratchCap}} {
		om.Reset()
		for _, n := range sizes {
			om.Grow(n)
			if len(om.scratch) > scratchCap {
				t.Fatalf("after Grow of %v, scratch holds %d bytes, want at most %d", sizes, len(om.scratch), scratchCap)
			}
		}
		if got, want := om.Len(), OutMessageHeaderSize+sumInts(sizes); got != want {
			t.Errorf("after Grow of %v, Len = %d, want %d", sizes, got, want)
		}
	}
}

func sumInts(ns []int) int {
	sum := 0
	for _, n := range ns {
		sum += n
	}
	return sum
}

func TestOutMessageAppendWithoutAllocating(t *testing.T) {
	var om OutMessage
	a, b, c := []byte("a"), []byte("b"), []byte("c")
	allocs := testing.AllocsPerRun(100, func() {
		om.Reset()
		om.Append(a, b)
		om.Append(c)
	})
	if allocs != 0 {
		t.Errorf("allocations per Reset and three appended segments = %v, want 0", allocs)
	}
}

// Appended slices belong to the caller, so a pooled message must not keep
// them alive.
func TestOutMessageResetDropsAppendedSlices(t *testing.T) {
	var om OutMessage
	om.Reset()
	om.Append([]byte("data"))
	om.Reset()
	for i, s := range om.sglist {
		if s != nil {
			t.Errorf("after Reset, segment %d still references %q", i, s)
		}
	}
}
