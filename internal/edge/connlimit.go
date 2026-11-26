package edge

import (
	"net"
	"net/netip"
	"sync"
)

const (
	defaultMaxConnsGlobal = 4096
	defaultMaxConnsPerIP  = 64
)

// connLimitListener wraps a net.Listener to cap both total concurrent connections
// and concurrent connections per remote IP, defending against connection-exhaustion DoS
// while leaving ample headroom for HTTP/2, poster grids, and household NATs.
type connLimitListener struct {
	net.Listener
	maxGlobal int
	maxPerIP  int

	mu     sync.Mutex
	total  int
	perIP  map[string]int
}

func newConnLimitListener(inner net.Listener, maxGlobal, maxPerIP int) *connLimitListener {
	if maxGlobal <= 0 {
		maxGlobal = defaultMaxConnsGlobal
	}
	if maxPerIP <= 0 {
		maxPerIP = defaultMaxConnsPerIP
	}
	return &connLimitListener{
		Listener:  inner,
		maxGlobal: maxGlobal,
		maxPerIP:  maxPerIP,
		perIP:     make(map[string]int),
	}
}

func (l *connLimitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := extractRemoteIP(c.RemoteAddr())
		if !l.acquire(ip) {
			_ = c.Close()
			continue
		}
		return &trackedConn{
			Conn:     c,
			listener: l,
			ip:       ip,
		}, nil
	}
}

func (l *connLimitListener) acquire(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxGlobal {
		return false
	}
	if ip != "" && l.perIP[ip] >= l.maxPerIP {
		return false
	}
	l.total++
	if ip != "" {
		l.perIP[ip]++
	}
	return true
}

func (l *connLimitListener) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total > 0 {
		l.total--
	}
	if ip != "" {
		if n := l.perIP[ip]; n <= 1 {
			delete(l.perIP, ip)
		} else {
			l.perIP[ip] = n - 1
		}
	}
}

type trackedConn struct {
	net.Conn
	listener *connLimitListener
	ip       string
	once     sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.listener.release(c.ip)
	})
	return err
}

func extractRemoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	if parsed, err := netip.ParseAddr(host); err == nil {
		return parsed.Unmap().String()
	}
	return host
}
