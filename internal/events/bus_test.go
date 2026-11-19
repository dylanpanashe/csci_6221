package events

import (
	"sync"
	"testing"
)

func TestFanOutAndSlowSubscriber(t *testing.T) {
	b := NewBus[int]()
	fast, cancelFast := b.Subscribe(100)
	slow, cancelSlow := b.Subscribe(1)
	defer cancelFast()
	defer cancelSlow()
	for i := 0; i < 10; i++ {
		b.Publish(i) // never blocks, even though slow is full after one
	}
	if len(fast) != 10 {
		t.Fatalf("fast got %d", len(fast))
	}
	if len(slow) != 1 || b.Dropped() != 9 {
		t.Fatalf("slow buffered %d, dropped %d", len(slow), b.Dropped())
	}
}

func TestUnsubscribeAndClose(t *testing.T) {
	b := NewBus[string]()
	ch, cancel := b.Subscribe(4)
	cancel()
	cancel() // safe twice
	if _, open := <-ch; open {
		t.Fatal("channel not closed after unsubscribe")
	}
	ch2, _ := b.Subscribe(4)
	b.Close()
	if _, open := <-ch2; open {
		t.Fatal("channel not closed by Close")
	}
	b.Publish("ignored") // must not panic after Close
	ch3, _ := b.Subscribe(1)
	if _, open := <-ch3; open {
		t.Fatal("subscribing to a closed bus should give a closed channel")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	b := NewBus[int]()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				b.Publish(j)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch, cancel := b.Subscribe(8)
				select {
				case <-ch:
				default:
				}
				cancel()
			}
		}()
	}
	wg.Wait()
	b.Close()
}
