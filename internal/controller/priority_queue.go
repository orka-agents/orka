/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"container/heap"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// QueueItem represents an item in the priority queue
type QueueItem struct {
	NamespacedName types.NamespacedName
	Priority       int32
	EnqueuedAt     time.Time
	index          int
}

// priorityQueue implements heap.Interface for priority-based scheduling
type priorityQueue []*QueueItem

func (pq priorityQueue) Len() int { return len(pq) }

func (pq priorityQueue) Less(i, j int) bool {
	// Higher priority (larger number) comes first
	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority > pq[j].Priority
	}
	// If same priority, earlier enqueue time comes first
	return pq[i].EnqueuedAt.Before(pq[j].EnqueuedAt)
}

func (pq priorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

func (pq *priorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*QueueItem)
	item.index = n
	*pq = append(*pq, item)
}

func (pq *priorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	item := old[n-1]
	old[n-1] = nil  // Avoid memory leak
	item.index = -1 // For safety
	*pq = old[0 : n-1]
	return item
}

// PriorityQueue provides thread-safe priority-based task scheduling
type PriorityQueue struct {
	mu    sync.RWMutex
	items priorityQueue
	index map[types.NamespacedName]*QueueItem
}

// NewPriorityQueue creates a new PriorityQueue
func NewPriorityQueue() *PriorityQueue {
	pq := &PriorityQueue{
		items: make(priorityQueue, 0),
		index: make(map[types.NamespacedName]*QueueItem),
	}
	heap.Init(&pq.items)
	return pq
}

// Enqueue adds a task to the queue
func (pq *PriorityQueue) Enqueue(nn types.NamespacedName, priority int32) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Check if already in queue
	if existing, exists := pq.index[nn]; exists {
		// Update priority if changed
		if existing.Priority != priority {
			existing.Priority = priority
			heap.Fix(&pq.items, existing.index)
		}
		return
	}

	item := &QueueItem{
		NamespacedName: nn,
		Priority:       priority,
		EnqueuedAt:     time.Now(),
	}

	heap.Push(&pq.items, item)
	pq.index[nn] = item
}

// Dequeue removes and returns the highest priority task
func (pq *PriorityQueue) Dequeue() (types.NamespacedName, bool) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if pq.items.Len() == 0 {
		return types.NamespacedName{}, false
	}

	item := heap.Pop(&pq.items).(*QueueItem)
	delete(pq.index, item.NamespacedName)

	return item.NamespacedName, true
}

// Remove removes a task from the queue
func (pq *PriorityQueue) Remove(nn types.NamespacedName) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if item, exists := pq.index[nn]; exists {
		heap.Remove(&pq.items, item.index)
		delete(pq.index, nn)
	}
}

// Peek returns the highest priority task without removing it
func (pq *PriorityQueue) Peek() (types.NamespacedName, int32, bool) {
	pq.mu.RLock()
	defer pq.mu.RUnlock()

	if pq.items.Len() == 0 {
		return types.NamespacedName{}, 0, false
	}

	item := pq.items[0]
	return item.NamespacedName, item.Priority, true
}

// Len returns the number of items in the queue
func (pq *PriorityQueue) Len() int {
	pq.mu.RLock()
	defer pq.mu.RUnlock()
	return pq.items.Len()
}

// Contains checks if a task is in the queue
func (pq *PriorityQueue) Contains(nn types.NamespacedName) bool {
	pq.mu.RLock()
	defer pq.mu.RUnlock()
	_, exists := pq.index[nn]
	return exists
}

// GetDepth returns the queue depth grouped by priority level
func (pq *PriorityQueue) GetDepth() map[int32]int {
	pq.mu.RLock()
	defer pq.mu.RUnlock()

	depth := make(map[int32]int)
	for _, item := range pq.items {
		depth[item.Priority]++
	}
	return depth
}
