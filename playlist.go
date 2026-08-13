package main

// Playlist ordering: in-order or random, looping forever until stopped.

import (
	"math/rand"
	"sync"
)

// Order is how a playlist advances.
type Order string

const (
	OrderSequential Order = "order"  // as listed, natural sort
	OrderRandom     Order = "random" // shuffled
)

// Playlist walks a set of items, looping indefinitely.
//
// Random mode shuffles the whole set and walks the shuffle, reshuffling at each
// wrap. That is deliberate rather than picking an independent random item each
// time: independent picks repeat and starve, so a 20-file stick visibly plays
// the same clip twice in a row and leaves others unseen for minutes. A reshuffled
// permutation shows everything once per cycle, which is what people mean by
// "random" for signage.
type Playlist struct {
	mu     sync.Mutex
	items  []Item
	order  Order
	seq    []int // indices into items, in play order
	pos    int   // index into seq
	loop   bool
	cycles int
	rng    *rand.Rand
	// lastPlayed is the item index most recently returned by Next, or -1
	// before anything has played. Used to stop a reshuffle from repeating an
	// item across a cycle boundary.
	lastPlayed int
}

func NewPlaylist(items []Item, order Order, loop bool, seed int64) *Playlist {
	p := &Playlist{
		items:      items,
		order:      order,
		loop:       loop,
		rng:        rand.New(rand.NewSource(seed)),
		lastPlayed: -1,
	}
	p.reseq()
	return p
}

// reseq rebuilds the play order. Caller holds the lock (or is the constructor).
func (p *Playlist) reseq() {
	p.seq = make([]int, len(p.items))
	for i := range p.seq {
		p.seq[i] = i
	}
	if p.order == OrderRandom && len(p.seq) > 1 {
		p.rng.Shuffle(len(p.seq), func(i, j int) { p.seq[i], p.seq[j] = p.seq[j], p.seq[i] })
		// Avoid the one visible artefact of per-cycle reshuffling: the last
		// item of the old cycle landing first in the new one, which reads as a
		// repeat to the viewer even though the cycle boundary is correct.
		if p.cycles > 0 && len(p.seq) > 1 && p.seq[0] == p.lastPlayed {
			p.seq[0], p.seq[len(p.seq)-1] = p.seq[len(p.seq)-1], p.seq[0]
		}
	}
	p.pos = 0
}

// Next returns the next item to play. ok is false when the playlist is
// exhausted, which only happens when loop is off.
func (p *Playlist) Next() (Item, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.items) == 0 {
		return Item{}, false
	}
	if p.pos >= len(p.seq) {
		if !p.loop {
			return Item{}, false
		}
		p.cycles++
		p.reseq()
	}
	idx := p.seq[p.pos]
	p.pos++
	p.lastPlayed = idx
	return p.items[idx], true
}

// Peek reports the item Next would return, without consuming it.
func (p *Playlist) Peek() (Item, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.items) == 0 {
		return Item{}, false
	}
	pos, seq := p.pos, p.seq
	if pos >= len(seq) {
		if !p.loop {
			return Item{}, false
		}
		// Peeking past a wrap would have to reshuffle, which would change what
		// actually plays. Report the first item of the current sequence as a
		// stable approximation.
		return p.items[seq[0]], true
	}
	return p.items[seq[pos]], true
}

// Prev steps back two positions so the following Next replays the previous
// item. Used by the UI's "back" control.
func (p *Playlist) Prev() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pos -= 2
	if p.pos < 0 {
		p.pos = 0
	}
}

// Len is the number of items in the set.
func (p *Playlist) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

// Position reports 1-based position within the current cycle, and the cycle
// count, for status display.
func (p *Playlist) Position() (pos, total, cycles int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pos, len(p.items), p.cycles
}

// Items returns a copy of the play set.
func (p *Playlist) Items() []Item {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Item, len(p.items))
	copy(out, p.items)
	return out
}

// SetOrder switches between sequential and random mid-playback, taking effect
// at the next cycle boundary for sequential and immediately for random.
func (p *Playlist) SetOrder(o Order) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.order == o {
		return
	}
	p.order = o
	played := p.pos
	p.reseq()
	if o == OrderSequential && played < len(p.seq) {
		// Keep our place when switching to sequential, so the viewer does not
		// see the playlist jump back to the top.
		p.pos = played
	}
}

// SetLoop turns looping on or off mid-playback.
func (p *Playlist) SetLoop(loop bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loop = loop
}
