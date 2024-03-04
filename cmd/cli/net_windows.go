package cli

import (
	"net"
)

func patchNetIfaceName(iface *net.Interface) error {
	return nil
}

// validInterface reports whether the *net.Interface is a valid one.
// On Windows, only physical interfaces are considered valid.
func validInterface(iface *net.Interface) bool {
	if iface == nil {
		return false
	}
	if isPhysicalInterface(iface.HardwareAddr.String()) {
		return true
	}
	return false
}
