package main

import "testing"

func TestEnviron(t *testing.T) {
	m := environ([]string{"A=1", "B=x=y", "A=2", "C", "D="})
	if len(m) != 3 || m["A"] != "1" || m["B"] != "x=y" || m["D"] != "" {
		t.Errorf("environ = %v", m)
	}
}
