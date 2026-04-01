package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/sardorazimov/binboi-go/internal/session"
	"github.com/sardorazimov/binboi-go/pkg/api"
)

const registerTimeout = 10 * time.Second

type protocolServer struct {
	address           string
	heartbeatInterval time.Duration
	logger            *slog.Logger
	manager           *session.Manager

	mu       sync.RWMutex
	listener net.Listener
}

func newProtocolServer(address string, heartbeatInterval time.Duration, logger *slog.Logger, manager *session.Manager) *protocolServer {
	return &protocolServer{
		address:           address,
		heartbeatInterval: heartbeatInterval,
		logger:            logger,
		manager:           manager,
	}
}

func (s *protocolServer) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return err
	}
	defer listener.Close()

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()

	s.logger.Info("stream control listener ready",
		"address", listener.Addr().String(),
		"heartbeat_interval", s.heartbeatInterval,
	)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Temporary() {
				s.logger.Warn("temporary accept error", "error", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}

		go s.handleConnection(ctx, conn)
	}
}

func (s *protocolServer) Address() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *protocolServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	codec := api.NewMessageCodec(conn, conn)
	remoteAddr := conn.RemoteAddr().String()
	s.logger.Info("control client connected", "remote_addr", remoteAddr)

	sessionID := ""
	localPort := 0

	defer func() {
		if sessionID != "" {
			s.manager.Remove(sessionID)
			s.logger.Info("control client disconnected",
				"remote_addr", remoteAddr,
				"tunnel_id", sessionID,
				"local_port", localPort,
			)
		} else {
			s.logger.Info("control client disconnected before registration", "remote_addr", remoteAddr)
		}
	}()

	if err := conn.SetReadDeadline(time.Now().Add(registerTimeout)); err != nil {
		s.logger.Warn("failed to set register deadline", "remote_addr", remoteAddr, "error", err)
		return
	}

	firstMessage, err := codec.Receive()
	if err != nil {
		s.logger.Warn("failed to read register message", "remote_addr", remoteAddr, "error", err)
		return
	}

	if firstMessage.Type != api.MessageTypeRegister {
		s.sendProtocolError(conn, codec, "invalid_register", "first message must be register")
		return
	}

	var register api.RegisterPayload
	if err := firstMessage.Decode(&register); err != nil {
		s.sendProtocolError(conn, codec, "malformed_payload", err.Error())
		return
	}
	if err := register.Validate(); err != nil {
		s.sendProtocolError(conn, codec, "invalid_register", err.Error())
		return
	}

	registeredSession, err := s.manager.RegisterTunnel(ctx, session.RegisterRequest{
		Protocol:  register.Protocol,
		LocalPort: register.LocalPort,
		AuthToken: register.AuthToken,
		Metadata:  register.Metadata,
		Conn:      conn,
	})
	if err != nil {
		s.sendProtocolError(conn, codec, "registration_failed", err.Error())
		s.logger.Warn("tunnel registration failed",
			"remote_addr", remoteAddr,
			"protocol", register.Protocol,
			"local_port", register.LocalPort,
			"error", err,
		)
		return
	}

	sessionID = registeredSession.ID
	localPort = register.LocalPort

	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.logger.Warn("failed to clear connection deadlines", "tunnel_id", sessionID, "error", err)
		return
	}

	registeredMessage, err := api.NewMessage(api.MessageTypeRegistered, api.RegisteredPayload{
		TunnelID:                 registeredSession.ID,
		Protocol:                 registeredSession.Protocol,
		LocalPort:                registeredSession.LocalPort,
		Target:                   registeredSession.Target,
		PublicURL:                registeredSession.PublicURL,
		Status:                   registeredSession.Status,
		HeartbeatIntervalSeconds: int(s.heartbeatInterval / time.Second),
	})
	if err != nil {
		s.logger.Warn("failed to encode registered response", "tunnel_id", sessionID, "error", err)
		return
	}
	if err := s.send(conn, codec, registeredMessage); err != nil {
		s.logger.Warn("failed to send registered response", "tunnel_id", sessionID, "error", err)
		return
	}

	s.logger.Info("tunnel registration succeeded",
		"remote_addr", remoteAddr,
		"tunnel_id", sessionID,
		"protocol", registeredSession.Protocol,
		"local_port", registeredSession.LocalPort,
		"public_url", registeredSession.PublicURL,
	)

	for {
		timeout := (s.heartbeatInterval * 2) + 5*time.Second
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			s.logger.Warn("failed to set heartbeat deadline", "tunnel_id", sessionID, "error", err)
			return
		}

		message, err := codec.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}

			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				s.logger.Warn("heartbeat timeout",
					"tunnel_id", sessionID,
					"remote_addr", remoteAddr,
					"timeout", timeout,
				)
				return
			}

			s.logger.Warn("control connection read failed",
				"tunnel_id", sessionID,
				"remote_addr", remoteAddr,
				"error", err,
			)
			return
		}

		switch message.Type {
		case api.MessageTypePing:
			var ping api.PingPayload
			if err := message.Decode(&ping); err != nil {
				s.sendProtocolError(conn, codec, "malformed_payload", err.Error())
				return
			}
			if err := s.manager.TouchHeartbeat(sessionID, time.Now().UTC()); err != nil {
				s.sendProtocolError(conn, codec, "unknown_session", err.Error())
				return
			}

			s.logger.Debug("received heartbeat ping",
				"tunnel_id", sessionID,
				"sequence", ping.Sequence,
			)

			pongMessage, err := api.NewMessage(api.MessageTypePong, api.PongPayload{
				Sequence:   ping.Sequence,
				ReceivedAt: time.Now().UTC(),
			})
			if err != nil {
				s.logger.Warn("failed to encode pong", "tunnel_id", sessionID, "error", err)
				return
			}
			if err := s.send(conn, codec, pongMessage); err != nil {
				s.logger.Warn("failed to send pong", "tunnel_id", sessionID, "error", err)
				return
			}

			s.logger.Debug("sent heartbeat pong",
				"tunnel_id", sessionID,
				"sequence", ping.Sequence,
			)
		case api.MessageTypeClose:
			var closePayload api.ClosePayload
			if message.Payload != nil {
				if err := message.Decode(&closePayload); err != nil {
					s.logger.Warn("failed to decode close message", "tunnel_id", sessionID, "error", err)
				}
			}
			s.logger.Info("client requested disconnect",
				"tunnel_id", sessionID,
				"reason", closePayload.Reason,
			)
			return
		default:
			s.sendProtocolError(conn, codec, "unsupported_message", fmt.Sprintf("message type %q is not supported yet", message.Type))
			return
		}
	}
}

func (s *protocolServer) sendProtocolError(conn net.Conn, codec *api.MessageCodec, code, message string) {
	protocolMessage, err := api.NewMessage(api.MessageTypeError, api.ProtocolErrorPayload{
		Code:    code,
		Message: message,
	})
	if err != nil {
		s.logger.Warn("failed to encode protocol error", "code", code, "error", err)
		return
	}
	if err := s.send(conn, codec, protocolMessage); err != nil {
		s.logger.Warn("failed to send protocol error", "code", code, "error", err)
	}
}

func (s *protocolServer) send(conn net.Conn, codec *api.MessageCodec, message api.Message) error {
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return codec.Send(message)
}
