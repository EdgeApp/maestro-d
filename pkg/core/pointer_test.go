package core

import (
	"reflect"
	"testing"
)

func TestParsePath(t *testing.T) {
	got, err := ParsePath([]string{"10%, 50%", "200, 300"}, 1000, 800)
	if err != nil || !reflect.DeepEqual(got, [][2]int{{100, 400}, {200, 300}}) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParsePath([]string{"1, 2", "x"}, 100, 100); err == nil {
		t.Fatal("bad point accepted")
	}
}

// The time is shared by length, the remainder to the last leg; a path with
// no length shares it evenly; fewer than two points have no legs.
func TestPathLegs(t *testing.T) {
	legs := PathLegs([][2]int{{0, 0}, {30, 0}, {30, 10}}, 1000)
	if !reflect.DeepEqual(legs, []PathLeg{{30, 0, 750}, {30, 10, 250}}) {
		t.Fatalf("by length: %v", legs)
	}
	legs = PathLegs([][2]int{{0, 0}, {1, 0}, {2, 0}, {3, 0}}, 100)
	if legs[0].Ms+legs[1].Ms+legs[2].Ms != 100 || legs[2].Ms != 34 {
		t.Fatalf("remainder: %v", legs)
	}
	legs = PathLegs([][2]int{{5, 5}, {5, 5}, {5, 5}}, 100)
	if !reflect.DeepEqual(legs, []PathLeg{{5, 5, 50}, {5, 5, 50}}) {
		t.Fatalf("no length: %v", legs)
	}
	if PathLegs([][2]int{{1, 1}}, 100) != nil {
		t.Fatal("one point has legs")
	}
}

func TestSplitLegs(t *testing.T) {
	moves := SplitLegs([2]int{0, 0}, []PathLeg{{25, 0, 100}, {25, 0, 30}}, 10)
	want := []PathLeg{{8, 0, 33}, {16, 0, 33}, {25, 0, 34}, {25, 0, 30}}
	if !reflect.DeepEqual(moves, want) {
		t.Fatalf("got %v, want %v", moves, want)
	}
}
