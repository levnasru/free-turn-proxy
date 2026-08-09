//go:build !unix

package mobile

import "fmt"

// SendFD - SCM_RIGHTS требует Unix-сокетов; -protect-path не имеет смысла вне
// Android/Unix-хостов (это мост к VpnService.protect), поэтому здесь просто
// ошибка вместо молчаливого no-op - если флаг всё же передан, вызывающий узнает.
func SendFD(protectPath string, fd int) error {
	return fmt.Errorf("protect: SCM_RIGHTS unsupported on this platform")
}
