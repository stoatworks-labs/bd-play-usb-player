package main

import (
	"testing"
)

func mkItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{Rel: string(rune('a' + i)), Name: string(rune('a' + i)), Kind: KindImage}
	}
	return items
}

func TestPlaylistSequentialLoops(t *testing.T) {
	pl := NewPlaylist(mkItems(3), OrderSequential, true, 1)

	// Two full cycles must come back in the same order, forever.
	var got []string
	for i := 0; i < 7; i++ {
		it, ok := pl.Next()
		if !ok {
			t.Fatalf("Next() returned !ok at %d with loop on", i)
		}
		got = append(got, it.Name)
	}
	want := []string{"a", "b", "c", "a", "b", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sequence = %v, want %v", got, want)
		}
	}
}

func TestPlaylistNoLoopStops(t *testing.T) {
	pl := NewPlaylist(mkItems(2), OrderSequential, false, 1)
	for i := 0; i < 2; i++ {
		if _, ok := pl.Next(); !ok {
			t.Fatalf("Next() !ok at %d, expected an item", i)
		}
	}
	if _, ok := pl.Next(); ok {
		t.Error("Next() returned an item past the end with loop off")
	}
}

// The property that makes "random" mean what people expect for signage: every
// item plays exactly once per cycle, so nothing is starved and nothing repeats
// within a cycle.
func TestPlaylistRandomIsAPermutation(t *testing.T) {
	const n = 8
	pl := NewPlaylist(mkItems(n), OrderRandom, true, 42)

	for cycle := 0; cycle < 5; cycle++ {
		seen := map[string]int{}
		for i := 0; i < n; i++ {
			it, ok := pl.Next()
			if !ok {
				t.Fatalf("cycle %d: Next() !ok at %d", cycle, i)
			}
			seen[it.Name]++
		}
		if len(seen) != n {
			t.Fatalf("cycle %d: saw %d distinct items, want %d (%v)", cycle, len(seen), n, seen)
		}
		for name, count := range seen {
			if count != 1 {
				t.Errorf("cycle %d: %q played %d times, want exactly 1", cycle, name, count)
			}
		}
	}
}

// A reshuffle must not put the item that just played at the front of the new
// cycle — to a viewer that reads as the same clip playing twice in a row.
func TestPlaylistRandomNoRepeatAcrossCycleBoundary(t *testing.T) {
	const n = 4
	for seed := int64(0); seed < 50; seed++ {
		pl := NewPlaylist(mkItems(n), OrderRandom, true, seed)
		var last string
		for i := 0; i < n*4; i++ {
			it, ok := pl.Next()
			if !ok {
				t.Fatal("unexpected end")
			}
			if it.Name == last {
				t.Fatalf("seed %d: %q played twice in a row at position %d", seed, it.Name, i)
			}
			last = it.Name
		}
	}
}

// A single-item playlist is the "loop this one video" case and must not trip
// the no-repeat rule into an infinite search.
func TestPlaylistSingleItemLoops(t *testing.T) {
	pl := NewPlaylist(mkItems(1), OrderRandom, true, 7)
	for i := 0; i < 5; i++ {
		it, ok := pl.Next()
		if !ok || it.Name != "a" {
			t.Fatalf("iteration %d: got %q ok=%v, want a", i, it.Name, ok)
		}
	}
}

func TestPlaylistEmpty(t *testing.T) {
	pl := NewPlaylist(nil, OrderSequential, true, 1)
	if _, ok := pl.Next(); ok {
		t.Error("empty playlist returned an item")
	}
	if _, ok := pl.Peek(); ok {
		t.Error("empty playlist peeked an item")
	}
}

func TestPlaylistPrevReplays(t *testing.T) {
	pl := NewPlaylist(mkItems(3), OrderSequential, true, 1)
	first, _ := pl.Next() // a
	pl.Next()             // b is now the item on screen
	// PREV while b is showing must take the viewer back to a, not replay b.
	pl.Prev()
	again, _ := pl.Next()
	if again.Name != first.Name {
		t.Errorf("after Prev, Next = %q, want %q", again.Name, first.Name)
	}
	// Stepping back from the very start must clamp, not go negative.
	pl.Prev()
	pl.Prev()
	pl.Prev()
	at, _ := pl.Next()
	if at.Name != first.Name {
		t.Errorf("after clamping Prev, Next = %q, want %q", at.Name, first.Name)
	}
}

func TestPlaylistSetLoopStops(t *testing.T) {
	pl := NewPlaylist(mkItems(2), OrderSequential, true, 1)
	pl.Next()
	pl.Next()
	pl.SetLoop(false)
	if _, ok := pl.Next(); ok {
		t.Error("SetLoop(false) did not stop the playlist at the end of the cycle")
	}
}

func TestPlaylistSetOrderKeepsPlace(t *testing.T) {
	pl := NewPlaylist(mkItems(5), OrderRandom, true, 3)
	pl.Next()
	pl.Next()
	pl.SetOrder(OrderSequential)
	pos, total, _ := pl.Position()
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	// Switching to sequential mid-playback keeps our place rather than jumping
	// the viewer back to the top of the list.
	if pos != 2 {
		t.Errorf("position after SetOrder = %d, want 2", pos)
	}
}
