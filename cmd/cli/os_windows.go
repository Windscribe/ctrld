package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"

	ctrldnet "github.com/Control-D-Inc/ctrld/internal/net"
)

const (
	v4InterfaceKeyPathFormat = `HKLM:\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters\Interfaces\`
	v6InterfaceKeyPathFormat = `HKLM:\SYSTEM\CurrentControlSet\Services\Tcpip6\Parameters\Interfaces\`
)

var (
	setDNSOnce   sync.Once
	resetDNSOnce sync.Once
)

// setDnsIgnoreUnusableInterface likes setDNS, but return a nil error if the interface is not usable.
func setDnsIgnoreUnusableInterface(iface *net.Interface, nameservers []string) error {
	return setDNS(iface, nameservers)
}

func setDnsPowershellCmd(iface *net.Interface, nameservers []string) string {
	nss := make([]string, 0, len(nameservers))
	for _, ns := range nameservers {
		nss = append(nss, strconv.Quote(ns))
	}
	return fmt.Sprintf("Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses (%s)", iface.Index, strings.Join(nss, ","))
}

// setDNS sets the dns server for the provided network interface
func setDNS(iface *net.Interface, nameservers []string) error {
	if len(nameservers) == 0 {
		return errors.New("empty DNS nameservers")
	}
	setDNSOnce.Do(func() {
		// If there's a Dns server running, that means we are on AD with Dns feature enabled.
		// Configuring the Dns server to forward queries to ctrld instead.
		if windowsHasLocalDnsServerRunning() {
			file := absHomeDir(windowsForwardersFilename)
			oldForwardersContent, _ := os.ReadFile(file)
			hasLocalIPv6Listener := needLocalIPv6Listener()
			forwarders := slices.DeleteFunc(slices.Clone(nameservers), func(s string) bool {
				if !hasLocalIPv6Listener {
					return false
				}
				return s == "::1"
			})
			if err := os.WriteFile(file, []byte(strings.Join(forwarders, ",")), 0600); err != nil {
				mainLog.Load().Warn().Err(err).Msg("could not save forwarders settings")
			}
			oldForwarders := strings.Split(string(oldForwardersContent), ",")
			if err := addDnsServerForwarders(forwarders, oldForwarders); err != nil {
				mainLog.Load().Warn().Err(err).Msg("could not set forwarders settings")
			}
		}
	})
	out, err := powershell(setDnsPowershellCmd(iface, nameservers))
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// resetDnsIgnoreUnusableInterface likes resetDNS, but return a nil error if the interface is not usable.
func resetDnsIgnoreUnusableInterface(iface *net.Interface) error {
	return resetDNS(iface)
}

// TODO(cuonglm): should we use system API?
func resetDNS(iface *net.Interface) error {
	resetDNSOnce.Do(func() {
		// See corresponding comment in setDNS.
		if windowsHasLocalDnsServerRunning() {
			file := absHomeDir(windowsForwardersFilename)
			content, err := os.ReadFile(file)
			if err != nil {
				mainLog.Load().Error().Err(err).Msg("could not read forwarders settings")
				return
			}
			nameservers := strings.Split(string(content), ",")
			if err := removeDnsServerForwarders(nameservers); err != nil {
				mainLog.Load().Error().Err(err).Msg("could not remove forwarders settings")
				return
			}
		}
	})

	// Restoring DHCP settings.
	cmd := fmt.Sprintf("Set-DnsClientServerAddress -InterfaceIndex %d -ResetServerAddresses", iface.Index)
	out, err := powershell(cmd)
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}

	// If there's static DNS saved, restoring it.
	if nss := savedStaticNameservers(iface); len(nss) > 0 {
		v4ns := make([]string, 0, 2)
		v6ns := make([]string, 0, 2)
		for _, ns := range nss {
			if ctrldnet.IsIPv6(ns) {
				v6ns = append(v6ns, ns)
			} else {
				v4ns = append(v4ns, ns)
			}
		}

		for _, ns := range [][]string{v4ns, v6ns} {
			if len(ns) == 0 {
				continue
			}
			mainLog.Load().Debug().Msgf("setting static DNS for interface %q", iface.Name)
			if err := setDNS(iface, ns); err != nil {
				return err
			}
		}
	}
	return nil
}

func currentDNS(iface *net.Interface) []string {
	luid, err := winipcfg.LUIDFromIndex(uint32(iface.Index))
	if err != nil {
		mainLog.Load().Error().Err(err).Msg("failed to get interface LUID")
		return nil
	}
	nameservers, err := luid.DNS()
	if err != nil {
		mainLog.Load().Error().Err(err).Msg("failed to get interface DNS")
		return nil
	}
	ns := make([]string, 0, len(nameservers))
	for _, nameserver := range nameservers {
		ns = append(ns, nameserver.String())
	}
	return ns
}

// currentStaticDNS returns the current static DNS settings of given interface.
func currentStaticDNS(iface *net.Interface) ([]string, error) {
	luid, err := winipcfg.LUIDFromIndex(uint32(iface.Index))
	if err != nil {
		return nil, err
	}
	guid, err := luid.GUID()
	if err != nil {
		return nil, err
	}
	var ns []string
	for _, path := range []string{v4InterfaceKeyPathFormat, v6InterfaceKeyPathFormat} {
		interfaceKeyPath := path + guid.String()
		found := false
		for _, key := range []string{"NameServer", "ProfileNameServer"} {
			if found {
				continue
			}
			cmd := fmt.Sprintf(`Get-ItemPropertyValue -Path "%s" -Name "%s"`, interfaceKeyPath, key)
			out, err := powershell(cmd)
			if err == nil && len(out) > 0 {
				found = true
				for _, e := range strings.Split(string(out), ",") {
					ns = append(ns, strings.TrimRight(e, "\x00"))
				}
			}
		}
	}
	return ns, nil
}

// addDnsServerForwarders adds given nameservers to DNS server forwarders list,
// and also removing old forwarders if provided.
func addDnsServerForwarders(nameservers, old []string) error {
	newForwardersMap := make(map[string]struct{})
	newForwarders := make([]string, len(nameservers))
	for i := range nameservers {
		newForwardersMap[nameservers[i]] = struct{}{}
		newForwarders[i] = fmt.Sprintf("%q", nameservers[i])
	}
	oldForwarders := old[:0]
	for _, fwd := range old {
		if _, ok := newForwardersMap[fwd]; !ok {
			oldForwarders = append(oldForwarders, fwd)
		}
	}
	// NOTE: It is important to add new forwarder before removing old one.
	//       Testing on Windows Server 2022 shows that removing forwarder1
	//       then adding forwarder2 sometimes ends up adding both of them
	//       to the forwarders list.
	cmd := fmt.Sprintf("Add-DnsServerForwarder -IPAddress %s", strings.Join(newForwarders, ","))
	if len(oldForwarders) > 0 {
		cmd = fmt.Sprintf("%s ; Remove-DnsServerForwarder -IPAddress %s -Force", cmd, strings.Join(oldForwarders, ","))
	}
	if out, err := powershell(cmd); err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// removeDnsServerForwarders removes given nameservers from DNS server forwarders list.
func removeDnsServerForwarders(nameservers []string) error {
	for _, ns := range nameservers {
		cmd := fmt.Sprintf("Remove-DnsServerForwarder -IPAddress %s -Force", ns)
		if out, err := powershell(cmd); err != nil {
			return fmt.Errorf("%w: %s", err, string(out))
		}
	}
	return nil
}
