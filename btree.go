// Copyright 2014-2022 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package btree implements in-memory B-Trees of arbitrary degree.
//
// btree implements an in-memory B-Tree for use as an ordered data structure.
// It is not meant for persistent storage solutions.
//
// It has a flatter structure than an equivalent red-black or other binary tree,
// which in some cases yields better memory usage and/or performance.
// See some discussion on the matter here:
//
//	http://google-opensource.blogspot.com/2013/01/c-containers-that-save-memory-and-time.html
//
// Note, though, that this project is in no way related to the C++ B-Tree
// implementation written about there.
//
// Within this tree, each node contains a slice of items and a (possibly nil)
// slice of children.  For basic numeric values or raw structs, this can cause
// efficiency differences when compared to equivalent C++ template code that
// stores values in arrays within the node:
//   - Due to the overhead of storing values as interfaces (each
//     value needs to be stored as the value itself, then 2 words for the
//     interface pointing to that value and its type), resulting in higher
//     memory use.
//   - Since interfaces can point to values anywhere in memory, values are
//     most likely not stored in contiguous blocks, resulting in a higher
//     number of cache misses.
//
// These issues don't tend to matter, though, when working with strings or other
// heap-allocated structures, since C++-equivalent structures also must store
// pointers and also distribute their values across the heap.
//
// This implementation is designed to be a drop-in replacement to gollrb.LLRB
// trees, (http://github.com/petar/gollrb), an excellent and probably the most
// widely used ordered tree implementation in the Go ecosystem currently.
// Its functions, therefore, exactly mirror those of
// llrb.LLRB where possible.  Unlike gollrb, though, we currently don't
// support storing multiple equivalent values.
//
// There are two implementations; those suffixed with 'G' are generics, usable
// for any type, and require a passed-in "less" function to define their ordering.
// Those without this prefix are specific to the 'Item' interface, and use
// its 'Less' function for ordering.
package btree

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

const (
	DefaultFreeListSize = 32
)

// FreeList represents a free list of btree nodes. By default each
// BTree has its own FreeList, but multiple BTrees can share the same
// FreeList, in particular when they're created with Clone.
// Two Btrees using the same freelist are safe for concurrent write access.
type FreeList[T Item[T]] struct {
	mu       sync.Mutex
	freelist []*node[T]
}

// NewFreeList creates a new free list.
// size is the maximum size of the returned free list.
func NewFreeList[T Item[T]](size int) *FreeList[T] {
	return &FreeList[T]{freelist: make([]*node[T], 0, size)}
}

func (f *FreeList[T]) newNode() (n *node[T]) {
	f.mu.Lock()
	index := len(f.freelist) - 1
	if index < 0 {
		f.mu.Unlock()
		return new(node[T])
	}
	n = f.freelist[index]
	f.freelist[index] = nil
	f.freelist = f.freelist[:index]
	f.mu.Unlock()
	return
}

func (f *FreeList[T]) freeNode(n *node[T]) (out bool) {
	f.mu.Lock()
	if len(f.freelist) < cap(f.freelist) {
		f.freelist = append(f.freelist, n)
		out = true
	}
	f.mu.Unlock()
	return
}

// ItemIterator allows callers of {A/De}scend* to iterate in-order over portions of
// the tree.  When this function returns false, iteration will stop and the
// associated Ascend* function will immediately return.
type ItemIterator[T Item[T]] func(item T) bool

// New creates a new B-Tree with the given degree.
//
// New(2), for example, will create a 2-3-4 tree (each node contains 1-3 items
// and 2-4 children).
//
// The passed-in LessFunc determines how objects of type T are ordered.
func New[T Item[T]](degree int) *BTree[T] {
	return NewWithFreeList(degree, NewFreeList[T](DefaultFreeListSize))
}

// NewWithFreeList creates a new B-Tree that uses the given node free list.
func NewWithFreeList[T Item[T]](degree int, f *FreeList[T]) *BTree[T] {
	if degree <= 1 {
		panic("bad degree")
	}
	return &BTree[T]{
		degree:   degree,
		freelist: f,
	}
}

// items stores items in a node.
type items[T Item[T]] []T

// insertAt inserts a value into the given index, pushing all subsequent values
// forward.
func (s *items[T]) insertAt(index int, item T) {
	var zero T
	*s = append(*s, zero)
	if index < len(*s) {
		copy((*s)[index+1:], (*s)[index:])
	}
	(*s)[index] = item
}

// removeAt removes a value at a given index, pulling all subsequent values
// back.
func (s *items[T]) removeAt(index int) T {
	item := (*s)[index]
	copy((*s)[index:], (*s)[index+1:])
	var zero T
	(*s)[len(*s)-1] = zero
	*s = (*s)[:len(*s)-1]
	return item
}

// pop removes and returns the last element in the list.
func (s *items[T]) pop() (out T) {
	index := len(*s) - 1
	out = (*s)[index]
	var zero T
	(*s)[index] = zero
	*s = (*s)[:index]
	return
}

// truncate truncates this instance at index so that it contains only the
// first index items. index must be less than or equal to length.
func (s *items[T]) truncate(index int) {
	var toClear items[T]
	*s, toClear = (*s)[:index], (*s)[index:]
	var zero T
	for i := 0; i < len(toClear); i++ {
		toClear[i] = zero
	}
}

// find returns the index where the given item should be inserted into this
// list.  'found' is true if the item already exists in the list at the given
// index.
func (s items[T]) find(item T) (index int, found bool) {
	i := sort.Search(len(s), func(i int) bool {
		return item.Less(s[i])
	})
	if i > 0 && !s[i-1].Less(item) {
		return i - 1, true
	}
	return i, false
}

func (s items[T]) DeepCopy() items[T] {
	s2 := make(items[T], 0, cap(s))

	for _, item := range s {
		s2 = append(s2, item.DeepCopy())
	}

	return s2
}

// node is an internal node in a tree.
//
// It must at all times maintain the invariant that either
//   - len(children) == 0, len(items) unconstrained
//   - len(children) == len(items) + 1
type node[T Item[T]] struct {
	items    items[T]
	children items[*node[T]]
	t        *BTree[T]
}

func (n *node[T]) Less(*node[T]) bool {
	panic("*node[T] should not call Less(*node[T])")
}

func (n *node[T]) DeepCopy() *node[T] {
	n2 := &node[T]{}

	if n == nil {
		return n2
	}

	if n.items != nil {
		n2.items = n.items.DeepCopy()
	}
	if n.children != nil {
		n2.children = n.children.DeepCopy()
	}

	return n2
}

// split splits the given node at the given index.  The current node shrinks,
// and this function returns the item that existed at that index and a new node
// containing all items/children after it.
func (n *node[T]) split(i int) (T, *node[T]) {
	item := n.items[i]
	next := n.t.newNode()
	next.items = append(next.items, n.items[i+1:]...)
	n.items.truncate(i)
	if len(n.children) > 0 {
		next.children = append(next.children, n.children[i+1:]...)
		n.children.truncate(i + 1)
	}
	return item, next
}

// maybeSplitChild checks if a child should be split, and if so splits it.
// Returns whether or not a split occurred.
func (n *node[T]) maybeSplitChild(i, maxItems int) bool {
	if len(n.children[i].items) < maxItems {
		return false
	}
	first := n.children[i]
	item, second := first.split(maxItems / 2)
	n.items.insertAt(i, item)
	n.children.insertAt(i+1, second)
	return true
}

// insert inserts an item into the subtree rooted at this node, making sure
// no nodes in the subtree exceed maxItems items.  Should an equivalent item be
// be found/replaced by insert, it will be returned.
func (n *node[T]) insert(item T, maxItems int) (_ T, _ bool) {
	i, found := n.items.find(item)
	if found {
		out := n.items[i]
		n.items[i] = item
		return out, true
	}
	if len(n.children) == 0 {
		n.items.insertAt(i, item)
		return
	}
	if n.maybeSplitChild(i, maxItems) {
		inTree := n.items[i]
		switch {
		case item.Less(inTree):
			// no change, we want first split node
		case inTree.Less(item):
			i++ // we want second split node
		default:
			out := n.items[i]
			n.items[i] = item
			return out, true
		}
	}
	return n.children[i].insert(item, maxItems)
}

// get finds the given key in the subtree and returns it.
func (n *node[T]) get(key T) (_ T, _ bool) {
	i, found := n.items.find(key)
	if found {
		return n.items[i], true
	} else if len(n.children) > 0 {
		return n.children[i].get(key)
	}
	return
}

// min returns the first item in the subtree.
func min[T Item[T]](n *node[T]) (_ T, found bool) {
	if n == nil {
		return
	}
	for len(n.children) > 0 {
		n = n.children[0]
	}
	if len(n.items) == 0 {
		return
	}
	return n.items[0], true
}

// max returns the last item in the subtree.
func max[T Item[T]](n *node[T]) (_ T, found bool) {
	if n == nil {
		return
	}
	for len(n.children) > 0 {
		n = n.children[len(n.children)-1]
	}
	if len(n.items) == 0 {
		return
	}
	return n.items[len(n.items)-1], true
}

// toRemove details what item to remove in a node.remove call.
type toRemove int

const (
	removeItem toRemove = iota // removes the given item
	removeMin                  // removes smallest item in the subtree
	removeMax                  // removes largest item in the subtree
)

// remove removes an item from the subtree rooted at this node.
func (n *node[T]) remove(item T, minItems int, typ toRemove) (_ T, _ bool) {
	var i int
	var found bool
	switch typ {
	case removeMax:
		if len(n.children) == 0 {
			return n.items.pop(), true
		}
		i = len(n.items)
	case removeMin:
		if len(n.children) == 0 {
			return n.items.removeAt(0), true
		}
		i = 0
	case removeItem:
		i, found = n.items.find(item)
		if len(n.children) == 0 {
			if found {
				return n.items.removeAt(i), true
			}
			return
		}
	default:
		panic("invalid type")
	}
	// If we get to here, we have children.
	if len(n.children[i].items) <= minItems {
		return n.growChildAndRemove(i, item, minItems, typ)
	}
	child := n.children[i]
	// Either we had enough items to begin with, or we've done some
	// merging/stealing, because we've got enough now and we're ready to return
	// stuff.
	if found {
		// The item exists at index 'i', and the child we've selected can give us a
		// predecessor, since if we've gotten here it's got > minItems items in it.
		out := n.items[i]
		// We use our special-case 'remove' call with typ=maxItem to pull the
		// predecessor of item i (the rightmost leaf of our immediate left child)
		// and set it into where we pulled the item from.
		var zero T
		n.items[i], _ = child.remove(zero, minItems, removeMax)
		return out, true
	}
	// Final recursive call.  Once we're here, we know that the item isn't in this
	// node and that the child is big enough to remove from.
	return child.remove(item, minItems, typ)
}

// growChildAndRemove grows child 'i' to make sure it's possible to remove an
// item from it while keeping it at minItems, then calls remove to actually
// remove it.
//
// Most documentation says we have to do two sets of special casing:
//  1. item is in this node
//  2. item is in child
//
// In both cases, we need to handle the two subcases:
//
//	A) node has enough values that it can spare one
//	B) node doesn't have enough values
//
// For the latter, we have to check:
//
//	a) left sibling has node to spare
//	b) right sibling has node to spare
//	c) we must merge
//
// To simplify our code here, we handle cases #1 and #2 the same:
// If a node doesn't have enough items, we make sure it does (using a,b,c).
// We then simply redo our remove call, and the second time (regardless of
// whether we're in case 1 or 2), we'll have enough items and can guarantee
// that we hit case A.
func (n *node[T]) growChildAndRemove(i int, item T, minItems int, typ toRemove) (T, bool) {
	if i > 0 && len(n.children[i-1].items) > minItems {
		// Steal from left child
		child := n.children[i]
		stealFrom := n.children[i-1]
		stolenItem := stealFrom.items.pop()
		child.items.insertAt(0, n.items[i-1])
		n.items[i-1] = stolenItem
		if len(stealFrom.children) > 0 {
			child.children.insertAt(0, stealFrom.children.pop())
		}
	} else if i < len(n.items) && len(n.children[i+1].items) > minItems {
		// steal from right child
		child := n.children[i]
		stealFrom := n.children[i+1]
		stolenItem := stealFrom.items.removeAt(0)
		child.items = append(child.items, n.items[i])
		n.items[i] = stolenItem
		if len(stealFrom.children) > 0 {
			child.children = append(child.children, stealFrom.children.removeAt(0))
		}
	} else {
		if i >= len(n.items) {
			i--
		}
		child := n.children[i]
		// merge with right child
		mergeItem := n.items.removeAt(i)
		mergeChild := n.children.removeAt(i + 1)
		child.items = append(child.items, mergeItem)
		child.items = append(child.items, mergeChild.items...)
		child.children = append(child.children, mergeChild.children...)
		n.t.freeNode(mergeChild)
	}
	return n.remove(item, minItems, typ)
}

type direction int

const (
	descend = direction(-1)
	ascend  = direction(+1)
)

type optionalItem[T Item[T]] struct {
	item  T
	valid bool
}

func optional[T Item[T]](item T) optionalItem[T] {
	return optionalItem[T]{item: item, valid: true}
}
func empty[T Item[T]]() optionalItem[T] {
	return optionalItem[T]{}
}

// iterate provides a simple method for iterating over elements in the tree.
//
// When ascending, the 'start' should be less than 'stop' and when descending,
// the 'start' should be greater than 'stop'. Setting 'includeStart' to true
// will force the iterator to include the first item when it equals 'start',
// thus creating a "greaterOrEqual" or "lessThanEqual" rather than just a
// "greaterThan" or "lessThan" queries.
func (n *node[T]) iterate(dir direction, start, stop optionalItem[T], includeStart bool, hit bool, iter ItemIterator[T]) (bool, bool) {
	var ok, found bool
	var index, stopIndex int
	switch dir {
	case ascend:
		if start.valid {
			index, _ = n.items.find(start.item)
		}
		if stop.valid {
			stopIndex, _ = n.items.find(stop.item)
		} else {
			stopIndex = len(n.items)
		}
		for i := index; i < stopIndex; i++ {
			if len(n.children) > 0 {
				if hit, ok = n.children[i].iterate(dir, start, stop, includeStart, hit, iter); !ok {
					return hit, false
				}
			}
			if !includeStart && !hit && start.valid && !start.item.Less(n.items[i]) {
				hit = true
				continue
			}
			hit = true
			if !iter(n.items[i]) {
				return hit, false
			}
		}
		if len(n.children) > 0 {
			if hit, ok = n.children[stopIndex].iterate(dir, start, stop, includeStart, hit, iter); !ok {
				return hit, false
			}
		}
	case descend:
		if start.valid {
			index, found = n.items.find(start.item)
			if !found {
				index = index - 1
			}
		} else {
			index = len(n.items) - 1
		}
		if stop.valid {
			stopIndex, found = n.items.find(stop.item)
			if found {
				stopIndex += 1
			}
		} else {
			stopIndex = 0
		}
		for i := index; i >= stopIndex; i-- {
			if start.valid && !n.items[i].Less(start.item) {
				if !includeStart || hit || start.item.Less(n.items[i]) {
					continue
				}
			}
			if len(n.children) > 0 {
				if hit, ok = n.children[i+1].iterate(dir, start, stop, includeStart, hit, iter); !ok {
					return hit, false
				}
			}
			hit = true
			if !iter(n.items[i]) {
				return hit, false
			}
		}
		if len(n.children) > 0 {
			if hit, ok = n.children[stopIndex].iterate(dir, start, stop, includeStart, hit, iter); !ok {
				return hit, false
			}
		}
	}
	return hit, true
}

// print is used for testing/debugging purposes.
//
//nolint:unused
func (n *node[T]) print(w io.Writer, level int) {
	fmt.Fprintf(w, "%sNODE:%v\n", strings.Repeat("  ", level), n.items)
	for _, c := range n.children {
		c.print(w, level+1)
	}
}

// BTree is a generic implementation of a B-Tree.
//
// BTree stores items of type T in an ordered structure, allowing easy insertion,
// removal, and iteration.
//
// Write operations are not safe for concurrent mutation by multiple
// goroutines, but Read operations are.
type BTree[T Item[T]] struct {
	degree   int
	length   int
	root     *node[T]
	freelist *FreeList[T]
}

func setBTreeRootRecursive[T Item[T]](t *BTree[T], n *node[T]) {
	for _, n2 := range n.children {
		setBTreeRootRecursive(t, n2)
	}

	n.t = t
}

func (t *BTree[T]) DeepCopy() *BTree[T] {
	t2 := New[T](t.degree)
	t2.root = t.root.DeepCopy()
	t2.length = t.length

	setBTreeRootRecursive(t2, t2.root)

	return t2
}

// LessFunc[T] determines how to order a type 'T'.  It should implement a strict
// ordering, and should return true if within that ordering, 'a' < 'b'.
type LessFunc[T Item[T]] func(a, b T) bool

// maxItems returns the max number of items to allow per node.
func (t *BTree[T]) maxItems() int {
	return t.degree*2 - 1
}

// minItems returns the min number of items to allow per node (ignored for the
// root node).
func (t *BTree[T]) minItems() int {
	return t.degree - 1
}

func (t *BTree[T]) newNode() (n *node[T]) {
	n = t.freelist.newNode()
	n.t = t
	return
}

func (t *BTree[T]) freeNode(n *node[T]) {
	// clear to allow GC
	n.items.truncate(0)
	n.children.truncate(0)
	n.t = nil // clear to allow GC
	t.freelist.freeNode(n)
}

// ReplaceOrInsert adds the given item to the tree.  If an item in the tree
// already equals the given one, it is removed from the tree and returned,
// and the second return value is true.  Otherwise, (zeroValue, false)
//
// nil cannot be added to the tree (will panic).
func (t *BTree[T]) ReplaceOrInsert(item T) (_ T, _ bool) {
	if t.root == nil {
		t.root = t.newNode()
		t.root.items = append(t.root.items, item)
		t.length++
		return
	} else {
		if len(t.root.items) >= t.maxItems() {
			item2, second := t.root.split(t.maxItems() / 2)
			oldroot := t.root
			t.root = t.newNode()
			t.root.items = append(t.root.items, item2)
			t.root.children = append(t.root.children, oldroot, second)
		}
	}
	out, outb := t.root.insert(item, t.maxItems())
	if !outb {
		t.length++
	}
	return out, outb
}

// Delete removes an item equal to the passed in item from the tree, returning
// it.  If no such item exists, returns (zeroValue, false).
func (t *BTree[T]) Delete(item T) (T, bool) {
	return t.deleteItem(item, removeItem)
}

// DeleteMin removes the smallest item in the tree and returns it.
// If no such item exists, returns (zeroValue, false).
func (t *BTree[T]) DeleteMin() (T, bool) {
	var zero T
	return t.deleteItem(zero, removeMin)
}

// DeleteMax removes the largest item in the tree and returns it.
// If no such item exists, returns (zeroValue, false).
func (t *BTree[T]) DeleteMax() (T, bool) {
	var zero T
	return t.deleteItem(zero, removeMax)
}

func (t *BTree[T]) deleteItem(item T, typ toRemove) (_ T, _ bool) {
	if t.root == nil || len(t.root.items) == 0 {
		return
	}
	out, outb := t.root.remove(item, t.minItems(), typ)
	if len(t.root.items) == 0 && len(t.root.children) > 0 {
		oldroot := t.root
		t.root = t.root.children[0]
		t.freeNode(oldroot)
	}
	if outb {
		t.length--
	}
	return out, outb
}

// AscendRange calls the iterator for every value in the tree within the range
// [greaterOrEqual, lessThan), until iterator returns false.
func (t *BTree[T]) AscendRange(greaterOrEqual, lessThan T, iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, optional(greaterOrEqual), optional(lessThan), true, false, iterator)
}

// AscendLessThan calls the iterator for every value in the tree within the range
// [first, pivot), until iterator returns false.
func (t *BTree[T]) AscendLessThan(pivot T, iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, empty[T](), optional(pivot), false, false, iterator)
}

// AscendGreaterOrEqual calls the iterator for every value in the tree within
// the range [pivot, last], until iterator returns false.
func (t *BTree[T]) AscendGreaterOrEqual(pivot T, iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, optional(pivot), empty[T](), true, false, iterator)
}

// Ascend calls the iterator for every value in the tree within the range
// [first, last], until iterator returns false.
func (t *BTree[T]) Ascend(iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(ascend, empty[T](), empty[T](), false, false, iterator)
}

// DescendRange calls the iterator for every value in the tree within the range
// [lessOrEqual, greaterThan), until iterator returns false.
func (t *BTree[T]) DescendRange(lessOrEqual, greaterThan T, iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, optional(lessOrEqual), optional(greaterThan), true, false, iterator)
}

// DescendLessOrEqual calls the iterator for every value in the tree within the range
// [pivot, first], until iterator returns false.
func (t *BTree[T]) DescendLessOrEqual(pivot T, iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, optional(pivot), empty[T](), true, false, iterator)
}

// DescendGreaterThan calls the iterator for every value in the tree within
// the range [last, pivot), until iterator returns false.
func (t *BTree[T]) DescendGreaterThan(pivot T, iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, empty[T](), optional(pivot), false, false, iterator)
}

// Descend calls the iterator for every value in the tree within the range
// [last, first], until iterator returns false.
func (t *BTree[T]) Descend(iterator ItemIterator[T]) {
	if t.root == nil {
		return
	}
	t.root.iterate(descend, empty[T](), empty[T](), false, false, iterator)
}

// Get looks for the key item in the tree, returning it.  It returns
// (zeroValue, false) if unable to find that item.
func (t *BTree[T]) Get(key T) (_ T, _ bool) {
	if t.root == nil {
		return
	}
	return t.root.get(key)
}

// Min returns the smallest item in the tree, or (zeroValue, false) if the tree is empty.
func (t *BTree[T]) Min() (_ T, _ bool) {
	return min(t.root)
}

// Max returns the largest item in the tree, or (zeroValue, false) if the tree is empty.
func (t *BTree[T]) Max() (_ T, _ bool) {
	return max(t.root)
}

// Has returns true if the given key is in the tree.
func (t *BTree[T]) Has(key T) bool {
	_, ok := t.Get(key)
	return ok
}

// Len returns the number of items currently in the tree.
func (t *BTree[T]) Len() int {
	return t.length
}

// Clear removes all items from the btree.  If addNodesToFreelist is true,
// t's nodes are added to its freelist as part of this call, until the freelist
// is full.  Otherwise, the root node is simply dereferenced and the subtree
// left to Go's normal GC processes.
//
// This can be much faster
// than calling Delete on all elements, because that requires finding/removing
// each element in the tree and updating the tree accordingly.  It also is
// somewhat faster than creating a new tree to replace the old one, because
// nodes from the old tree are reclaimed into the freelist for use by the new
// one, instead of being lost to the garbage collector.
//
// This call takes:
//
//	O(1): when addNodesToFreelist is false, this is a single operation.
//	O(1): when the freelist is already full, it breaks out immediately
//	O(freelist size):  when the freelist is empty and the nodes are all owned
//	    by this tree, nodes are added to the freelist until full.
//	O(tree size):  when all nodes are owned by another tree, all nodes are
//	    iterated over looking for nodes to add to the freelist, and due to
//	    ownership, none are.
func (t *BTree[T]) Clear(addNodesToFreelist bool) {
	t.root, t.length = nil, 0
}
