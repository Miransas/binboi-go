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

const forwardTimeout = 30 * time.Second

var (
	errTunnelNotFound    = errors.New("tunnel not found")
	errTunnelUnavailable = errors.New("tunnel unavailable")
	errTunnelUnsupported = errors.New("tunnel protocol does not support HTTP forwarding")
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

	bodyReader *io.PipeReader
	bodyWriter *io.PipeWriter

	startCh chan responseStartResult
	doneCh  chan error

	mu            sync.Mutex
	responseStart bool
	completed     bool
	cancelSent    bool
	finalErr      error
}

func newPendingRequest(id string, peer *protocolPeer) *pendingRequest {
	reader, writer := io.Pipe()
	return &pendingRequest{
		id:         id,
		sessionID:  peer.session.ID,
		peer:       peer,
		logger:     peer.logger,
		bodyReader: reader,
		bodyWriter: writer,
		startCh:    make(chan responseStartResult, 1),
		doneCh:     make(chan error, 1),
	}
}

func (r *pendingRequest) waitForResponseStart(ctx context.Context) (api.ResponseStartPayload, error) {
	select {
	case result := <-r.startCh:
		return result.start, result.err
	case <-ctx.Done():
		return api.ResponseStartPayload{}, ctx.Err()
	}
}

func (r *pendingRequest) waitForDone(ctx context.Context) error {
	select {
	case err := <-r.doneCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
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
	r.mu.Unlock()

	if len(chunk) == 0 {
		return nil
	}
	if _, err := r.bodyWriter.Write(chunk); err != nil {
		return err
	}
	return nil
}

func (r *pendingRequest) finish(err error) {
	r.mu.Lock()
	if r.completed {
		r.mu.Unlock()
		return
	}
	r.completed = true
	r.finalErr = err
	responseStarted := r.responseStart
	r.mu.Unlock()

	if err != nil {
		_ = r.bodyWriter.CloseWithError(err)
	} else {
		_ = r.bodyWriter.Close()
	}
	if !responseStarted {
		r.startCh <- responseStartResult{err: err}
	}
	r.doneCh <- err
}

func (r *pendingRequest) cancel(reason string, err error) {
	r.mu.Lock()
	if r.completed || r.cancelSent {
		r.mu.Unlock()
		return
	}
	r.cancelSent = true
	r.mu.Unlock()

	cancelMessage, encodeErr := api.NewMessage(api.MessageTypeRequestCancel, api.RequestCancelPayload{Reason: reason})
	if encodeErr == nil {
		cancelMessage.ID = r.id
		if sendErr := r.peer.send(cancelMessage); sendErr != nil {
			r.logger.Warn("failed to send request cancel",
				"tunnel_id", r.sessionID,
				"request_id", r.id,
				"error", sendErr,
			)
		}
	}

	r.finish(err)
}

type protocolPeer struct {
	session api.Session
	conn    net.Conn
	codec   *api.MessageCodec
	logger  *slog.Logger
	manager *session.Manager

	nextRequestID atomic.Uint64

	sendMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]*pendingRequest
}

func newProtocolPeer(session api.Session, conn net.Conn, codec *api.MessageCodec, logger *slog.Logger, manager *session.Manager) *protocolPeer {
	return &protocolPeer{
		session: session,
		conn:    conn,
		codec:   codec,
		logger:  logger,
		manager: manager,
		pending: make(map[string]*pendingRequest),
	}
}

func (p *protocolPeer) forwardHTTP(ctx context.Context, request *http.Request) (*pendingRequest, error) {
	start := requestStartFromHTTP(request)
	requestID := p.newRequestID()
	pending := newPendingRequest(requestID, p)

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

	if err := p.send(startMessage); err != nil {
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

	go p.streamRequestBody(ctx, pending, request.Body)
	go p.watchRequestContext(ctx, pending)

	return pending, nil
}

func (p *protocolPeer) streamRequestBody(ctx context.Context, pending *pendingRequest, body io.ReadCloser) {
	defer body.Close()

	buf := make([]byte, api.DefaultBodyChunkSize)
	chunks := 0

	for {
		if err := ctx.Err(); err != nil {
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
			if sendErr := p.send(message); sendErr != nil {
				pending.cancel("request body send failed", sendErr)
				p.removePending(pending.id)
				return
			}
			chunks++
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
			if sendErr := p.send(endMessage); sendErr != nil {
				pending.cancel("request end send failed", sendErr)
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

func (p *protocolPeer) watchRequestContext(ctx context.Context, pending *pendingRequest) {
	<-ctx.Done()
	p.logger.Info("request context canceled, propagating cancel",
		"tunnel_id", p.session.ID,
		"request_id", pending.id,
		"error", ctx.Err(),
	)
	pending.cancel("server request canceled", ctx.Err())
	p.removePending(pending.id)
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

func (p *protocolPeer) send(message api.Message) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()

	if err := p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return p.codec.Send(message)
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
	delete(p.pending, id)
	remaining := len(p.pending)
	p.pendingMu.Unlock()
	p.manager.SetInFlight(p.session.ID, remaining)
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
