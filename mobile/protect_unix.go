//go:build unix

package mobile

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// SendFD connects to the Unix socket at protectPath and sends fd via SCM_RIGHTS.
func SendFD(protectPath string, fd int) error {
	addr, err := net.ResolveUnixAddr("unix", protectPath)
	if err != nil {
		return err
	}
	var conn *net.UnixConn
	for attempt := 0; attempt < 5; attempt++ {
		conn, err = net.DialUnix("unix", nil, addr)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	defer conn.Close()

	sysconn, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var opErr error
	err = sysconn.Control(func(ctrlFd uintptr) {
		rights := syscall.UnixRights(fd)
		// Send a dummy byte along with the FD
		err = syscall.Sendmsg(int(ctrlFd), []byte("p"), rights, nil, 0)
		if err != nil {
			opErr = err
		}
	})
	if err != nil {
		return err
	}
	if opErr != nil {
		return opErr
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	ackBuf := make([]byte, 1)
	if _, err := conn.Read(ackBuf); err != nil {
		return err
	}
	if ackBuf[0] != 1 {
		return errors.New("protect daemon returned negative ack")
	}
	return nil
}
