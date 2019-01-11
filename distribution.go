package redisbench

type Distribution interface {
	// Normalize() returns a number (0,1] that conforms to the given distribution.
	Normalize(float64) float64
}

type FlatDistribution struct{}

func (FlatDistribution) Normalize(in float64) float64 {
	return in
}
