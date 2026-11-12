package main

import (
	"reflect"
	"testing"
)

func TestTieredKeepsGroupOrderThenBase(t *testing.T) {
	base := []string{"a", "b", "c", "d", "e"}
	got := tiered(base, []string{"d"}, []string{"e", "b", "d"})
	if want := []string{"d", "b", "e", "a", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
