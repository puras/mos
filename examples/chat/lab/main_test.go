package main

import "testing"

func TestSumRange(t *testing.T) {
	if got := SumRange(1, 5); got != 15 {
		t.Fatalf("SumRange(1, 5) = %d, want 15（含两端）", got)
	}
}
