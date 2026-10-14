package gds_test

import (
	"testing"

	"github.com/buildbuddy-io/buildbuddy/server/util/gds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func evictAll(p *gds.Pool[string]) []string {
	var keys []string
	for {
		k, ok := p.Evict()
		if !ok {
			return keys
		}
		keys = append(keys, k)
	}
}

func TestEvictsLowestValueFirst(t *testing.T) {
	p := gds.New[string]()
	p.Add("big", 10_000)
	p.Add("small", 50)
	p.Add("medium", 1_000)

	evicted := evictAll(p)

	assert.Equal(t, []string{"small", "medium", "big"}, evicted)
}

func TestIdleEntryAgesOut(t *testing.T) {
	// A 10,000 entry outlives about ten 1,000 entries that arrive after it,
	// then gets evicted ahead of newer 1,000 entries.
	p := gds.New[string]()
	p.Add("big", 10_000)
	var evicted []string
	for i := range 12 {
		p.Add(string(rune('a'+i)), 1_000)
		k, ok := p.Evict()
		require.True(t, ok)
		evicted = append(evicted, k)
	}

	assert.Equal(t, []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "big", "j", "k"}, evicted)
}

func TestReAddRefreshesPriority(t *testing.T) {
	p := gds.New[string]()
	p.Add("big", 10_000)
	for i := range 9 {
		p.Add(string(rune('a'+i)), 1_000)
		_, ok := p.Evict()
		require.True(t, ok)
	}
	p.Add("big", 10_000)
	p.Add("small", 1_000)

	k, ok := p.Evict()

	require.True(t, ok)
	assert.Equal(t, "small", k)
}

func TestRemove(t *testing.T) {
	p := gds.New[string]()
	p.Add("a", 1)
	p.Add("b", 2)

	removed := p.Remove("a")
	removedAgain := p.Remove("a")
	evicted := evictAll(p)

	assert.True(t, removed)
	assert.False(t, removedAgain)
	assert.Equal(t, []string{"b"}, evicted)
}

func TestEqualValuesEvictInLRUOrder(t *testing.T) {
	p := gds.New[string]()
	p.Add("a", 7)
	p.Add("b", 7)
	p.Add("c", 7)
	first, ok := p.Evict()
	require.True(t, ok)
	p.Add("d", 7)
	p.Add("b", 7)

	evicted := append([]string{first}, evictAll(p)...)

	assert.Equal(t, []string{"a", "c", "d", "b"}, evicted)
}
