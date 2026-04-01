package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

func (s *TunnelSession) handleRequest(ctx context.Context, requestID string, request api.RequestPayload) {
	s.logger.Info("received forwarded request",
		"tunnel_id", s.registered.TunnelID,
		"request_id", requestID,
		"method", request.Method,
		"path", request.Path,
	)

	response, err := s.proxyRequest(ctx, request)
	if err != nil {
		s.logger.Warn("local proxy request failed",
			"tunnel_id", s.registered.TunnelID,
			"request_id", requestID,
			"error", err,
		)
		_ = s.sendProtocolError(requestID, "local_proxy_failed", err.Error())
		return
	}

	message, err := api.NewMessage(api.MessageTypeResponse, response)
	if err != nil {
		s.logger.Warn("failed to encode response message",
			"tunnel_id", s.registered.TunnelID,
			"request_id", requestID,
			"error", err,
		)
		_ = s.sendProtocolError(requestID, "encode_response_failed", err.Error())
		return
	}
	message.ID = requestID

	if err := s.sendMessage(message); err != nil {
		s.logger.Warn("failed to send forwarded response",
			"tunnel_id", s.registered.TunnelID,
			"request_id", requestID,
			"error", err,
		)
		return
	}

	s.logger.Info("returned forwarded response",
		"tunnel_id", s.registered.TunnelID,
		"request_id", requestID,
		"status", response.Status,
	)
}

func (s *TunnelSession) proxyRequest(ctx context.Context, request api.RequestPayload) (api.ResponsePayload, error) {
	targetURL, err := localForwardURL(s.registered.Target, request.Path)
	if err != nil {
		return api.ResponsePayload{}, err
	}

	proxyReq, err := http.NewRequestWithContext(ctx, request.Method, targetURL, strings.NewReader(request.Body))
	if err != nil {
		return api.ResponsePayload{}, err
	}

	proxyReq.Header = cloneHeaders(request.Headers)
	if request.Host != "" {
		proxyReq.Host = request.Host
	}

	resp, err := s.httpClient.Do(proxyReq)
	if err != nil {
		return api.ResponsePayload{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return api.ResponsePayload{}, err
	}

	return api.ResponsePayload{
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
		Body:    string(body),
	}, nil
}

func (s *TunnelSession) sendProtocolError(requestID, code, message string) error {
	protocolMessage, err := api.NewMessage(api.MessageTypeError, api.ProtocolErrorPayload{
		Code:    code,
		Message: message,
	})
	if err != nil {
		return err
	}
	protocolMessage.ID = requestID
	return s.sendMessage(protocolMessage)
}

func (s *TunnelSession) send(messageType api.MessageType, payload any) error {
	message, err := api.NewMessage(messageType, payload)
	if err != nil {
		return err
	}
	return s.sendMessage(message)
}

func (s *TunnelSession) sendMessage(message api.Message) error {
	if err := s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return s.codec.Send(message)
}

func localForwardURL(baseTarget, requestPath string) (string, error) {
	base, err := url.Parse(baseTarget)
	if err != nil {
		return "", fmt.Errorf("parse base target: %w", err)
	}

	pathURL, err := url.Parse(requestPath)
	if err != nil {
		return "", fmt.Errorf("parse request path: %w", err)
	}

	return base.ResolveReference(pathURL).String(), nil
}

func cloneHeaders(headers map[string][]string) http.Header {
	out := make(http.Header, len(headers))
	for key, values := range headers {
		cloned := make([]string, len(values))
		copy(cloned, values)
		out[key] = cloned
	}
	return out
}
