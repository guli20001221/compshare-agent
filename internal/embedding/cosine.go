// Package embedding provides a minimal ModelVerse-compatible embedding client
// and float-vector helpers used by the hybrid retrieval branch.
//
// The cosine helper accumulates in float64 and divides once at the end so the
// runtime ranking matches the Python eval helper retrieval_scoring.cosine_similarity
// byte-for-byte on the same inputs. Any drift here breaks the Go-Python parity
// contract enforced by the 377-Q parity test.
package embedding

import "math"

// Cosine returns the cosine similarity of two equal-length float32 vectors,
// or 0.0 if either vector is empty / zero-norm or the lengths differ.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0.0
	}
	var dot, na, nb float64
	for i, x := range a {
		y := b[i]
		fx := float64(x)
		fy := float64(y)
		dot += fx * fy
		na += fx * fx
		nb += fy * fy
	}
	if na == 0.0 || nb == 0.0 {
		return 0.0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
