package control

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

	"github.com/sardorazimov/binboi-go/internal/auth"
	"github.com/sardorazimov/binboi-go/internal/session"
	"github.com/sardorazimov/binboi-go/pkg/api"
)

const registerTimeout = 10 * time.Second

type protocolServer struct {
	address           string
	heartbeatInterval time.Duration
	flowControl       api.FlowControl
	logger            *slog.Logger
	manager           *session.Manager
	validator         TokenValidator

	mu       sync.RWMutex
	listener net.Listener
	peers    map[string]*protocolPeer
}

func newProtocolServer(address string, heartbeatInterval time.Duration, flowControl api.FlowControl, logger *slog.Logger, manager *session.Manager, validator TokenValidator) *protocolServer {
	return &protocolServer{
		address:           address,
		heartbeatInterval: heartbeatInterval,
		flowControl:       flowControl.Normalize(),
		logger:            logger,
		manager:           manager,
		validator:         validator,
		peers:             make(map[string]*protocolPeer),
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

func (s *protocolServer) ForwardRequest(ctx context.Context, host string, request *http.Request) (*pendingRequest, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, errTunnelNotFound
	}

	sessionState, ok := s.manager.LookupByHost(host)
	if !ok {
		return nil, errTunnelNotFound
	}
	if sessionState.Connection != "connected" {
		return nil, errTunnelUnavailable
	}
	if sessionState.Protocol != "http" && sessionState.Protocol != "https" {
		return nil, errTunnelUnsupported
	}

	peer := s.peerBySessionID(sessionState.ID)
	if peer == nil {
		return nil, errTunnelUnavailable
	}

	return peer.forwardHTTP(ctx, request)
}

func (s *protocolServer) handleConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	codec := api.NewMessageCodec(conn, conn)
	remoteAddr := conn.RemoteAddr().String()
	s.logger.Info("control client connected", "remote_addr", remoteAddr)

	var (
		sessionID string
		localPort int
		peer      *protocolPeer
	)

	defer func() {
		if sessionID != "" {
			if peer != nil {
				peer.close()
			}
			s.removePeer(sessionID, peer)
			s.manager.DetachConnection(sessionID, time.Now().UTC())
			if peer != nil {
				peer.failPending(errTunnelUnavailable)
			}
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
		s.sendProtocolError(conn, codec, "", "invalid_register", "first message must be register")
		return
	}

	var register api.RegisterPayload
	if err := firstMessage.Decode(&register); err != nil {
		s.sendProtocolError(conn, codec, "", "malformed_payload", err.Error())
		return
	}
	if err := register.Validate(); err != nil {
		s.sendProtocolError(conn, codec, "", "invalid_register", err.Error())
		return
	}

	var principal auth.Principal
	if s.validator != nil {
		principal, err = s.validator.Validate(ctx, register.AuthToken)
		if err != nil {
			errorCode := "invalid_token"
			switch {
			case errors.Is(err, auth.ErrTokenRequired):
				errorCode = "token_required"
			case errors.Is(err, auth.ErrRevokedToken):
				errorCode = "revoked_token"
			case errors.Is(err, auth.ErrInvalidToken):
				errorCode = "invalid_token"
			}
			s.sendProtocolError(conn, codec, "", errorCode, err.Error())
			s.logger.Warn("tunnel authentication failed",
				"remote_addr", remoteAddr,
				"protocol", register.Protocol,
				"local_port", register.LocalPort,
				"resume_tunnel_id", register.ResumeTunnelID,
				"error", err,
			)
			return
		}
	}

	registerResult, err := s.manager.RegisterTunnel(ctx, session.RegisterRequest{
		Protocol:       register.Protocol,
		LocalPort:      register.LocalPort,
		UserID:         principal.UserID,
		TokenID:        principal.TokenID,
		TokenPrefix:    principal.Prefix,
		Metadata:       register.Metadata,
		Conn:           conn,
		ResumeTunnelID: register.ResumeTunnelID,
		ResumeToken:    register.ResumeToken,
	})
	if err != nil {
		errorCode := "registration_failed"
		if errors.Is(err, session.ErrInvalidResume) {
			errorCode = "invalid_resume"
		}
		s.sendProtocolError(conn, codec, "", errorCode, err.Error())
		s.logger.Warn("tunnel registration failed",
			"remote_addr", remoteAddr,
			"protocol", register.Protocol,
			"local_port", register.LocalPort,
			"resume_tunnel_id", register.ResumeTunnelID,
			"error", err,
		)
		return
	}

	registeredSession := registerResult.Session
	sessionID = registeredSession.ID
	localPort = register.LocalPort
	peer = newProtocolPeer(registeredSession, conn, codec, s.logger, s.manager, s.flowControl)
	s.addPeer(peer)

	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.logger.Warn("failed to clear connection deadlines", "tunnel_id", sessionID, "error", err)
		return
	}

	resumeToken, _ := s.manager.ResumeToken(registeredSession.ID)
	registeredMessage, err := api.NewMessage(api.MessageTypeRegistered, api.RegisteredPayload{
		TunnelID:                 registeredSession.ID,
		Protocol:                 registeredSession.Protocol,
		LocalPort:                registeredSession.LocalPort,
		Target:                   registeredSession.Target,
		PublicURL:                registeredSession.PublicURL,
		Status:                   registeredSession.Status,
		HeartbeatIntervalSeconds: int(s.heartbeatInterval / time.Second),
		ResumeToken:              resumeToken,
		Resumed:                  registerResult.Resumed,
		FlowControl:              s.flowControl,
	})
	if err != nil {
		s.logger.Warn("failed to encode registered response", "tunnel_id", sessionID, "error", err)
		return
	}
	if err := peer.sendControl(ctx, registeredMessage); err != nil {
		s.logger.Warn("failed to send registered response", "tunnel_id", sessionID, "error", err)
		return
	}

	s.logger.Info("tunnel registration succeeded",
		"remote_addr", remoteAddr,
		"tunnel_id", sessionID,
		"user_id", principal.UserID,
		"token_prefix", principal.Prefix,
		"protocol", registeredSession.Protocol,
		"local_port", registeredSession.LocalPort,
		"public_url", registeredSession.PublicURL,
		"resumed", registerResult.Resumed,
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
				_ = peer.sendProtocolError(ctx, message.ID, "malformed_payload", err.Error())
				return
			}
			if err := s.manager.TouchHeartbeat(sessionID, time.Now().UTC()); err != nil {
				_ = peer.sendProtocolError(ctx, message.ID, "unknown_session", err.Error())
				return
			}

			pongMessage, err := api.NewMessage(api.MessageTypePong, api.PongPayload{
				Sequence:   ping.Sequence,
				ReceivedAt: time.Now().UTC(),
			})
			if err != nil {
				s.logger.Warn("failed to encode pong", "tunnel_id", sessionID, "error", err)
				return
			}
			pongMessage.ID = message.ID
			if err := peer.sendControl(ctx, pongMessage); err != nil {
				s.logger.Warn("failed to send pong", "tunnel_id", sessionID, "error", err)
				return
			}
		case api.MessageTypeResponseStart:
			var responseStart api.ResponseStartPayload
			if err := message.Decode(&responseStart); err != nil {
				_ = peer.sendProtocolError(ctx, message.ID, "malformed_payload", err.Error())
				continue
			}
			if message.ID == "" {
				_ = peer.sendProtocolError(ctx, "", "malformed_payload", "response_start missing id")
				continue
			}
			if err := peer.handleResponseStart(message.ID, responseStart); err != nil {
				s.logger.Warn("received invalid response_start",
					"tunnel_id", sessionID,
					"request_id", message.ID,
					"error", err,
				)
				_ = peer.sendProtocolError(ctx, message.ID, "invalid_frame_order", err.Error())
				continue
			}

			s.logger.Info("received response_start",
				"tunnel_id", sessionID,
				"request_id", message.ID,
				"status", responseStart.Status,
			)
		case api.MessageTypeResponseBody:
			var body api.BodyChunkPayload
			if err := message.Decode(&body); err != nil {
				_ = peer.sendProtocolError(ctx, message.ID, "malformed_payload", err.Error())
				continue
			}
			if message.ID == "" {
				_ = peer.sendProtocolError(ctx, "", "malformed_payload", "response_body missing id")
				continue
			}
			if err := peer.handleResponseBody(message.ID, body.Chunk); err != nil {
				s.logger.Warn("received invalid response_body",
					"tunnel_id", sessionID,
					"request_id", message.ID,
					"error", err,
				)
				_ = peer.sendProtocolError(ctx, message.ID, "invalid_frame_order", err.Error())
				continue
			}
			s.logger.Debug("received response_body chunk",
				"tunnel_id", sessionID,
				"request_id", message.ID,
				"chunk_size", len(body.Chunk),
			)
		case api.MessageTypeResponseEnd:
			if message.Payload != nil {
				var end api.StreamEndPayload
				if err := message.Decode(&end); err != nil {
					_ = peer.sendProtocolError(ctx, message.ID, "malformed_payload", err.Error())
					continue
				}
			}
			if message.ID == "" {
				_ = peer.sendProtocolError(ctx, "", "malformed_payload", "response_end missing id")
				continue
			}
			if err := peer.handleResponseEnd(message.ID); err != nil {
				s.logger.Warn("received invalid response_end",
					"tunnel_id", sessionID,
					"request_id", message.ID,
					"error", err,
				)
				_ = peer.sendProtocolError(ctx, message.ID, "invalid_frame_order", err.Error())
				continue
			}
			s.logger.Debug("received response_end",
				"tunnel_id", sessionID,
				"request_id", message.ID,
			)
		case api.MessageTypeError:
			var protocolErr api.ProtocolErrorPayload
			if err := message.Decode(&protocolErr); err != nil {
				s.logger.Warn("failed to decode protocol error", "tunnel_id", sessionID, "error", err)
				return
			}

			if message.ID != "" && peer.handleRequestError(message.ID, protocolErr) {
				s.logger.Warn("client reported request error",
					"tunnel_id", sessionID,
					"request_id", message.ID,
					"code", protocolErr.Code,
					"error", protocolErr.Message,
				)
				continue
			}

			s.logger.Warn("client reported session error",
				"tunnel_id", sessionID,
				"code", protocolErr.Code,
				"error", protocolErr.Message,
			)
			return
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
			_ = peer.sendProtocolError(ctx, message.ID, "unsupported_message", fmt.Sprintf("message type %q is not supported yet", message.Type))
			return
		}
	}
}

func (s *protocolServer) addPeer(peer *protocolPeer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.peers[peer.session.ID]; ok && existing != peer {
		existing.failPending(errTunnelUnavailable)
		_ = existing.conn.Close()
	}
	s.peers[peer.session.ID] = peer
}

func (s *protocolServer) removePeer(sessionID string, peer *protocolPeer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.peers[sessionID]
	if ok && (peer == nil || current == peer) {
		delete(s.peers, sessionID)
	}
}

func (s *protocolServer) peerBySessionID(sessionID string) *protocolPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peers[sessionID]
}

func (s *protocolServer) closePeer(sessionID string) error {
	peer := s.peerBySessionID(sessionID)
	if peer == nil {
		return errTunnelNotFound
	}
	return peer.conn.Close()
}

func (s *protocolServer) sendProtocolError(conn net.Conn, codec *api.MessageCodec, id, code, message string) {
	protocolMessage, err := api.NewMessage(api.MessageTypeError, api.ProtocolErrorPayload{
		Code:    code,
		Message: message,
	})
	if err != nil {
		s.logger.Warn("failed to encode protocol error", "code", code, "error", err)
		return
	}
	protocolMessage.ID = id
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.logger.Warn("failed to set error write deadline", "code", code, "error", err)
		return
	}
	if err := codec.Send(protocolMessage); err != nil {
		s.logger.Warn("failed to send protocol error", "code", code, "error", err)
	}
}
