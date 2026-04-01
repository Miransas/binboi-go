package api

import (
	"context"
	"errors"
	"sync"
)

// ErrDispatcherClosed indicates that a message dispatcher has shut down.
var ErrDispatcherClosed = errors.New("message dispatcher closed")

// DispatcherConfig controls fair stream scheduling across one connection.
type DispatcherConfig struct {
	StreamQueueCapacity int
}

type dispatchStream struct {
	queue     chan Message
	scheduled bool
	draining  bool
}

// MessageDispatcher provides fair round-robin delivery for stream messages.
type MessageDispatcher struct {
	send     func(Message) error
	onError  func(error)
	capacity int

	mu        sync.Mutex
	control   []Message
	streams   map[string]*dispatchStream
	ready     []string
	closed    bool
	closeErr  error
	closeOnce sync.Once
	errorOnce sync.Once
	wakeCh    chan struct{}
	closeCh   chan struct{}
}

// NewMessageDispatcher starts a dispatcher for one stream connection.
func NewMessageDispatcher(send func(Message) error, cfg DispatcherConfig, onError func(error)) *MessageDispatcher {
	if cfg.StreamQueueCapacity <= 0 {
		cfg.StreamQueueCapacity = 1
	}

	dispatcher := &MessageDispatcher{
		send:     send,
		onError:  onError,
		capacity: cfg.StreamQueueCapacity,
		streams:  make(map[string]*dispatchStream),
		wakeCh:   make(chan struct{}, 1),
		closeCh:  make(chan struct{}),
	}

	go dispatcher.run()
	return dispatcher
}

// EnqueueControl queues a non-stream-scoped message for delivery.
func (d *MessageDispatcher) EnqueueControl(_ context.Context, message Message) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return d.closedErr()
	}

	d.control = append(d.control, message)
	d.notifyLocked()
	return nil
}

// EnqueueStream queues a request- or response-scoped message fairly with peer streams.
func (d *MessageDispatcher) EnqueueStream(ctx context.Context, streamID string, message Message, final bool) error {
	d.mu.Lock()
	if d.closed {
		err := d.closedErr()
		d.mu.Unlock()
		return err
	}

	stream := d.streams[streamID]
	if stream == nil {
		stream = &dispatchStream{
			queue: make(chan Message, d.capacity),
		}
		d.streams[streamID] = stream
	}
	d.mu.Unlock()

	select {
	case stream.queue <- message:
	case <-ctx.Done():
		return ctx.Err()
	case <-d.closeCh:
		return d.closedErr()
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return d.closedErr()
	}
	if final {
		stream.draining = true
	}
	if !stream.scheduled {
		stream.scheduled = true
		d.ready = append(d.ready, streamID)
		d.notifyLocked()
	}
	return nil
}

// Close shuts down the dispatcher and unblocks pending writers.
func (d *MessageDispatcher) Close(err error) {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		if err == nil {
			err = ErrDispatcherClosed
		}
		d.closed = true
		d.closeErr = err
		d.mu.Unlock()
		close(d.closeCh)
		d.notify()
	})
}

func (d *MessageDispatcher) run() {
	for {
		message, ok := d.next()
		if !ok {
			return
		}

		if err := d.send(message); err != nil {
			d.errorOnce.Do(func() {
				d.Close(err)
				if d.onError != nil {
					d.onError(err)
				}
			})
			return
		}
	}
}

func (d *MessageDispatcher) next() (Message, bool) {
	for {
		d.mu.Lock()

		if len(d.control) > 0 {
			message := d.control[0]
			d.control = d.control[1:]
			d.mu.Unlock()
			return message, true
		}

		if len(d.ready) > 0 {
			streamID := d.ready[0]
			d.ready = d.ready[1:]
			stream := d.streams[streamID]
			if stream != nil {
				stream.scheduled = false
			}
			d.mu.Unlock()

			if stream == nil {
				continue
			}

			select {
			case message := <-stream.queue:
				d.afterStreamMessage(streamID)
				return message, true
			default:
				d.afterEmptyStream(streamID)
				continue
			}
		}

		closed := d.closed
		d.mu.Unlock()

		if closed {
			return Message{}, false
		}

		select {
		case <-d.wakeCh:
		case <-d.closeCh:
			return Message{}, false
		}
	}
}

func (d *MessageDispatcher) afterStreamMessage(streamID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	stream := d.streams[streamID]
	if stream == nil {
		return
	}

	if len(stream.queue) > 0 && !stream.scheduled {
		stream.scheduled = true
		d.ready = append(d.ready, streamID)
		d.notifyLocked()
		return
	}

	if len(stream.queue) == 0 && stream.draining {
		delete(d.streams, streamID)
	}
}

func (d *MessageDispatcher) afterEmptyStream(streamID string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	stream := d.streams[streamID]
	if stream == nil {
		return
	}
	if len(stream.queue) == 0 && stream.draining {
		delete(d.streams, streamID)
	}
}

func (d *MessageDispatcher) closedErr() error {
	if d.closeErr != nil {
		return d.closeErr
	}
	return ErrDispatcherClosed
}

func (d *MessageDispatcher) notify() {
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}

func (d *MessageDispatcher) notifyLocked() {
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}
