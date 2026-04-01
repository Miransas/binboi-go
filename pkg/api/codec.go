package api

import (
	"encoding/json"
	"io"
	"sync"
)

// MessageCodec sends and receives newline-delimited JSON control messages.
type MessageCodec struct {
	dec *json.Decoder
	enc *json.Encoder
	mu  sync.Mutex
}

// NewMessageCodec returns a stream codec.
func NewMessageCodec(r io.Reader, w io.Writer) *MessageCodec {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &MessageCodec{
		dec: json.NewDecoder(r),
		enc: enc,
	}
}

// Receive decodes the next message from the stream.
func (c *MessageCodec) Receive() (Message, error) {
	var msg Message
	if err := c.dec.Decode(&msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}

// Send encodes a message to the stream.
func (c *MessageCodec) Send(msg Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(msg)
}
