package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/sardorazimov/binboi-go/pkg/api"
)

type bufferedConnReader struct {
	reader *bufio.Reader
	conn   net.Conn
}

func (r *bufferedConnReader) Read(p []byte) (int, error) {
	if r.reader != nil && r.reader.Buffered() > 0 {
		return r.reader.Read(p)
	}
	return r.conn.Read(p)
}

func (s *Server) handleUpgradeTunnelRequest(w http.ResponseWriter, r *http.Request, host string) {
	pending, err := s.protocolServer.ForwardRequest(r.Context(), host, r)
	if err != nil {
		s.writeTunnelForwardError(w, r, host, err)
		return
	}

	start, ok := s.awaitForwardResponseStart(w, r, host, pending)
	if !ok {
		return
	}

	if !api.IsUpgradeResponse(start) {
		s.streamForwardedHTTPResponse(w, r, host, pending, start)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		pending.cancel("response writer does not support hijack", http.ErrNotSupported)
		pending.peer.removePending(pending.id)
		writeError(w, http.StatusInternalServerError, "upgrade is not supported by this server")
		return
	}

	conn, rw, err := hijacker.Hijack()
	if err != nil {
		pending.cancel("failed to hijack upgraded connection", err)
		pending.peer.removePending(pending.id)
		s.logger.Warn("failed to hijack upgraded downstream connection",
			"host", host,
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"error", err,
		)
		return
	}
	defer conn.Close()

	if err := writeUpgradeResponse(rw, start); err != nil {
		pending.cancel("failed to write upgrade response", err)
		pending.peer.removePending(pending.id)
		s.logger.Warn("failed to send upgraded response headers",
			"host", host,
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"error", err,
		)
		return
	}

	s.logger.Info("upgrade accepted for tunneled request",
		"host", host,
		"method", r.Method,
		"path", r.URL.RequestURI(),
		"request_id", pending.id,
		"upgrade_type", start.UpgradeType,
	)

	go pending.peer.streamUpgradeData(pending, &bufferedConnReader{
		reader: rw.Reader,
		conn:   conn,
	})

	if _, err := io.Copy(conn, pending.bodyReader); err != nil && !isClosedNetworkError(err) && !errors.Is(err, context.Canceled) {
		pending.cancel("failed to write upgraded response stream", err)
		pending.peer.removePending(pending.id)
		s.logger.Warn("upgraded response stream copy failed",
			"host", host,
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"request_id", pending.id,
			"error", err,
		)
	}

	if err := pending.waitForDone(); err != nil && !errors.Is(err, context.Canceled) && !isClosedNetworkError(err) {
		s.logger.Warn("upgraded stream completed with error",
			"host", host,
			"method", r.Method,
			"path", r.URL.RequestURI(),
			"request_id", pending.id,
			"error", err,
		)
	}
}

func writeUpgradeResponse(rw *bufio.ReadWriter, start api.ResponseStartPayload) error {
	if rw == nil {
		return fmt.Errorf("nil upgraded response writer")
	}

	statusText := http.StatusText(start.Status)
	if statusText == "" {
		statusText = "Switching Protocols"
	}

	if _, err := fmt.Fprintf(rw, "HTTP/1.1 %d %s\r\n", start.Status, statusText); err != nil {
		return err
	}
	for key, values := range start.Headers {
		for _, value := range values {
			if _, err := fmt.Fprintf(rw, "%s: %s\r\n", key, value); err != nil {
				return err
			}
		}
	}
	if _, err := io.WriteString(rw, "\r\n"); err != nil {
		return err
	}
	return rw.Flush()
}

func isClosedNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "closed network connection")
}
