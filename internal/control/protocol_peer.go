package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sardorazimov/binboi-go/internal/session"
	"github.com/sardorazimov/binboi-go/pkg/api"
)

var (
	errTunnelNotFound      = errors.New("tunnel not found")
	errTunnelUnavailable   = errors.New("tunnel unavailable")
	errTunnelUnsupported   = errors.New("tunnel protocol does not support HTTP forwarding")
	errStreamIdleTimeout   = errors.New("stream idle timeout")
	errStreamTimedOut      = errors.New("stream timed out")
	errStreamBackpressured = errors.New("stream backpressure wait failed")
)

const (
	streamStateWaiting  = "waiting"
	streamStateActive   = "active"
	streamStateFinished = "finished"
	streamStateCanceled = "canceled"
	streamStateFailed   = "failed"
)

type responseStartResult struct {
	start api.ResponseStartPayload
	err   error
}

type pendingRequest struct {
	id        string
	sessionID string
	peer      *protocolPeer
	logger    *slog.Logger

	ctx        context.Context
	cancelFn   context.CancelFunc
	finishedCh chan struct{}
	doneCh     chan error
	startCh    chan responseStartResult

	bodyReader *io.PipeReader
	bodyWriter *io.PipeWriter

	responseChunks chan []byte
	pumpDone       chan struct{}

	releaseOnce sync.Once
	chunksOnce  sync.Once

	mu            sync.Mutex
	state         string
	responseStart bool
	completed     bool
	cancelSent    bool
	createdAt     time.Time
	lastActivity  time.Time
	bytesSent     int64
	bytesReceived int64
	idleTimer     *time.Timer
}

func newPendingRequest(id string, peer *protocolPeer, parent context.Context) *pendingRequest {
	streamCtx, cancel := context.WithTimeout(parent, peer.flowControl.StreamTimeout())
	reader, writer := io.Pipe()
	now := time.Now().UTC()

	request := &pendingRequest{
		id:             id,
		sessionID:      peer.session.ID,
		peer:           peer,
		logger:         peer.logger,
		ctx:            streamCtx,
		cancelFn:       cancel,
		finishedCh:     make(chan struct{}),
		doneCh:         make(chan error, 1),
		startCh:        make(chan responseStartResult, 1),
		bodyReader:     reader,
		bodyWriter:     writer,
		responseChunks: make(chan []byte, peer.flowControl.BufferedFrameCapacity()),
		pumpDone:       make(chan struct{}),
		state:          streamStateWaiting,
		createdAt:      now,
		lastActivity:   now,
	}

	if idleTimeout := peer.flowControl.StreamIdleTimeout(); idleTimeout > 0 {
		request.idleTimer = time.AfterFunc(idleTimeout, request.onIdleTimeout)
	}

	go request.pumpResponseBody()
	return request
}

func (r *pendingRequest) waitForResponseStart() (api.ResponseStartPayload, error) {
	select {
	case result := <-r.startCh:
		return result.start, result.err
	case <-r.ctx.Done():
		return api.ResponseStartPayload{}, r.contextErr()
	}
}

func (r *pendingRequest) waitForDone() error {
	select {
	case err := <-r.doneCh:
		return err
	case <-r.ctx.Done():
		return r.contextErr()
	}
}

func (r *pendingRequest) handleResponseStart(start api.ResponseStartPayload) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.completed {
		return fmt.Errorf("response_start after completion")
	}
	if r.responseStart {
		return fmt.Errorf("duplicate response_start")
	}

	r.responseStart = true
	r.state = streamStateActive
	r.touchLocked(0, 0)
	r.startCh <- responseStartResult{start: start}
	return nil
}

func (r *pendingRequest) writeResponseBody(chunk []byte) error {
	r.mu.Lock()
	if r.completed {
		r.mu.Unlock()
		return fmt.Errorf("response_body after completion")
	}
	if !r.responseStart {
		r.mu.Unlock()
		return fmt.Errorf("response_body before response_start")
	}
	r.touchLocked(0, int64(len(chunk)))
	r.mu.Unlock()

	if len(chunk) == 0 {
		return nil
	}

	select {
	case r.responseChunks <- append([]byte(nil), chunk...):
		return nil
	case <-r.ctx.Done():
		return r.contextErr()
	}
}

func (r *pendingRequest) finish(err error) {
	r.mu.Lock()
	if r.completed {
		r.mu.Unlock()
		return
	}
	r.completed = true
	if err == nil {
		r.state = streamStateFinished
	} else if errors.Is(err, context.Canceled) || errors.Is(err, errStreamIdleTimeout) || errors.Is(err, errStreamTimedOut) {
		r.state = streamStateCanceled
	} else {
		r.state = streamStateFailed
	}
	responseStarted := r.responseStart
	createdAt := r.createdAt
	lastActivity := r.lastActivity
	bytesSent := r.bytesSent
	bytesReceived := r.bytesReceived
	if r.idleTimer != nil {
		r.idleTimer.Stop()
	}
	r.mu.Unlock()

	if err != nil {
		_ = r.bodyWriter.CloseWithError(err)
	}
	r.closeChunkStream()
	<-r.pumpDone
	if err == nil {
		_ = r.bodyWriter.Close()
	}
	if !responseStarted {
		r.startCh <- responseStartResult{err: err}
	}
	r.doneCh <- err
	r.releaseSlot()
	close(r.finishedCh)
	r.cancelFn()

	r.logger.Info("stream completed",
		"tunnel_id", r.sessionID,
		"request_id", r.id,
		"state", r.stateSnapshot(),
		"bytes_sent", bytesSent,
		"bytes_received", bytesReceived,
		"duration", time.Since(createdAt),
		"idle_for", time.Since(lastActivity),
		"error", err,
	)
}

func (r *pendingRequest) cancel(reason string, err error) {
	r.mu.Lock()
	if r.completed || r.cancelSent {
		r.mu.Unlock()
		return
	}
	r.cancelSent = true
	r.state = streamStateCanceled
	r.touchLocked(0, 0)
	r.mu.Unlock()

	cancelMessage, encodeErr := api.NewMessage(api.MessageTypeRequestCancel, api.RequestCancelPayload{Reason: reason})
	if encodeErr == nil {
		cancelMessage.ID = r.id
		cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if sendErr := r.peer.sendStream(cancelCtx, r.id, cancelMessage, true); sendErr != nil {
			r.logger.Warn("failed to send request cancel",
				"tunnel_id", r.sessionID,
				"request_id", r.id,
				"error", sendErr,
			)
		}
	}

	r.finish(err)
}

func (r *pendingRequest) markRequestBytesSent(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.touchLocked(int64(n), 0)
}

func (r *pendingRequest) onIdleTimeout() {
	r.logger.Warn("stream idle timeout reached",
		"tunnel_id", r.sessionID,
		"request_id", r.id,
	)
	r.cancel("stream idle timeout", errStreamIdleTimeout)
	r.peer.removePending(r.id)
}

func (r *pendingRequest) releaseSlot() {
	r.releaseOnce.Do(func() {
		r.peer.releaseStreamSlot()
	})
}

func (r *pendingRequest) closeChunkStream() {
	r.chunksOnce.Do(func() {
		close(r.responseChunks)
	})
}

func (r *pendingRequest) touchLocked(sentDelta, receivedDelta int64) {
	r.lastActivity = time.Now().UTC()
	r.bytesSent += sentDelta
	r.bytesReceived += receivedDelta
	if r.idleTimer != nil {
		_ = r.idleTimer.Reset(r.peer.flowControl.StreamIdleTimeout())
	}
}

func (r *pendingRequest) stateSnapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *pendingRequest) contextErr() error {
	if errors.Is(r.ctx.Err(), context.DeadlineExceeded) {
		return errStreamTimedOut
	}
	if errors.Is(r.ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	return r.ctx.Err()
}

func (r *pendingRequest) pumpResponseBody() {
	defer close(r.pumpDone)

	for chunk := range r.responseChunks {
		if len(chunk) == 0 {
			continue
		}
		if _, err := r.bodyWriter.Write(chunk); err != nil {
			return
		}
	}
}

type protocolPeer struct {
	session api.Session
	conn    net.Conn
	codec   *api.MessageCodec
	logger  *slog.Logger
	manager *session.Manager

	flowControl api.FlowControl
	dispatcher  *api.MessageDispatcher
	streamSlots chan struct{}
	closedCh    chan struct{}
	closeOnce   sync.Once

	nextRequestID atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]*pendingRequest
}

func newProtocolPeer(session api.Session, conn net.Conn, codec *api.MessageCodec, logger *slog.Logger, manager *session.Manager, flowControl api.FlowControl) *protocolPeer {
	peer := &protocolPeer{
		session:     session,
		conn:        conn,
		codec:       codec,
		logger:      logger,
		manager:     manager,
		flowControl: flowControl.Normalize(),
		streamSlots: make(chan struct{}, flowControl.Normalize().MaxConcurrentStreams),
		closedCh:    make(chan struct{}),
		pending:     make(map[string]*pendingRequest),
	}

	peer.dispatcher = api.NewMessageDispatcher(peer.rawSend, api.DispatcherConfig{
		StreamQueueCapacity: peer.flowControl.BufferedFrameCapacity(),
	}, func(err error) {
		peer.logger.Warn("stream dispatcher stopped",
			"tunnel_id", peer.session.ID,
			"error", err,
		)
		peer.close()
		_ = peer.conn.Close()
	})

	return peer
}

func (p *protocolPeer) forwardHTTP(ctx context.Context, request *http.Request) (*pendingRequest, error) {
	if err := p.acquireStreamSlot(ctx); err != nil {
		return nil, err
	}

	start := requestStartFromHTTP(request)
	requestID := p.newRequestID()
	pending := newPendingRequest(requestID, p, ctx)

	p.pendingMu.Lock()
	p.pending[requestID] = pending
	inFlight := len(p.pending)
	p.pendingMu.Unlock()
	p.manager.SetInFlight(p.session.ID, inFlight)

	startMessage, err := api.NewMessage(api.MessageTypeRequestStart, start)
	if err != nil {
		p.removePending(requestID)
		return nil, err
	}
	startMessage.ID = requestID

	if err := p.sendStream(pending.ctx, requestID, startMessage, false); err != nil {
		p.removePending(requestID)
		return nil, err
	}

	p.logger.Info("forwarded request_start to tunnel client",
		"tunnel_id", p.session.ID,
		"request_id", requestID,
		"method", start.Method,
		"path", start.Path,
		"in_flight", inFlight,
	)

	go p.streamRequestBody(pending, request.Body)
	go p.watchRequestContext(pending)

	return pending, nil
}

func (p *protocolPeer) streamRequestBody(pending *pendingRequest, body io.ReadCloser) {
	defer body.Close()

	buf := make([]byte, api.DefaultBodyChunkSize)
	chunks := 0

	for {
		if err := pending.ctx.Err(); err != nil {
			return
		}

		n, err := body.Read(buf)
		if n > 0 {
			chunkPayload := api.BodyChunkPayload{Chunk: append([]byte(nil), buf[:n]...)}
			message, encodeErr := api.NewMessage(api.MessageTypeRequestBody, chunkPayload)
			if encodeErr != nil {
				pending.cancel("encode request body failed", encodeErr)
				p.removePending(pending.id)
				return
			}
			message.ID = pending.id
			if sendErr := p.sendStream(pending.ctx, pending.id, message, false); sendErr != nil {
				pending.cancel("request body send failed", wrapBackpressure(sendErr))
				p.removePending(pending.id)
				return
			}
			chunks++
			pending.markRequestBytesSent(n)
			p.logger.Debug("forwarded request_body chunk",
				"tunnel_id", p.session.ID,
				"request_id", pending.id,
				"chunk_size", n,
				"chunk_index", chunks,
			)
		}

		if err == io.EOF {
			endMessage, encodeErr := api.NewMessage(api.MessageTypeRequestEnd, api.StreamEndPayload{})
			if encodeErr != nil {
				pending.cancel("encode request end failed", encodeErr)
				p.removePending(pending.id)
				return
			}
			endMessage.ID = pending.id
			if sendErr := p.sendStream(pending.ctx, pending.id, endMessage, true); sendErr != nil {
				pending.cancel("request end send failed", wrapBackpressure(sendErr))
				p.removePending(pending.id)
				return
			}
			p.logger.Debug("forwarded request_end",
				"tunnel_id", p.session.ID,
				"request_id", pending.id,
				"chunks", chunks,
			)
			return
		}

		if err != nil {
			pending.cancel("request body read failed", err)
			p.removePending(pending.id)
			return
		}
	}
}

func (p *protocolPeer) watchRequestContext(pending *pendingRequest) {
	select {
	case <-pending.finishedCh:
		return
	case <-pending.ctx.Done():
		p.logger.Info("request context canceled, propagating cancel",
			"tunnel_id", p.session.ID,
			"request_id", pending.id,
			"error", pending.contextErr(),
		)
		pending.cancel("server request canceled", pending.contextErr())
		p.removePending(pending.id)
	}
}

func (p *protocolPeer) handleResponseStart(id string, start api.ResponseStartPayload) error {
	pending, ok := p.pendingByID(id)
	if !ok {
		return fmt.Errorf("unknown request for response_start")
	}
	return pending.handleResponseStart(start)
}

func (p *protocolPeer) handleResponseBody(id string, chunk []byte) error {
	pending, ok := p.pendingByID(id)
	if !ok {
		return fmt.Errorf("unknown request for response_body")
	}
	return pending.writeResponseBody(chunk)
}

func (p *protocolPeer) handleResponseEnd(id string) error {
	pending, ok := p.takePending(id)
	if !ok {
		return fmt.Errorf("unknown request for response_end")
	}
	pending.finish(nil)
	return nil
}

func (p *protocolPeer) handleRequestError(id string, protocolErr api.ProtocolErrorPayload) bool {
	pending, ok := p.takePending(id)
	if !ok {
		return false
	}
	pending.finish(fmt.Errorf("%s (%s)", protocolErr.Message, protocolErr.Code))
	return true
}

func (p *protocolPeer) failPending(err error) {
	p.pendingMu.Lock()
	pending := p.pending
	p.pending = make(map[string]*pendingRequest)
	p.pendingMu.Unlock()
	p.manager.SetInFlight(p.session.ID, 0)

	for _, request := range pending {
		request.finish(err)
	}
}

func (p *protocolPeer) sendControl(ctx context.Context, message api.Message) error {
	return p.dispatcher.EnqueueControl(ctx, message)
}

func (p *protocolPeer) sendProtocolError(ctx context.Context, id, code, message string) error {
	protocolMessage, err := api.NewMessage(api.MessageTypeError, api.ProtocolErrorPayload{
		Code:    code,
		Message: message,
	})
	if err != nil {
		return err
	}
	protocolMessage.ID = id
	return p.sendControl(ctx, protocolMessage)
}

func (p *protocolPeer) sendStream(ctx context.Context, requestID string, message api.Message, final bool) error {
	start := time.Now()
	err := p.dispatcher.EnqueueStream(ctx, requestID, message, final)
	if waited := time.Since(start); waited > 25*time.Millisecond {
		p.logger.Warn("stream backpressure engaged",
			"tunnel_id", p.session.ID,
			"request_id", requestID,
			"message_type", message.Type,
			"wait", waited,
		)
	}
	return err
}

func (p *protocolPeer) rawSend(message api.Message) error {
	if err := p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return p.codec.Send(message)
}

func (p *protocolPeer) close() {
	p.closeOnce.Do(func() {
		close(p.closedCh)
		if p.dispatcher != nil {
			p.dispatcher.Close(errTunnelUnavailable)
		}
	})
}

func (p *protocolPeer) acquireStreamSlot(ctx context.Context) error {
	start := time.Now()
	select {
	case p.streamSlots <- struct{}{}:
		if waited := time.Since(start); waited > 25*time.Millisecond {
			p.logger.Warn("stream admission waited for capacity",
				"tunnel_id", p.session.ID,
				"wait", waited,
				"max_streams", p.flowControl.MaxConcurrentStreams,
			)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closedCh:
		return errTunnelUnavailable
	}
}

func (p *protocolPeer) releaseStreamSlot() {
	select {
	case <-p.streamSlots:
	default:
	}
}

func (p *protocolPeer) newRequestID() string {
	sequence := p.nextRequestID.Add(1)
	return fmt.Sprintf("%s-%06d", p.session.ID, sequence)
}

func (p *protocolPeer) pendingByID(id string) (*pendingRequest, bool) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	request, ok := p.pending[id]
	return request, ok
}

func (p *protocolPeer) takePending(id string) (*pendingRequest, bool) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	request, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.manager.SetInFlight(p.session.ID, len(p.pending))
	return request, ok
}

func (p *protocolPeer) removePending(id string) {
	p.pendingMu.Lock()
	request, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	remaining := len(p.pending)
	p.pendingMu.Unlock()
	p.manager.SetInFlight(p.session.ID, remaining)
	if ok {
		request.releaseSlot()
	}
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return parsedHost
	}
	return host
}

func requestStartFromHTTP(r *http.Request) api.RequestStartPayload {
	return api.RequestStartPayload{
		Method:  r.Method,
		Path:    r.URL.RequestURI(),
		Host:    normalizeHost(r.Host),
		Headers: r.Header.Clone(),
	}
}

func wrapBackpressure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, api.ErrDispatcherClosed) {
		return errTunnelUnavailable
	}
	return fmt.Errorf("%w: %v", errStreamBackpressured, err)
}
