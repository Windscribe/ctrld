package ctrld

import (
	"context"
	"sync"
	"sync/atomic"
	"tailscale.com/types/logger"
	"tailscale.com/util/eventbus"
	"time"

	"tailscale.com/net/netmon"

	ctrldnet "github.com/Control-D-Inc/ctrld/internal/net"
)

var (
	hasIPv6Once   sync.Once
	ipv6Available atomic.Bool
)

// HasIPv6 reports whether the current network stack has IPv6 available.
func HasIPv6() bool {
	hasIPv6Once.Do(func() {
		ProxyLogger.Load().Debug().Msg("checking for IPv6 availability once")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		val := ctrldnet.IPv6Available(ctx)
		ipv6Available.Store(val)
		ProxyLogger.Load().Debug().Msgf("ipv6 availability: %v", val)
		bus := eventbus.New()
		logf := logger.FromContext(ctx)
		mon, err := netmon.New(bus, logf)
		if err != nil {
			ProxyLogger.Load().Debug().Err(err).Msg("failed to monitor IPv6 state")
			return
		}
		mon.RegisterChangeCallback(func(delta *netmon.ChangeDelta) {
			old := ipv6Available.Load()
			cur := delta.Monitor.InterfaceState().HaveV6
			if old != cur {
				ProxyLogger.Load().Warn().Msgf("ipv6 availability changed, old: %v, new: %v", old, cur)
			} else {
				ProxyLogger.Load().Debug().Msg("ipv6 availability does not changed")
			}
			ipv6Available.Store(cur)
		})
		mon.Start()
	})
	return ipv6Available.Load()
}

// DisableIPv6 marks IPv6 as unavailable if enabled.
func DisableIPv6() {
	if ipv6Available.CompareAndSwap(true, false) {
		ProxyLogger.Load().Debug().Msg("turned off IPv6 availability")
	}
}
