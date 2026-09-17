//go:build unix

package mobile

import (
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
	return opErr
}
