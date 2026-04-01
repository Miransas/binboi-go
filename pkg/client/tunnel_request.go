package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

type activeRequest struct {
	id     string
	start  api.RequestStartPayload
	cancel context.CancelFunc

	bodyReader *io.PipeReader
	bodyWriter *io.PipeWriter

	done     chan struct{}
	finished bool
}

func newActiveRequest(id string, start api.RequestStartPayload, cancel context.CancelFunc) *activeRequest {
	reader, writer := io.Pipe()
	return &activeRequest{
		id:         id,
		start:      start,
		cancel:     cancel,
		bodyReader: reader,
		bodyWriter: writer,
		done:       make(chan struct{}),
	}
}

func (s *TunnelSession) handleRequestStart(ctx context.Context, requestID string, start api.RequestStartPayload) error {
	s.activeMu.Lock()
	if _, exists := s.active[requestID]; exists {
		s.activeMu.Unlock()
		return s.sendProtocolError(requestID, "duplicate_request_start", "request_start received twice")
	}

	requestCtx, cancel := context.WithCancel(ctx)
	request := newActiveRequest(requestID, start, cancel)
	s.active[requestID] = request
	s.activeMu.Unlock()

	s.logger.Info("received request_start",
		"tunnel_id", s.registered.TunnelID,
		"request_id", requestID,
		"method", start.Method,
		"path", start.Path,
	)

	go s.executeRequest(requestCtx, request)
	return nil
}

func (s *TunnelSession) handleRequestBody(requestID string, chunk []byte) error {
	request, ok := s.requestByID(requestID)
	if !ok {
		return s.sendProtocolError(requestID, "unknown_request", "request_body received for unknown request")
	}
	if len(chunk) == 0 {
		return nil
	}

	if _, err := request.bodyWriter.Write(chunk); err != nil {
		return err
	}

	s.logger.Debug("received request_body chunk",
		"tunnel_id", s.registered.TunnelID,
		"request_id", requestID,
		"chunk_size", len(chunk),
	)
	return nil
}

func (s *TunnelSession) handleRequestEnd(requestID string) error {
	request, ok := s.requestByID(requestID)
	if !ok {
		return s.sendProtocolError(requestID, "unknown_request", "request_end received for unknown request")
	}
	if err := request.bodyWriter.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return err
	}

	s.logger.Debug("received request_end",
		"tunnel_id", s.registered.TunnelID,
		"request_id", requestID,
	)
	return nil
}

func (s *TunnelSession) handleRequestCancel(requestID string, cancelPayload api.RequestCancelPayload) {
	request, ok := s.takeRequest(requestID)
	if !ok {
		s.logger.Warn("received request_cancel for unknown request",
			"tunnel_id", s.registered.TunnelID,
			"request_id", requestID,
		)
		return
	}

	s.logger.Info("received request_cancel",
		"tunnel_id", s.registered.TunnelID,
		"request_id", requestID,
		"reason", cancelPayload.Reason,
	)
	request.cancel()
	_ = request.bodyWriter.CloseWithError(context.Canceled)
}

func (s *TunnelSession) executeRequest(ctx context.Context, request *activeRequest) {
	defer s.finishRequest(request.id)
	defer close(request.done)

	responseStart, responseBody, err := s.proxyRequest(ctx, request)
	if err != nil {
		s.logger.Warn("local proxy request failed",
			"tunnel_id", s.registered.TunnelID,
			"request_id", request.id,
			"error", err,
		)
		_ = s.sendProtocolError(request.id, "local_proxy_failed", err.Error())
		return
	}
	defer responseBody.Close()

	startMessage, err := api.NewMessage(api.MessageTypeResponseStart, responseStart)
	if err != nil {
		_ = s.sendProtocolError(request.id, "encode_response_start_failed", err.Error())
		return
	}
	startMessage.ID = request.id
	if err := s.sendMessage(startMessage); err != nil {
		return
	}

	s.logger.Info("sent response_start",
		"tunnel_id", s.registered.TunnelID,
		"request_id", request.id,
		"status", responseStart.Status,
	)

	buf := make([]byte, api.DefaultBodyChunkSize)
	chunkIndex := 0
	for {
		n, readErr := responseBody.Read(buf)
		if n > 0 {
			chunkMessage, err := api.NewMessage(api.MessageTypeResponseBody, api.BodyChunkPayload{
				Chunk: append([]byte(nil), buf[:n]...),
			})
			if err != nil {
				_ = s.sendProtocolError(request.id, "encode_response_body_failed", err.Error())
				return
			}
			chunkMessage.ID = request.id
			if err := s.sendMessage(chunkMessage); err != nil {
				return
			}
			chunkIndex++
			s.logger.Debug("sent response_body chunk",
				"tunnel_id", s.registered.TunnelID,
				"request_id", request.id,
				"chunk_index", chunkIndex,
				"chunk_size", n,
			)
		}

		if readErr == io.EOF {
			endMessage, err := api.NewMessage(api.MessageTypeResponseEnd, api.StreamEndPayload{})
			if err != nil {
				_ = s.sendProtocolError(request.id, "encode_response_end_failed", err.Error())
				return
			}
			endMessage.ID = request.id
			if err := s.sendMessage(endMessage); err != nil {
				return
			}
			s.logger.Debug("sent response_end",
				"tunnel_id", s.registered.TunnelID,
				"request_id", request.id,
				"chunks", chunkIndex,
			)
			return
		}
		if readErr != nil {
			_ = s.sendProtocolError(request.id, "response_body_read_failed", readErr.Error())
			return
		}
	}
}

func (s *TunnelSession) proxyRequest(ctx context.Context, request *activeRequest) (api.ResponseStartPayload, io.ReadCloser, error) {
	targetURL, err := localForwardURL(s.registered.Target, request.start.Path)
	if err != nil {
		return api.ResponseStartPayload{}, nil, err
	}

	proxyReq, err := http.NewRequestWithContext(ctx, request.start.Method, targetURL, request.bodyReader)
	if err != nil {
		return api.ResponseStartPayload{}, nil, err
	}

	proxyReq.Header = cloneHeaders(request.start.Headers)
	if request.start.Host != "" {
		proxyReq.Host = request.start.Host
	}

	resp, err := s.httpClient.Do(proxyReq)
	if err != nil {
		return api.ResponseStartPayload{}, nil, err
	}

	return api.ResponseStartPayload{
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
	}, resp.Body, nil
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

func (s *TunnelSession) requestByID(id string) (*activeRequest, bool) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()

	request, ok := s.active[id]
	return request, ok
}

func (s *TunnelSession) takeRequest(id string) (*activeRequest, bool) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()

	request, ok := s.active[id]
	if ok {
		delete(s.active, id)
	}
	return request, ok
}

func (s *TunnelSession) finishRequest(id string) {
	s.activeMu.Lock()
	request, ok := s.active[id]
	if ok {
		delete(s.active, id)
	}
	s.activeMu.Unlock()

	if ok {
		request.cancel()
	}
}

func (s *TunnelSession) cancelActiveRequests(err error) {
	s.activeMu.Lock()
	requests := s.active
	s.active = make(map[string]*activeRequest)
	s.activeMu.Unlock()

	for _, request := range requests {
		request.cancel()
		_ = request.bodyWriter.CloseWithError(err)
	}
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
