package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MessageType identifies a control-plane protocol message.
type MessageType string

const (
	MessageTypeRegister      MessageType = "register"
	MessageTypeRegistered    MessageType = "registered"
	MessageTypePing          MessageType = "ping"
	MessageTypePong          MessageType = "pong"
	MessageTypeRequestStart  MessageType = "request_start"
	MessageTypeRequestBody   MessageType = "request_body"
	MessageTypeRequestEnd    MessageType = "request_end"
	MessageTypeRequestCancel MessageType = "request_cancel"
	MessageTypeResponseStart MessageType = "response_start"
	MessageTypeResponseBody  MessageType = "response_body"
	MessageTypeResponseEnd   MessageType = "response_end"
	MessageTypeError         MessageType = "error"
	MessageTypeClose         MessageType = "close"
)

// Message is the generic JSON envelope exchanged over the stream control plane.
type Message struct {
	Type    MessageType      `json:"type"`
	ID      string           `json:"id,omitempty"`
	Payload *json.RawMessage `json:"payload,omitempty"`
}

// RegisterPayload is sent by the client immediately after connecting.
type RegisterPayload struct {
	Protocol       string         `json:"protocol"`
	LocalPort      int            `json:"local_port"`
	AuthToken      string         `json:"auth_token,omitempty"`
	Metadata       ClientMetadata `json:"metadata,omitempty"`
	ResumeTunnelID string         `json:"resume_tunnel_id,omitempty"`
	ResumeToken    string         `json:"resume_token,omitempty"`
}

// ClientMetadata describes the connecting CLI instance.
type ClientMetadata struct {
	ClientVersion string `json:"client_version,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
}

// RegisteredPayload confirms that the daemon accepted a tunnel session.
type RegisteredPayload struct {
	TunnelID                 string `json:"tunnel_id"`
	Protocol                 string `json:"protocol"`
	LocalPort                int    `json:"local_port"`
	Target                   string `json:"target"`
	PublicURL                string `json:"public_url"`
	Status                   string `json:"status"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
	ResumeToken              string `json:"resume_token,omitempty"`
	Resumed                  bool   `json:"resumed,omitempty"`
}

// PingPayload keeps a control connection alive.
type PingPayload struct {
	Sequence int64     `json:"sequence"`
	SentAt   time.Time `json:"sent_at"`
}

// PongPayload acknowledges a ping.
type PongPayload struct {
	Sequence   int64     `json:"sequence"`
	ReceivedAt time.Time `json:"received_at"`
}

const DefaultBodyChunkSize = 32 * 1024

// RequestStartPayload describes an incoming HTTP request before body frames arrive.
type RequestStartPayload struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Host    string              `json:"host,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
}

// BodyChunkPayload carries a body chunk for a request or response stream.
type BodyChunkPayload struct {
	Chunk []byte `json:"chunk,omitempty"`
}

// StreamEndPayload marks the end of a framed request or response body stream.
type StreamEndPayload struct{}

// ResponseStartPayload describes the upstream response before body frames arrive.
type ResponseStartPayload struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
}

// RequestCancelPayload cancels an in-flight request stream.
type RequestCancelPayload struct {
	Reason string `json:"reason,omitempty"`
}

// ProtocolErrorPayload reports a protocol-level failure.
type ProtocolErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ClosePayload describes an intentional connection close.
type ClosePayload struct {
	Reason string `json:"reason,omitempty"`
}

// NewMessage creates a typed control-plane message.
func NewMessage(messageType MessageType, payload any) (Message, error) {
	msg := Message{Type: messageType}
	if payload == nil {
		return msg, nil
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return Message{}, fmt.Errorf("marshal %s payload: %w", messageType, err)
	}

	encoded := json.RawMessage(raw)
	msg.Payload = &encoded
	return msg, nil
}

// Decode decodes a typed payload from a control-plane envelope.
func (m Message) Decode(dst any) error {
	if m.Payload == nil {
		return fmt.Errorf("message %q has no payload", m.Type)
	}
	if err := json.Unmarshal(*m.Payload, dst); err != nil {
		return fmt.Errorf("decode %s payload: %w", m.Type, err)
	}
	return nil
}

// Validate ensures the register payload is usable by the daemon.
func (p RegisterPayload) Validate() error {
	if _, err := NormalizeTunnelProtocol(p.Protocol); err != nil {
		return err
	}
	if p.LocalPort < 1 || p.LocalPort > 65535 {
		return fmt.Errorf("local port %d must be between 1 and 65535", p.LocalPort)
	}
	if (p.ResumeTunnelID == "") != (p.ResumeToken == "") {
		return fmt.Errorf("resume_tunnel_id and resume_token must be provided together")
	}
	return nil
}

// NormalizeTunnelProtocol validates and normalizes a protocol name.
func NormalizeTunnelProtocol(protocol string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(protocol))
	switch normalized {
	case "http", "https", "tcp":
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported protocol %q", protocol)
	}
}
