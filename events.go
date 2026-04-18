package main

import (
	"encoding/json"
	"strings"
	"sync"
)

// Event represents a system event with a topic and associated data.
type Event struct {
	Topic string      `json:"topic"`
	Data  interface{} `json:"data"`
}

// Bus represents an internal event bus for publishing and subscribing to events.
type Bus struct {
	mu        sync.RWMutex
	listeners map[string][]chan Event
}

// NewBus creates a new instance of the event bus.
func NewBus() *Bus {
	return &Bus{
		listeners: make(map[string][]chan Event),
	}
}

// Subscribe adds a listener for a specific topic and returns a channel for receiving events.
func (b *Bus) Subscribe(topic string) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan Event, 64)
	b.listeners[topic] = append(b.listeners[topic], ch)
	return ch
}

// Unsubscribe removes a specific channel from the listeners for a topic.
func (b *Bus) Unsubscribe(topic string, ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if listeners, ok := b.listeners[topic]; ok {
		for i, listener := range listeners {
			if listener == ch {
				b.listeners[topic] = append(listeners[:i], listeners[i+1:]...)
				close(ch)
				break
			}
		}
	}
}

// Publish sends an event to all listeners subscribed to the topic.
func (b *Bus) Publish(topic string, data interface{}) {
	// Support namespace-specific topics (e.g., "server:123:status")
	// listeners can subscribe to the base topic or the specific one.
	baseTopic := topic
	if strings.Contains(topic, ":") {
		parts := strings.SplitN(topic, ":", 2)
		baseTopic = parts[0]
	}

	event := Event{Topic: topic, Data: data}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// Notify base topic listeners
	if baseTopic != topic {
		for _, ch := range b.listeners[baseTopic] {
			select {
			case ch <- event:
			default:
				// Drop event if channel is full to prevent blocking
			}
		}
	}

	// Notify specific topic listeners
	if listeners, ok := b.listeners[topic]; ok {
		for _, ch := range listeners {
			select {
			case ch <- event:
			default:
				// Drop event if channel is full
			}
		}
	}
}

// MarshalJSON returns the JSON representation of the event.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"topic": e.Topic,
		"data":  e.Data,
	})
}
