package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultHeartbeat        = 10 * time.Second
)

var reconnectBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// ProtocolError is returned when the server rejects a protocol action.
type ProtocolError struct {
	Code    string
	Message string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// TunnelClient manages a long-lived stream control connection to a daemon.
type TunnelClient struct {
	address string
	dialer  net.Dialer
	logger  *slog.Logger
}

// TunnelSession is an active registered control connection.
type TunnelSession struct {
	conn              net.Conn
	codec             *api.MessageCodec
	logger            *slog.Logger
	registered        api.RegisteredPayload
	heartbeatInterval time.Duration
	httpClient        *http.Client
	closeConn         func()
	sendMu            sync.Mutex
	activeMu          sync.Mutex
	active            map[string]*activeRequest
}

// NewTunnel creates a client for the daemon's stream control listener.
func NewTunnel(address string, logger *slog.Logger) (*TunnelClient, error) {
	if _, _, err := net.SplitHostPort(address); err != nil {
		return nil, fmt.Errorf("parse tunnel server address: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &TunnelClient{
		address: address,
		logger:  logger,
	}, nil
}

// Connect opens a control stream and completes the register handshake.
func (c *TunnelClient) Connect(ctx context.Context, payload api.RegisterPayload) (*TunnelSession, error) {
	if err := payload.Validate(); err != nil {
		return nil, err
	}

	c.logger.Info("connecting to daemon control protocol",
		"server", c.address,
		"protocol", payload.Protocol,
		"local_port", payload.LocalPort,
		"resume_tunnel_id", payload.ResumeTunnelID,
	)

	conn, err := c.dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, fmt.Errorf("dial control protocol: %w", err)
	}

	codec := api.NewMessageCodec(conn, conn)
	closeConn := syncOnceCloser(conn.Close)

	if err := conn.SetWriteDeadline(time.Now().Add(defaultHandshakeTimeout)); err != nil {
		closeConn()
		return nil, fmt.Errorf("set register write deadline: %w", err)
	}

	registerMessage, err := api.NewMessage(api.MessageTypeRegister, payload)
	if err != nil {
		closeConn()
		return nil, err
	}
	if err := codec.Send(registerMessage); err != nil {
		closeConn()
		return nil, fmt.Errorf("send register message: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(defaultHandshakeTimeout)); err != nil {
		closeConn()
		return nil, fmt.Errorf("set register read deadline: %w", err)
	}

	message, err := codec.Receive()
	if err != nil {
		closeConn()
		return nil, fmt.Errorf("read register response: %w", err)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		closeConn()
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}

	switch message.Type {
	case api.MessageTypeRegistered:
		var registered api.RegisteredPayload
		if err := message.Decode(&registered); err != nil {
			closeConn()
			return nil, err
		}

		heartbeatInterval := time.Duration(registered.HeartbeatIntervalSeconds) * time.Second
		if heartbeatInterval <= 0 {
			heartbeatInterval = defaultHeartbeat
		}

		c.logger.Info("tunnel registration succeeded",
			"tunnel_id", registered.TunnelID,
			"public_url", registered.PublicURL,
			"heartbeat_interval", heartbeatInterval,
			"resumed", registered.Resumed,
		)

		return &TunnelSession{
			conn:              conn,
			codec:             codec,
			logger:            c.logger,
			registered:        registered,
			heartbeatInterval: heartbeatInterval,
			httpClient: &http.Client{
				Timeout: 30 * time.Second,
			},
			closeConn: closeConn,
			active:    make(map[string]*activeRequest),
		}, nil
	case api.MessageTypeError:
		var protocolErr api.ProtocolErrorPayload
		if err := message.Decode(&protocolErr); err != nil {
			closeConn()
			return nil, err
		}
		closeConn()
		return nil, &ProtocolError{Code: protocolErr.Code, Message: protocolErr.Message}
	default:
		closeConn()
		return nil, fmt.Errorf("unexpected register response type %q", message.Type)
	}
}

// Run maintains a tunnel session across disconnects using reconnect backoff and resume attempts.
func (c *TunnelClient) Run(ctx context.Context, payload api.RegisterPayload, onConnect func(api.RegisteredPayload)) error {
	if err := payload.Validate(); err != nil {
		return err
	}

	basePayload := payload
	resumeTunnelID := payload.ResumeTunnelID
	resumeToken := payload.ResumeToken
	attempt := 0

	for {
		connectPayload := basePayload
		connectPayload.ResumeTunnelID = resumeTunnelID
		connectPayload.ResumeToken = resumeToken

		session, err := c.Connect(ctx, connectPayload)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			var protocolErr *ProtocolError
			if errors.As(err, &protocolErr) {
				switch protocolErr.Code {
				case "invalid_resume":
					c.logger.Warn("resume rejected, falling back to fresh registration",
						"resume_tunnel_id", resumeTunnelID,
						"error", protocolErr.Message,
					)
					resumeTunnelID = ""
					resumeToken = ""
					continue
				case "invalid_register":
					return err
				}
			}

			delay := backoffDelay(attempt)
			c.logger.Warn("tunnel connect failed, retrying",
				"attempt", attempt+1,
				"delay", delay,
				"error", err,
			)
			if err := sleepContext(ctx, delay); err != nil {
				return nil
			}
			attempt++
			continue
		}

		attempt = 0
		registered := session.Registered()
		resumeTunnelID = registered.TunnelID
		if registered.ResumeToken != "" {
			resumeToken = registered.ResumeToken
		}
		if onConnect != nil {
			onConnect(registered)
		}

		err = session.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}

		delay := backoffDelay(attempt)
		if err != nil {
			c.logger.Warn("tunnel session ended, scheduling reconnect",
				"tunnel_id", registered.TunnelID,
				"delay", delay,
				"error", err,
			)
		} else {
			c.logger.Warn("tunnel session closed unexpectedly, scheduling reconnect",
				"tunnel_id", registered.TunnelID,
				"delay", delay,
			)
		}

		if err := sleepContext(ctx, delay); err != nil {
			return nil
		}
		attempt++
	}
}

// Registered returns the server's registration response.
func (s *TunnelSession) Registered() api.RegisteredPayload {
	return s.registered
}

// Run starts heartbeats and blocks until the session ends.
func (s *TunnelSession) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.closeConn()
	defer s.cancelActiveRequests(context.Canceled)

	heartbeatErrCh := make(chan error, 1)

	go func() {
		<-ctx.Done()
		_ = s.send(api.MessageTypeClose, api.ClosePayload{Reason: "client shutdown"})
		s.closeConn()
	}()

	go func() {
		heartbeatErrCh <- s.heartbeatLoop(ctx)
	}()

	for {
		timeout := (s.heartbeatInterval * 2) + 5*time.Second
		if err := s.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}

		message, err := s.codec.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			select {
			case heartbeatErr := <-heartbeatErrCh:
				if heartbeatErr != nil {
					return heartbeatErr
				}
			default:
			}

			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				return fmt.Errorf("heartbeat timed out waiting for server pong: %w", err)
			}
			return fmt.Errorf("read control message: %w", err)
		}

		switch message.Type {
		case api.MessageTypePong:
			var pong api.PongPayload
			if err := message.Decode(&pong); err != nil {
				return err
			}
			s.logger.Debug("received heartbeat pong",
				"tunnel_id", s.registered.TunnelID,
				"sequence", pong.Sequence,
				"received_at", pong.ReceivedAt,
			)
		case api.MessageTypeError:
			var protocolErr api.ProtocolErrorPayload
			if err := message.Decode(&protocolErr); err != nil {
				return err
			}
			return &ProtocolError{Code: protocolErr.Code, Message: protocolErr.Message}
		case api.MessageTypeClose:
			var closePayload api.ClosePayload
			if message.Payload != nil {
				if err := message.Decode(&closePayload); err != nil {
					return err
				}
			}
			s.logger.Info("server closed tunnel session",
				"tunnel_id", s.registered.TunnelID,
				"reason", closePayload.Reason,
			)
			return nil
		case api.MessageTypeRequestStart:
			var start api.RequestStartPayload
			if err := message.Decode(&start); err != nil {
				return err
			}
			if message.ID == "" {
				return errors.New("request_start missing id")
			}
			if err := s.handleRequestStart(ctx, message.ID, start); err != nil {
				return err
			}
		case api.MessageTypeRequestBody:
			var body api.BodyChunkPayload
			if err := message.Decode(&body); err != nil {
				return err
			}
			if message.ID == "" {
				return errors.New("request_body missing id")
			}
			if err := s.handleRequestBody(message.ID, body.Chunk); err != nil {
				return err
			}
		case api.MessageTypeRequestEnd:
			if message.ID == "" {
				return errors.New("request_end missing id")
			}
			if err := s.handleRequestEnd(message.ID); err != nil {
				return err
			}
		case api.MessageTypeRequestCancel:
			var cancelPayload api.RequestCancelPayload
			if message.Payload != nil {
				if err := message.Decode(&cancelPayload); err != nil {
					return err
				}
			}
			if message.ID == "" {
				return errors.New("request_cancel missing id")
			}
			s.handleRequestCancel(message.ID, cancelPayload)
		default:
			s.logger.Warn("ignoring unexpected control message",
				"tunnel_id", s.registered.TunnelID,
				"type", message.Type,
			)
		}
	}
}

func (s *TunnelSession) sendMessage(message api.Message) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	if err := s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return s.codec.Send(message)
}

func (s *TunnelSession) heartbeatLoop(ctx context.Context) error {
	s.logger.Info("starting heartbeat loop",
		"tunnel_id", s.registered.TunnelID,
		"interval", s.heartbeatInterval,
	)

	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	var sequence int64 = 1
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			payload := api.PingPayload{
				Sequence: sequence,
				SentAt:   time.Now().UTC(),
			}
			if err := s.send(api.MessageTypePing, payload); err != nil {
				s.closeConn()
				return fmt.Errorf("send heartbeat ping: %w", err)
			}

			s.logger.Debug("sent heartbeat ping",
				"tunnel_id", s.registered.TunnelID,
				"sequence", sequence,
			)
			sequence++
		}
	}
}

func backoffDelay(attempt int) time.Duration {
	if attempt < 0 {
		return reconnectBackoff[0]
	}
	if attempt >= len(reconnectBackoff) {
		return reconnectBackoff[len(reconnectBackoff)-1]
	}
	return reconnectBackoff[attempt]
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func syncOnceCloser(closeFn func() error) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = closeFn()
		})
	}
}
