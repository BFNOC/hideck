package netstack

import (
	"errors"
	"net"
	"os"
	"syscall"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

// gonet flattens endpoint errors into strings, while its own timeoutError
// represents a local read/write deadline. Restore that distinction only at
// this adapter boundary, rather than guessing from arbitrary IMS errors.
type imsTCPConn struct {
	*gonet.TCPConn
	dev *devConnTrace
}

// newIMSTCPConn wraps a gonet connection; origin ("dial" or "accept") goes
// into the [dev] trace.
func newIMSTCPConn(conn *gonet.TCPConn, origin string) *imsTCPConn {
	return &imsTCPConn{TCPConn: conn, dev: newDevConnTrace("tcp", origin, conn.LocalAddr(), conn.RemoteAddr())}
}

func (c *imsTCPConn) Read(buffer []byte) (int, error) {
	n, err := c.TCPConn.Read(buffer)
	err = normalizeGonetTCPError(err)
	c.dev.read(buffer, n, err)
	return n, err
}

func (c *imsTCPConn) Write(buffer []byte) (int, error) {
	n, err := c.TCPConn.Write(buffer)
	c.dev.wrote(buffer, n)
	return n, normalizeGonetTCPError(err)
}

func (c *imsTCPConn) Close() error {
	err := c.TCPConn.Close()
	c.dev.close(err)
	return err
}

func normalizeGonetTCPError(err error) error {
	var operation *net.OpError
	if !errors.As(err, &operation) || operation.Err == nil {
		return err
	}
	var cause error
	switch operation.Err.Error() {
	case (&tcpip.ErrTimeout{}).String():
		cause = syscall.ETIMEDOUT
	case (&tcpip.ErrConnectionReset{}).String():
		cause = syscall.ECONNRESET
	default:
		var deadline net.Error
		if errors.As(operation.Err, &deadline) && deadline.Timeout() {
			cause = os.ErrDeadlineExceeded
		}
	}
	if cause == nil {
		return err
	}
	normalized := *operation
	normalized.Err = cause
	return &normalized
}
