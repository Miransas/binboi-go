package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

const (
	requestStateActive   = "active"
	requestStateFinished = "finished"
	requestStateCanceled = "canceled"
	requestStateFailed   = "failed"
)

type activeRequest struct {
	id     string
	start  api.RequestStartPayload
	cancel context.CancelFunc
	ctx    context.Context

	bodyReader *io.PipeReader
	bodyWriter *io.PipeWriter
	bodyChunks chan []byte
	pumpDone   chan struct{}

	done       chan struct{}
	chunksOnce sync.Once
	rawOnce    sync.Once

	mu           sync.Mutex
	state        string
	finished     bool
	createdAt    time.Time
	lastActivity time.Time
	bytesIn      int64
	bytesOut     int64
	idleTimer    *time.Timer

	rawMu            sync.Mutex
	rawInput         chan []byte
	rawDone          chan struct{}
	rawConn          net.Conn
	rawInputClosed   bool
	upgradeActivated bool
}

func newActiveRequest(session *TunnelSession, id string, start api.RequestStartPayload, parent context.Context, cancel context.CancelFunc) *activeRequest {
	reader, writer := io.Pipe()
	now := time.Now().UTC()
	request := &activeRequest{
		id:           id,
		start:        start,
		cancel:       cancel,
		ctx:          parent,
		bodyReader:   reader,
		bodyWriter:   writer,
		bodyChunks:   make(chan []byte, session.flowControl.BufferedFrameCapacity()),
		pumpDone:     make(chan struct{}),
		done:         make(chan struct{}),
		state:        requestStateActive,
		createdAt:    now,
		lastActivity: now,
	}
	if api.IsUpgradeRequest(start) {
		request.rawInput = make(chan []byte, session.flowControl.BufferedFrameCapacity())
		request.rawDone = make(chan struct{})
	}

	if idleTimeout := session.flowControl.StreamIdleTimeout(); idleTimeout > 0 {
		request.idleTimer = time.AfterFunc(idleTimeout, func() {
			session.logger.Warn("client stream idle timeout reached",
				"tunnel_id", session.registered.TunnelID,
				"request_id", request.id,
			)
			request.cancel()
			_ = request.bodyWriter.CloseWithError(context.DeadlineExceeded)
			_ = session.sendProtocolError(request.id, "stream_idle_timeout", "request stream exceeded idle timeout")
		})
	}

	go request.pumpRequestBody()
	return request
}

func (r *activeRequest) enqueueBody(chunk []byte, idleTimeout time.Duration) error {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return fmt.Errorf("request_body after completion")
	}
	r.bytesIn += int64(len(chunk))
	r.lastActivity = time.Now().UTC()
	if r.idleTimer != nil {
		_ = r.idleTimer.Reset(idleTimeout)
	}
	r.mu.Unlock()

	if len(chunk) == 0 {
		return nil
	}

	select {
	case r.bodyChunks <- append([]byte(nil), chunk...):
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func (r *activeRequest) enqueueRawChunk(chunk []byte, idleTimeout time.Duration) error {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return fmt.Errorf("request_body after completion")
	}
	r.bytesIn += int64(len(chunk))
	r.lastActivity = time.Now().UTC()
	if r.idleTimer != nil {
		_ = r.idleTimer.Reset(idleTimeout)
	}
	rawInput := r.rawInput
	r.mu.Unlock()

	if rawInput == nil || len(chunk) == 0 {
		return nil
	}

	select {
	case rawInput <- append([]byte(nil), chunk...):
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func (r *activeRequest) closeRequestBody() error {
	r.chunksOnce.Do(func() {
		close(r.bodyChunks)
	})
	return nil
}

func (r *activeRequest) closeRawInput() {
	r.rawMu.Lock()
	r.rawInputClosed = true
	rawInput := r.rawInput
	rawConn := r.rawConn
	r.rawMu.Unlock()

	r.rawOnce.Do(func() {
		if rawInput != nil {
			close(rawInput)
		}
	})
	if rawConn != nil {
		_ = closeWrite(rawConn)
	}
}

func (r *activeRequest) fail(err error) {
	if r.idleTimer != nil {
		r.idleTimer.Stop()
	}
	r.chunksOnce.Do(func() {
		close(r.bodyChunks)
	})
	_ = r.bodyWriter.CloseWithError(err)

	r.rawMu.Lock()
	rawConn := r.rawConn
	r.rawMu.Unlock()
	r.rawOnce.Do(func() {
		if r.rawInput != nil {
			close(r.rawInput)
		}
	})
	if rawConn != nil {
		_ = rawConn.Close()
	}
}

func (r *activeRequest) markResponseBytes(n int, idleTimeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bytesOut += int64(n)
	r.lastActivity = time.Now().UTC()
	if r.idleTimer != nil {
		_ = r.idleTimer.Reset(idleTimeout)
	}
}

func (r *activeRequest) finish(state string) {
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.finished = true
	r.state = state
	if r.idleTimer != nil {
		r.idleTimer.Stop()
	}
	r.mu.Unlock()

	r.cancel()
	close(r.done)
}

func (r *activeRequest) snapshot() (string, int64, int64, time.Time, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.bytesIn, r.bytesOut, r.createdAt, r.lastActivity
}

func (r *activeRequest) isUpgrade() bool {
	return api.IsUpgradeRequest(r.start)
}

func (r *activeRequest) setRawConn(conn net.Conn) {
	r.rawMu.Lock()
	r.rawConn = conn
	closed := r.rawInputClosed
	r.upgradeActivated = true
	r.rawMu.Unlock()

	if closed {
		_ = closeWrite(conn)
	}
}

func (r *activeRequest) pumpRequestBody() {
	defer close(r.pumpDone)

	for chunk := range r.bodyChunks {
		if len(chunk) == 0 {
			continue
		}
		if _, err := r.bodyWriter.Write(chunk); err != nil {
			return
		}
	}

	_ = r.bodyWriter.Close()
}

func (s *TunnelSession) handleRequestStart(ctx context.Context, requestID string, start api.RequestStartPayload) error {
	s.activeMu.Lock()
	if len(s.active) >= s.flowControl.MaxConcurrentStreams {
		s.activeMu.Unlock()
		s.logger.Warn("rejecting request_start because client is at stream capacity",
			"tunnel_id", s.registered.TunnelID,
			"request_id", requestID,
			"max_streams", s.flowControl.MaxConcurrentStreams,
		)
		return s.sendProtocolError(requestID, "stream_over_capacity", "client has reached the stream concurrency limit")
	}
	if _, exists := s.active[requestID]; exists {
		s.activeMu.Unlock()
		return s.sendProtocolError(requestID, "duplicate_request_start", "request_start received twice")
	}

	requestCtx, cancel := context.WithTimeout(ctx, s.flowControl.StreamTimeout())
	request := newActiveRequest(s, requestID, start, requestCtx, cancel)
	s.active[requestID] = request
	s.activeMu.Unlock()

	s.logger.Info("received request_start",
		"tunnel_id", s.registered.TunnelID,
		"request_id", requestID,
		"method", start.Method,
		"path", start.Path,
	)

	go s.executeRequest(request)
	return nil
}

func (s *TunnelSession) handleRequestBody(requestID string, chunk []byte) error {
	request, ok := s.requestByID(requestID)
	if !ok {
		return s.sendProtocolError(requestID, "unknown_request", "request_body received for unknown request")
	}

	if request.isUpgrade() {
		if err := request.enqueueRawChunk(chunk, s.flowControl.StreamIdleTimeout()); err != nil {
			return err
		}
	} else {
		if err := request.enqueueBody(chunk, s.flowControl.StreamIdleTimeout()); err != nil {
			return err
		}
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

	if request.isUpgrade() {
		request.closeRawInput()
		s.logger.Debug("received request_end for upgraded stream",
			"tunnel_id", s.registered.TunnelID,
			"request_id", requestID,
		)
		return nil
	}

	if err := request.closeRequestBody(); err != nil {
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

	request.fail(context.Canceled)
	request.finish(requestStateCanceled)
}

func (s *TunnelSession) executeRequest(request *activeRequest) {
	defer s.finishRequest(request.id)

	if request.isUpgrade() {
		s.executeUpgradeRequest(request)
		return
	}

	responseStart, responseBody, err := s.proxyRequest(request.ctx, request)
	if err != nil {
		state := requestStateFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			state = requestStateCanceled
		}
		request.finish(state)

		s.logger.Warn("local proxy request failed",
			"tunnel_id", s.registered.TunnelID,
			"request_id", request.id,
			"error", err,
		)

		if !errors.Is(err, context.Canceled) {
			_ = s.sendProtocolError(request.id, "local_proxy_failed", err.Error())
		}
		return
	}
	defer responseBody.Close()
	if err := s.sendResponseStart(request, responseStart); err != nil {
		request.finish(requestStateFailed)
		return
	}
	s.streamResponseReader(request, responseStart, responseBody)
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
	return s.sendControl(context.Background(), protocolMessage)
}

func (s *TunnelSession) send(ctx context.Context, messageType api.MessageType, payload any) error {
	message, err := api.NewMessage(messageType, payload)
	if err != nil {
		return err
	}
	return s.sendControl(ctx, message)
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

	if !ok {
		return
	}

	request.fail(context.Canceled)
	<-request.pumpDone
	if request.rawDone != nil && request.upgradeActivated {
		waitForRawDone(request.rawDone)
	}
	state, bytesIn, bytesOut, createdAt, lastActivity := request.snapshot()
	s.logger.Info("client stream completed",
		"tunnel_id", s.registered.TunnelID,
		"request_id", request.id,
		"state", state,
		"bytes_in", bytesIn,
		"bytes_out", bytesOut,
		"duration", time.Since(createdAt),
		"idle_for", time.Since(lastActivity),
	)
}

func (s *TunnelSession) cancelActiveRequests(err error) {
	s.activeMu.Lock()
	requests := s.active
	s.active = make(map[string]*activeRequest)
	s.activeMu.Unlock()

	for _, request := range requests {
		request.fail(err)
		request.finish(requestStateCanceled)
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
