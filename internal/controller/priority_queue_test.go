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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestNewPriorityQueue(t *testing.T) {
	pq := NewPriorityQueue()
	if pq == nil {
		t.Fatal("NewPriorityQueue returned nil")
	}
	if pq.Len() != 0 {
		t.Errorf("New queue should be empty, got len %d", pq.Len())
	}
}

func TestPriorityQueue_Enqueue(t *testing.T) {
	pq := NewPriorityQueue()

	nn := types.NamespacedName{Namespace: "default", Name: "task1"}
	pq.Enqueue(nn, 100)

	if pq.Len() != 1 {
		t.Errorf("Queue len = %d, want 1", pq.Len())
	}

	if !pq.Contains(nn) {
		t.Error("Queue should contain the enqueued item")
	}
}

func TestPriorityQueue_Enqueue_UpdatePriority(t *testing.T) {
	pq := NewPriorityQueue()

	nn := types.NamespacedName{Namespace: "default", Name: "task1"}
	pq.Enqueue(nn, 100)
	pq.Enqueue(nn, 200) // Update priority

	if pq.Len() != 1 {
		t.Errorf("Queue len = %d, want 1 (no duplicate)", pq.Len())
	}

	// Check priority was updated
	name, priority, ok := pq.Peek()
	if !ok {
		t.Fatal("Peek failed")
	}
	if priority != 200 {
		t.Errorf("Priority = %d, want 200", priority)
	}
	if name != nn {
		t.Errorf("Name = %v, want %v", name, nn)
	}
}

func TestPriorityQueue_Enqueue_SamePriority(t *testing.T) {
	pq := NewPriorityQueue()

	nn := types.NamespacedName{Namespace: "default", Name: "task1"}
	pq.Enqueue(nn, 100)
	pq.Enqueue(nn, 100) // Same priority

	if pq.Len() != 1 {
		t.Errorf("Queue len = %d, want 1", pq.Len())
	}
}

func TestPriorityQueue_Dequeue(t *testing.T) {
	pq := NewPriorityQueue()

	// Empty queue
	_, ok := pq.Dequeue()
	if ok {
		t.Error("Dequeue on empty queue should return false")
	}

	// Add items
	nn1 := types.NamespacedName{Namespace: "default", Name: "task1"}
	nn2 := types.NamespacedName{Namespace: "default", Name: "task2"}
	pq.Enqueue(nn1, 100)
	pq.Enqueue(nn2, 200)

	// Dequeue should return highest priority
	name, ok := pq.Dequeue()
	if !ok {
		t.Fatal("Dequeue failed")
	}
	if name != nn2 {
		t.Errorf("Dequeue returned %v, want %v (higher priority)", name, nn2)
	}

	if pq.Len() != 1 {
		t.Errorf("Queue len = %d, want 1", pq.Len())
	}
}

func TestPriorityQueue_Dequeue_SamePriority_FIFO(t *testing.T) {
	pq := NewPriorityQueue()

	nn1 := types.NamespacedName{Namespace: "default", Name: "task1"}
	nn2 := types.NamespacedName{Namespace: "default", Name: "task2"}

	pq.Enqueue(nn1, 100)
	time.Sleep(1 * time.Millisecond) // Ensure different EnqueuedAt
	pq.Enqueue(nn2, 100)

	// First enqueued should be dequeued first
	name, ok := pq.Dequeue()
	if !ok {
		t.Fatal("Dequeue failed")
	}
	if name != nn1 {
		t.Errorf("Dequeue returned %v, want %v (FIFO order)", name, nn1)
	}
}

func TestPriorityQueue_Remove(t *testing.T) {
	pq := NewPriorityQueue()

	nn1 := types.NamespacedName{Namespace: "default", Name: "task1"}
	nn2 := types.NamespacedName{Namespace: "default", Name: "task2"}

	pq.Enqueue(nn1, 100)
	pq.Enqueue(nn2, 200)

	pq.Remove(nn1)

	if pq.Len() != 1 {
		t.Errorf("Queue len = %d, want 1", pq.Len())
	}

	if pq.Contains(nn1) {
		t.Error("Queue should not contain removed item")
	}
}

func TestPriorityQueue_Remove_NonExisting(t *testing.T) {
	pq := NewPriorityQueue()

	nn := types.NamespacedName{Namespace: "default", Name: "task1"}
	pq.Enqueue(nn, 100)

	// Remove non-existing item should not panic
	nonExisting := types.NamespacedName{Namespace: "default", Name: "nonexistent"}
	pq.Remove(nonExisting)

	if pq.Len() != 1 {
		t.Errorf("Queue len = %d, want 1", pq.Len())
	}
}

func TestPriorityQueue_Peek(t *testing.T) {
	pq := NewPriorityQueue()

	// Empty queue
	_, _, ok := pq.Peek()
	if ok {
		t.Error("Peek on empty queue should return false")
	}

	// Add items
	nn1 := types.NamespacedName{Namespace: "default", Name: "task1"}
	nn2 := types.NamespacedName{Namespace: "default", Name: "task2"}
	pq.Enqueue(nn1, 100)
	pq.Enqueue(nn2, 200)

	// Peek should return highest priority without removing
	name, priority, ok := pq.Peek()
	if !ok {
		t.Fatal("Peek failed")
	}
	if name != nn2 {
		t.Errorf("Peek returned %v, want %v", name, nn2)
	}
	if priority != 200 {
		t.Errorf("Peek priority = %d, want 200", priority)
	}

	// Len should be unchanged
	if pq.Len() != 2 {
		t.Errorf("Queue len = %d, want 2 after Peek", pq.Len())
	}
}

func TestPriorityQueue_Len(t *testing.T) {
	pq := NewPriorityQueue()

	if pq.Len() != 0 {
		t.Errorf("Empty queue len = %d, want 0", pq.Len())
	}

	nn1 := types.NamespacedName{Namespace: "default", Name: "task1"}
	pq.Enqueue(nn1, 100)

	if pq.Len() != 1 {
		t.Errorf("Queue len = %d, want 1", pq.Len())
	}

	nn2 := types.NamespacedName{Namespace: "default", Name: "task2"}
	pq.Enqueue(nn2, 200)

	if pq.Len() != 2 {
		t.Errorf("Queue len = %d, want 2", pq.Len())
	}
}

func TestPriorityQueue_Contains(t *testing.T) {
	pq := NewPriorityQueue()

	nn := types.NamespacedName{Namespace: "default", Name: "task1"}

	if pq.Contains(nn) {
		t.Error("Empty queue should not contain any item")
	}

	pq.Enqueue(nn, 100)

	if !pq.Contains(nn) {
		t.Error("Queue should contain enqueued item")
	}

	other := types.NamespacedName{Namespace: "default", Name: "other"}
	if pq.Contains(other) {
		t.Error("Queue should not contain non-enqueued item")
	}
}

func TestPriorityQueue_GetDepth(t *testing.T) {
	pq := NewPriorityQueue()

	// Empty queue
	depth := pq.GetDepth()
	if len(depth) != 0 {
		t.Errorf("Empty queue depth = %v, want empty map", depth)
	}

	// Add items with different priorities
	pq.Enqueue(types.NamespacedName{Namespace: "default", Name: "task1"}, 100)
	pq.Enqueue(types.NamespacedName{Namespace: "default", Name: "task2"}, 100)
	pq.Enqueue(types.NamespacedName{Namespace: "default", Name: "task3"}, 200)
	pq.Enqueue(types.NamespacedName{Namespace: "default", Name: "task4"}, 300)

	depth = pq.GetDepth()
	if depth[100] != 2 {
		t.Errorf("Depth[100] = %d, want 2", depth[100])
	}
	if depth[200] != 1 {
		t.Errorf("Depth[200] = %d, want 1", depth[200])
	}
	if depth[300] != 1 {
		t.Errorf("Depth[300] = %d, want 1", depth[300])
	}
}

func TestPriorityQueue_PriorityOrdering(t *testing.T) {
	pq := NewPriorityQueue()

	// Add items with various priorities
	items := []struct {
		nn       types.NamespacedName
		priority int32
	}{
		{types.NamespacedName{Namespace: "default", Name: "low"}, 10},
		{types.NamespacedName{Namespace: "default", Name: "high"}, 1000},
		{types.NamespacedName{Namespace: "default", Name: "medium"}, 500},
	}

	for _, item := range items {
		pq.Enqueue(item.nn, item.priority)
	}

	// Dequeue should return in priority order
	expected := []string{"high", "medium", "low"}
	for _, name := range expected {
		nn, ok := pq.Dequeue()
		if !ok {
			t.Fatal("Dequeue failed")
		}
		if nn.Name != name {
			t.Errorf("Dequeue returned %s, want %s", nn.Name, name)
		}
	}
}

func TestQueueItem_Fields(t *testing.T) {
	item := &QueueItem{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "task"},
		Priority:       100,
		EnqueuedAt:     time.Now(),
		index:          0,
	}

	if item.NamespacedName.Namespace != "ns" {
		t.Errorf("Namespace = %s, want ns", item.NamespacedName.Namespace)
	}
	if item.NamespacedName.Name != "task" {
		t.Errorf("Name = %s, want task", item.NamespacedName.Name)
	}
	if item.Priority != 100 {
		t.Errorf("Priority = %d, want 100", item.Priority)
	}
}

func Test_priorityQueue_HeapInterface(t *testing.T) {
	// Test the internal heap implementation
	pq := &priorityQueue{}

	// Test Len
	if pq.Len() != 0 {
		t.Errorf("Len = %d, want 0", pq.Len())
	}

	// Test Push
	item1 := &QueueItem{Priority: 100, EnqueuedAt: time.Now()}
	pq.Push(item1)
	if pq.Len() != 1 {
		t.Errorf("Len = %d, want 1", pq.Len())
	}

	// Test Pop
	popped := pq.Pop().(*QueueItem)
	if popped != item1 {
		t.Error("Pop returned wrong item")
	}
	if pq.Len() != 0 {
		t.Errorf("Len = %d, want 0", pq.Len())
	}
}

func Test_priorityQueue_Less(t *testing.T) {
	now := time.Now()
	pq := priorityQueue{
		&QueueItem{Priority: 100, EnqueuedAt: now},
		&QueueItem{Priority: 200, EnqueuedAt: now},
	}

	// Higher priority should come first
	if !pq.Less(1, 0) {
		t.Error("Higher priority item should be less (come first)")
	}

	// Same priority - earlier time should come first
	pq[0].Priority = 100
	pq[1].Priority = 100
	pq[0].EnqueuedAt = now
	pq[1].EnqueuedAt = now.Add(time.Second)

	if !pq.Less(0, 1) {
		t.Error("Earlier enqueued item should be less (come first) when same priority")
	}
}

func Test_priorityQueue_Swap(t *testing.T) {
	pq := priorityQueue{
		&QueueItem{Priority: 100, index: 0},
		&QueueItem{Priority: 200, index: 1},
	}

	pq.Swap(0, 1)

	if pq[0].Priority != 200 {
		t.Error("Swap did not swap items")
	}
	if pq[0].index != 0 {
		t.Error("Swap did not update index")
	}
	if pq[1].index != 1 {
		t.Error("Swap did not update index")
	}
}
