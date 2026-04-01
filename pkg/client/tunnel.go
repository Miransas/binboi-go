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
	closeOnce         sync.Once
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
	)

	conn, err := c.dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, fmt.Errorf("dial control protocol: %w", err)
	}

	codec := api.NewMessageCodec(conn, conn)
	if err := conn.SetWriteDeadline(time.Now().Add(defaultHandshakeTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set register write deadline: %w", err)
	}

	registerMessage, err := api.NewMessage(api.MessageTypeRegister, payload)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := codec.Send(registerMessage); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send register message: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(defaultHandshakeTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set register read deadline: %w", err)
	}

	message, err := codec.Receive()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read register response: %w", err)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}

	switch message.Type {
	case api.MessageTypeRegistered:
		var registered api.RegisteredPayload
		if err := message.Decode(&registered); err != nil {
			conn.Close()
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
		}, nil
	case api.MessageTypeError:
		var protocolErr api.ProtocolErrorPayload
		if err := message.Decode(&protocolErr); err != nil {
			conn.Close()
			return nil, err
		}
		conn.Close()
		return nil, fmt.Errorf("registration rejected: %s (%s)", protocolErr.Message, protocolErr.Code)
	default:
		conn.Close()
		return nil, fmt.Errorf("unexpected register response type %q", message.Type)
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
			return fmt.Errorf("control server error: %s (%s)", protocolErr.Message, protocolErr.Code)
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
		case api.MessageTypeRequest:
			var request api.RequestPayload
			if err := message.Decode(&request); err != nil {
				return err
			}
			if message.ID == "" {
				return errors.New("request message missing id")
			}

			go s.handleRequest(ctx, message.ID, request)
		default:
			s.logger.Warn("ignoring unexpected control message",
				"tunnel_id", s.registered.TunnelID,
				"type", message.Type,
			)
		}
	}
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

func (s *TunnelSession) closeConn() {
	s.closeOnce.Do(func() {
		_ = s.conn.Close()
	})
}
