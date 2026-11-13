package internal

import (
	"cmp"
	"math"
	"slices"
	"strings"

	mergesortedslices "fiatjaf.com/lib/merge-sorted-slices"
	"github.com/nbd-wtf/go-nostr"
)

func IsOlder(previous, next *nostr.Event) bool {
	return previous.CreatedAt < next.CreatedAt ||
		(previous.CreatedAt == next.CreatedAt && previous.ID > next.ID)
}

func ChooseNarrowestTag(filter nostr.Filter) (key string, values []string, goodness int) {
	var tagKey string
	var tagValues []string
	for key, values := range filter.Tags {
		switch key {
		case "e", "E", "q":
			// 'e' and 'q' are the narrowest possible, so if we have that we will use it and that's it
			tagKey = key
			tagValues = values
			goodness = 9
			break
		case "a", "A", "i", "I", "g", "r":
			// these are second-best as they refer to relatively static things
			goodness = 8
			tagKey = key
			tagValues = values
		case "d":
			// this is as good as long as we have an "authors"
			if len(filter.Authors) != 0 && goodness < 7 {
				goodness = 7
				tagKey = key
				tagValues = values
			} else if goodness < 4 {
				goodness = 4
				tagKey = key
				tagValues = values
			}
		case "h", "t", "l", "k", "K":
			// these things denote "categories", so they are a little more broad
			if goodness < 6 {
				goodness = 6
				tagKey = key
				tagValues = values
			}
		case "p":
			// this is broad and useless for a pure tag search, but we will still prefer it over others
			// for secondary filtering
			if goodness < 2 {
				goodness = 2
				tagKey = key
				tagValues = values
			}
		default:
			// all the other tags are probably too broad and useless
			if goodness == 0 {
				tagKey = key
				tagValues = values
			}
		}
	}

	return tagKey, tagValues, goodness
}

func CopyMapWithoutKey[K comparable, V any](originalMap map[K]V, key K) map[K]V {
	newMap := make(map[K]V, len(originalMap)-1)
	for k, v := range originalMap {
		if k != key {
			newMap[k] = v
		}
	}
	return newMap
}

type IterEvent struct {
	*nostr.Event
	Q int
}

// MergeSortMultipleBatches takes the results of multiple iterators, which are already sorted,
// and merges them into a single big sorted slice
func MergeSortMultiple(batches [][]IterEvent, limit int, dst []IterEvent) []IterEvent {
	// clear up empty lists here while simultaneously computing the total count.
	// this helps because if there are a bunch of empty lists then this pre-clean
	//   step will get us in the faster 'merge' branch otherwise we would go to the other.
	// we would have to do the cleaning anyway inside it.
	// and even if we still go on the other we save one iteration by already computing the
	//   total count.
	total := 0
	for i := len(batches) - 1; i >= 0; i-- {
		if len(batches[i]) == 0 {
			batches = SwapDelete(batches, i)
		} else {
			total += len(batches[i])
		}
	}

	if limit == -1 {
		limit = total
	}

	// this amazing equation will ensure that if one of the two sides goes very small (like 1 or 2)
	//   the other can go very high (like 500) and we're still in the 'merge' branch.
	// if values go somewhere in the middle then they may match the 'merge' branch (batches=20,limit=70)
	//   or not (batches=25, limit=60)
	if math.Log(float64(len(batches)*2))+math.Log(float64(limit)) < 8 {
		if dst == nil {
			dst = make([]IterEvent, limit)
		} else if cap(dst) < limit {
			dst = slices.Grow(dst, limit-len(dst))
		}
		dst = dst[0:limit]
		return mergesortedslices.MergeFuncNoEmptyListsIntoSlice(dst, batches, compareIterEvent)
	} else {
		if dst == nil {
			dst = make([]IterEvent, total)
		} else if cap(dst) < total {
			dst = slices.Grow(dst, total-len(dst))
		}
		dst = dst[0:total]

		// use quicksort in a dumb way that will still be fast because it's cheated
		lastIndex := 0
		for _, batch := range batches {
			copy(dst[lastIndex:], batch)
			lastIndex += len(batch)
		}

		slices.SortFunc(dst, compareIterEvent)

		for i, j := 0, total-1; i < j; i, j = i+1, j-1 {
			dst[i], dst[j] = dst[j], dst[i]
		}

		if limit < len(dst) {
			return dst[0:limit]
		}
		return dst
	}
}

// BatchSizePerNumberOfQueries tries to make an educated guess for the batch size given the total filter limit and
// the number of abstract queries we'll be conducting at the same time
func BatchSizePerNumberOfQueries(totalFilterLimit int, numberOfQueries int) int {
	if numberOfQueries == 1 || totalFilterLimit*numberOfQueries < 50 {
		return totalFilterLimit
	}

	return int(
		math.Ceil(
			math.Pow(float64(totalFilterLimit), 0.80) / math.Pow(float64(numberOfQueries), 0.71),
		),
	)
}

func SwapDelete[A any](arr []A, i int) []A {
	arr[i] = arr[len(arr)-1]
	return arr[:len(arr)-1]
}

func compareIterEvent(a, b IterEvent) int {
	if a.Event == nil {
		if b.Event == nil {
			return 0
		} else {
			return -1
		}
	} else if b.Event == nil {
		return 1
	}

	if a.CreatedAt == b.CreatedAt {
		return strings.Compare(a.ID, b.ID)
	}
	return cmp.Compare(a.CreatedAt, b.CreatedAt)
}
