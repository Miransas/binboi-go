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

type forwardResult struct {
	response api.ResponsePayload
	err      error
}

type protocolPeer struct {
	session api.Session
	conn    net.Conn
	codec   *api.MessageCodec
	logger  *slog.Logger
	manager *session.Manager

	nextRequestID atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]chan forwardResult
}

func newProtocolPeer(session api.Session, conn net.Conn, codec *api.MessageCodec, logger *slog.Logger, manager *session.Manager) *protocolPeer {
	return &protocolPeer{
		session: session,
		conn:    conn,
		codec:   codec,
		logger:  logger,
		manager: manager,
		pending: make(map[string]chan forwardResult),
	}
}

func (p *protocolPeer) forward(ctx context.Context, request api.RequestPayload) (api.ResponsePayload, error) {
	requestID := p.newRequestID()
	resultCh := make(chan forwardResult, 1)

	p.pendingMu.Lock()
	p.pending[requestID] = resultCh
	inFlight := len(p.pending)
	p.pendingMu.Unlock()
	p.manager.SetInFlight(p.session.ID, inFlight)

	message, err := api.NewMessage(api.MessageTypeRequest, request)
	if err != nil {
		p.removePending(requestID)
		return api.ResponsePayload{}, err
	}
	message.ID = requestID

	if err := p.send(message); err != nil {
		p.removePending(requestID)
		return api.ResponsePayload{}, err
	}

	p.logger.Info("forwarded request to tunnel client",
		"tunnel_id", p.session.ID,
		"request_id", requestID,
		"method", request.Method,
		"path", request.Path,
		"in_flight", inFlight,
	)

	waitCtx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()

	select {
	case result := <-resultCh:
		return result.response, result.err
	case <-waitCtx.Done():
		p.removePending(requestID)
		p.logger.Warn("request timed out waiting for tunnel response",
			"tunnel_id", p.session.ID,
			"request_id", requestID,
			"error", waitCtx.Err(),
		)
		return api.ResponsePayload{}, waitCtx.Err()
	}
}

func (p *protocolPeer) handleResponse(id string, response api.ResponsePayload) bool {
	resultCh, ok := p.takePending(id)
	if !ok {
		return false
	}
	resultCh <- forwardResult{response: response}
	return true
}

func (p *protocolPeer) handleRequestError(id string, protocolErr api.ProtocolErrorPayload) bool {
	resultCh, ok := p.takePending(id)
	if !ok {
		return false
	}
	resultCh <- forwardResult{err: fmt.Errorf("%s (%s)", protocolErr.Message, protocolErr.Code)}
	return true
}

func (p *protocolPeer) failPending(err error) {
	p.pendingMu.Lock()
	pending := p.pending
	p.pending = make(map[string]chan forwardResult)
	p.pendingMu.Unlock()
	p.manager.SetInFlight(p.session.ID, 0)

	for _, resultCh := range pending {
		resultCh <- forwardResult{err: err}
	}
}

func (p *protocolPeer) send(message api.Message) error {
	if err := p.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return p.codec.Send(message)
}

func (p *protocolPeer) newRequestID() string {
	sequence := p.nextRequestID.Add(1)
	return fmt.Sprintf("%s-%06d", p.session.ID, sequence)
}

func (p *protocolPeer) takePending(id string) (chan forwardResult, bool) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()

	resultCh, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.manager.SetInFlight(p.session.ID, len(p.pending))
	return resultCh, ok
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

func requestFromHTTP(r *http.Request) (api.RequestPayload, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return api.RequestPayload{}, err
	}

	return api.RequestPayload{
		Method:  r.Method,
		Path:    r.URL.RequestURI(),
		Host:    normalizeHost(r.Host),
		Headers: r.Header.Clone(),
		Body:    string(body),
	}, nil
}
