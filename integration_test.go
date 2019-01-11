package redisbench

import (
	"testing"

	"github.com/alicebob/miniredis"
)

type integration struct {
	server   *miniredis.Miniredis
	tearDown func()
}

var globalIntegration *integration

func NewIntegration() *integration {
	s, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	return &integration{server: s, tearDown: s.Close}
}

func (i *integration) run(tb testing.TB, n int64) {
	// addr := i.server.Addr()
	l, err := NewLoadTest("tcp", "[::1]:6379",
		TrafficMix(map[string]float64{
			"GET": 1, "SET": 1, "PING": 1,
			"EXPIRE": 1, "SETEX": 1}),
		Seed(42))
	if err != nil {
		tb.Fail()
	}

	if err := l.RunOne(n); err != nil {
		tb.Log(err)
		tb.Fail()
	}
	if l.rpcs != n {
		tb.Log("Expected ", n, " RPCs, got ", l.rpcs)
		tb.Fail()
	}
}

func BenchmarkIntegration(b *testing.B) {
	globalIntegration.run(b, int64(b.N))
}

func TestIntegration(t *testing.T) {
	globalIntegration.run(t, 10000)
}

func init() {
	globalIntegration = NewIntegration()
	defer globalIntegration.tearDown()
}
