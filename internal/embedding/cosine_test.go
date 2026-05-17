package embedding

import (
	"math"
	"testing"
)

func TestCosineKnownPairs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a    []float32
		b    []float32
		want float64
	}{
		{name: "identical", a: []float32{1, 0, 0}, b: []float32{1, 0, 0}, want: 1.0},
		{name: "orthogonal", a: []float32{1, 0}, b: []float32{0, 1}, want: 0.0},
		{name: "opposite", a: []float32{1, 0}, b: []float32{-1, 0}, want: -1.0},
		{name: "empty", a: nil, b: []float32{1}, want: 0.0},
		{name: "length mismatch", a: []float32{1, 2}, b: []float32{1}, want: 0.0},
		{name: "zero norm", a: []float32{0, 0, 0}, b: []float32{1, 2, 3}, want: 0.0},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := Cosine(tc.a, tc.b)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Cosine(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// Locks Go-Python parity at production storage layout: corpus / query vectors
// arrive as float32 (from JSON []float32 + ModelVerse embedding response).
// Both Go's Cosine and Python's retrieval_scoring.cosine_similarity must
// accumulate in float64. The reference value here is computed with Python
// after explicitly round-tripping the input vectors through float32 storage
// (struct.pack/unpack 'f') so the Python side mimics Go's storage class.
// Tolerance is 1e-6 rather than 1e-12 because float32 storage introduces ~1e-7
// per-component noise which compounds slightly through dot/sqrt; this is well
// below any cosine gap that could flip a rank in the 377-Q parity fixture.
func TestCosineParityFixture(t *testing.T) {
	t.Parallel()
	a := []float32{0.1, -0.2, 0.3, 0.4, -0.5}
	b := []float32{-0.05, 0.7, 0.1, -0.2, 0.3}
	want := -0.5849348419310150
	got := Cosine(a, b)
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("parity drift: Cosine = %.16f, want %.16f", got, want)
	}
}
