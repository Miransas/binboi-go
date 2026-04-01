package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

type bufferedConnReadCloser struct {
	conn   net.Conn
	reader *bufio.Reader
}

func (r *bufferedConnReadCloser) Read(p []byte) (int, error) {
	if r.reader != nil && r.reader.Buffered() > 0 {
		return r.reader.Read(p)
	}
	return r.conn.Read(p)
}

func (r *bufferedConnReadCloser) Close() error {
	return r.conn.Close()
}

func (s *TunnelSession) executeUpgradeRequest(request *activeRequest) {
	responseStart, responseBody, rawConn, err := s.proxyUpgradeRequest(request.ctx, request)
	if err != nil {
		state := requestStateFailed
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			state = requestStateCanceled
		}
		request.finish(state)

		s.logger.Warn("local upgrade proxy request failed",
			"tunnel_id", s.registered.TunnelID,
			"request_id", request.id,
			"error", err,
		)

		if !errors.Is(err, context.Canceled) {
			_ = s.sendProtocolError(request.id, "local_upgrade_failed", err.Error())
		}
		return
	}
	defer responseBody.Close()

	if err := s.sendResponseStart(request, responseStart); err != nil {
		request.finish(requestStateFailed)
		if rawConn != nil {
			_ = rawConn.Close()
		}
		return
	}

	if !api.IsUpgradeResponse(responseStart) || rawConn == nil {
		s.streamResponseReader(request, responseStart, responseBody)
		return
	}

	request.setRawConn(rawConn)
	go s.pumpUpgradeInput(request, rawConn)
	s.streamResponseReader(request, responseStart, responseBody)
}

func (s *TunnelSession) proxyUpgradeRequest(ctx context.Context, request *activeRequest) (api.ResponseStartPayload, io.ReadCloser, net.Conn, error) {
	targetURL, err := localForwardURL(s.registered.Target, request.start.Path)
	if err != nil {
		return api.ResponseStartPayload{}, nil, nil, err
	}

	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return api.ResponseStartPayload{}, nil, nil, fmt.Errorf("parse upgrade target URL: %w", err)
	}

	address := parsedURL.Host
	if address == "" {
		return api.ResponseStartPayload{}, nil, nil, fmt.Errorf("upgrade target %q is missing a host", targetURL)
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return api.ResponseStartPayload{}, nil, nil, err
	}

	proxyReq, err := http.NewRequestWithContext(ctx, request.start.Method, targetURL, nil)
	if err != nil {
		_ = conn.Close()
		return api.ResponseStartPayload{}, nil, nil, err
	}

	proxyReq.Header = cloneHeaders(request.start.Headers)
	if request.start.Host != "" {
		proxyReq.Host = request.start.Host
	}

	if err := proxyReq.Write(conn); err != nil {
		_ = conn.Close()
		return api.ResponseStartPayload{}, nil, nil, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, proxyReq)
	if err != nil {
		_ = conn.Close()
		return api.ResponseStartPayload{}, nil, nil, err
	}

	responseStart := api.ResponseStartPayload{
		Status:      resp.StatusCode,
		Headers:     resp.Header.Clone(),
		UpgradeType: api.UpgradeTypeFromHeaders(resp.Header),
	}

	if !api.IsUpgradeResponse(responseStart) {
		return responseStart, resp.Body, nil, nil
	}

	return responseStart, &bufferedConnReadCloser{conn: conn, reader: reader}, conn, nil
}

func (s *TunnelSession) sendResponseStart(request *activeRequest, responseStart api.ResponseStartPayload) error {
	startMessage, err := api.NewMessage(api.MessageTypeResponseStart, responseStart)
	if err != nil {
		_ = s.sendProtocolError(request.id, "encode_response_start_failed", err.Error())
		return err
	}
	startMessage.ID = request.id
	if err := s.sendStream(request.ctx, request.id, startMessage, false); err != nil {
		return err
	}

	s.logger.Info("sent response_start",
		"tunnel_id", s.registered.TunnelID,
		"request_id", request.id,
		"status", responseStart.Status,
		"upgrade_type", responseStart.UpgradeType,
	)
	return nil
}

func (s *TunnelSession) streamResponseReader(request *activeRequest, responseStart api.ResponseStartPayload, responseBody io.ReadCloser) {
	buf := make([]byte, api.DefaultBodyChunkSize)
	chunkIndex := 0

	for {
		n, readErr := responseBody.Read(buf)
		if n > 0 {
			chunkMessage, err := api.NewMessage(api.MessageTypeResponseBody, api.BodyChunkPayload{
				Chunk: append([]byte(nil), buf[:n]...),
			})
			if err != nil {
				request.finish(requestStateFailed)
				_ = s.sendProtocolError(request.id, "encode_response_body_failed", err.Error())
				return
			}
			chunkMessage.ID = request.id
			if err := s.sendStream(request.ctx, request.id, chunkMessage, false); err != nil {
				request.finish(requestStateFailed)
				return
			}
			chunkIndex++
			request.markResponseBytes(n, s.flowControl.StreamIdleTimeout())
			s.logger.Debug("sent response_body chunk",
				"tunnel_id", s.registered.TunnelID,
				"request_id", request.id,
				"chunk_index", chunkIndex,
				"chunk_size", n,
				"upgrade_type", responseStart.UpgradeType,
			)
		}

		if readErr == io.EOF {
			endMessage, err := api.NewMessage(api.MessageTypeResponseEnd, api.StreamEndPayload{})
			if err != nil {
				request.finish(requestStateFailed)
				_ = s.sendProtocolError(request.id, "encode_response_end_failed", err.Error())
				return
			}
			endMessage.ID = request.id
			if err := s.sendStream(request.ctx, request.id, endMessage, true); err != nil {
				request.finish(requestStateFailed)
				return
			}
			request.finish(requestStateFinished)
			s.logger.Debug("sent response_end",
				"tunnel_id", s.registered.TunnelID,
				"request_id", request.id,
				"chunks", chunkIndex,
				"upgrade_type", responseStart.UpgradeType,
			)
			return
		}
		if readErr != nil {
			state := requestStateFailed
			if errors.Is(readErr, net.ErrClosed) || errors.Is(readErr, context.Canceled) {
				state = requestStateCanceled
			}
			request.finish(state)
			_ = s.sendProtocolError(request.id, "response_body_read_failed", readErr.Error())
			return
		}
	}
}

func (s *TunnelSession) pumpUpgradeInput(request *activeRequest, conn net.Conn) {
	defer close(request.rawDone)
	defer func() {
		_ = closeWrite(conn)
	}()

	for {
		select {
		case <-request.ctx.Done():
			return
		case chunk, ok := <-request.rawInput:
			if !ok {
				return
			}
			if len(chunk) == 0 {
				continue
			}

			if _, err := conn.Write(chunk); err != nil {
				s.logger.Warn("failed to write upgraded request stream to local upstream",
					"tunnel_id", s.registered.TunnelID,
					"request_id", request.id,
					"error", err,
				)
				request.cancel()
				_ = conn.Close()
				return
			}
		}
	}
}

func closeWrite(conn net.Conn) error {
	type closeWriter interface {
		CloseWrite() error
	}

	if conn == nil {
		return nil
	}
	if tcpConn, ok := conn.(closeWriter); ok {
		return tcpConn.CloseWrite()
	}
	return conn.Close()
}

func waitForRawDone(ch <-chan struct{}) {
	if ch == nil {
		return
	}

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
	}
}
