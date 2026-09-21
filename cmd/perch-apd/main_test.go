package main

import "testing"

func TestOneProcOnlyOn32BitWithoutAnExplicitGOMAXPROCS(t *testing.T) {
	unset := func(string) string { return "" }
	set := func(k string) string {
		if k == "GOMAXPROCS" {
			return "2"
		}
		return ""
	}
	for _, tc := range []struct {
		intSize int
		getenv  func(string) string
		want    bool
	}{
		{32, unset, true},
		{32, set, false},
		{64, unset, false},
		{64, set, false},
	} {
		if got := oneProc(tc.intSize, tc.getenv); got != tc.want {
			t.Errorf("oneProc(%d, GOMAXPROCS=%q) = %v", tc.intSize, tc.getenv("GOMAXPROCS"), got)
		}
	}
}
