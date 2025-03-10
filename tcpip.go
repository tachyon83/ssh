package ssh

import (
	"errors"
	"io"
	"net"
	"strconv"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

const (
	forwardedTCPChannelType = "forwarded-tcpip"
)

// direct-tcpip data struct as specified in RFC4254, Section 7.2
type localForwardChannelData struct {
	DestAddr string
	DestPort uint32

	OriginAddr string
	OriginPort uint32
}

// DirectTCPIPHandler can be enabled by adding it to the server's
// ChannelHandlers under direct-tcpip.
func DirectTCPIPHandler(srv *Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx Context) {
	d := localForwardChannelData{}
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		newChan.Reject(gossh.ConnectionFailed, "error parsing forward data: "+err.Error())
		return
	}

	if srv.LocalPortForwardingCallback == nil || !srv.LocalPortForwardingCallback(ctx, d.DestAddr, d.DestPort) {
		newChan.Reject(gossh.Prohibited, "port forwarding is disabled")
		return
	}

	dest := net.JoinHostPort(d.DestAddr, strconv.FormatInt(int64(d.DestPort), 10))

	var dialer net.Dialer
	dconn, err := dialer.DialContext(ctx, "tcp", dest)
	if err != nil {
		newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}

	ch, reqs, err := newChan.Accept()
	if err != nil {
		dconn.Close()
		return
	}
	go gossh.DiscardRequests(reqs)

	go func() {
		defer ch.Close()
		defer dconn.Close()
		io.Copy(ch, dconn)
	}()
	go func() {
		defer ch.Close()
		defer dconn.Close()
		io.Copy(dconn, ch)
	}()
}

type remoteForwardRequest struct {
	BindAddr string
	BindPort uint32
}

type remoteForwardSuccess struct {
	BindPort uint32
}

type remoteForwardCancelRequest struct {
	BindAddr string
	BindPort uint32
}

type remoteForwardChannelData struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

// ForwardedTCPHandler can be enabled by creating a ForwardedTCPHandler and
// adding the HandleSSHRequest callback to the server's RequestHandlers under
// tcpip-forward and cancel-tcpip-forward.
type ForwardedTCPHandler struct {
	forwards map[string]net.Listener
	sync.Mutex
}

func (h *ForwardedTCPHandler) HandleSSHRequest(ctx Context, srv *Server, req *gossh.Request) (bool, []byte) {
	h.Lock()
	if h.forwards == nil {
		h.forwards = make(map[string]net.Listener)
	}
	h.Unlock()
	conn := ctx.Value(ContextKeyConn).(*gossh.ServerConn)
	switch req.Type {
	case "tcpip-forward":
		var reqPayload remoteForwardRequest
		if err := gossh.Unmarshal(req.Payload, &reqPayload); err != nil {
			srv.logMsg("failed to unmarshal %s payload s from %s - %s", req.Type, conn.RemoteAddr().String(), err.Error())
			return false, []byte{}
		}
		if srv.ReversePortForwardingCallback == nil {
			return false, []byte("port forwarding is disabled")
		}

		addrstr := net.JoinHostPort(reqPayload.BindAddr, strconv.Itoa(int(reqPayload.BindPort)))
		addr, err := net.ResolveTCPAddr("tcp", addrstr)
		if err != nil {
			return false, []byte("port forwarding disabled - invalid address requested")
		}

		if !srv.ReversePortForwardingCallback(ctx, addr, nil, 0, nil, nil) {
			return false, []byte("port forwarding is rejected")
		}

		ln, err := net.ListenTCP("tcp", addr)
		if err != nil {
			srv.logMsg("failed to listen on %s - %s", addr, err.Error())
			return false, []byte{}
		}

		srv.logMsg("port forward started on %s for %s", addr, conn.RemoteAddr().String())
		srv.ReversePortForwardingCallback(ctx, addr, nil, 1, ln, nil)
		_, destPortStr, _ := net.SplitHostPort(ln.Addr().String())
		destPort, _ := strconv.Atoi(destPortStr)
		h.Lock()
		h.forwards[addrstr] = ln
		h.Unlock()
		go func() {
			<-ctx.Done()
			h.Lock()
			ln, ok := h.forwards[addrstr]
			h.Unlock()
			if ok {
				ln.Close()
			}
		}()
		go func() {
			var lwg sync.WaitGroup
			for {
				c, err := ln.Accept()
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						srv.logMsg("failed to accept connection on %s - %s", ln.Addr().String(), err.Error())
					}
					break
				}
				originAddr, orignPortStr, _ := net.SplitHostPort(c.RemoteAddr().String())
				originPort, _ := strconv.Atoi(orignPortStr)
				payload := gossh.Marshal(&remoteForwardChannelData{
					DestAddr:   reqPayload.BindAddr,
					DestPort:   uint32(destPort),
					OriginAddr: originAddr,
					OriginPort: uint32(originPort),
				})
				lwg.Add(1)
				go func() {
					defer lwg.Done()
					ch, reqs, err := conn.OpenChannel(forwardedTCPChannelType, payload)
					if err != nil {
						srv.logMsg("failed to open channel on %s:%d for %s:%d - %s", reqPayload.BindAddr, destPort, originAddr, originPort, err.Error())
						c.Close()
						return
					}

					var claddr *net.TCPAddr
					var craddr *net.TCPAddr
					var ok bool
					claddr, ok = c.LocalAddr().(*net.TCPAddr)
					if !ok {
						srv.logMsg("invalid channel local address %v", c.LocalAddr())
						c.Close()
					}
					craddr, ok = c.RemoteAddr().(*net.TCPAddr)
					if !ok {
						srv.logMsg("invalid channel remote address %v", c.RemoteAddr())
						c.Close()
					}

					srv.ReversePortForwardingCallback(ctx, claddr, craddr, 2, ln, c)
					srv.logMsg("opened channel on %s:%d for %s:%d", reqPayload.BindAddr, destPort, originAddr, originPort)

					go gossh.DiscardRequests(reqs)

					var wg sync.WaitGroup
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer ch.Close()
						defer c.Close()
						io.Copy(ch, c)
					}()
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer ch.Close()
						defer c.Close()
						io.Copy(c, ch)
					}()
					wg.Wait()

					srv.ReversePortForwardingCallback(ctx, claddr, craddr, -2, ln, nil)
					srv.logMsg("closed channel on %s:%d for %s:%d", reqPayload.BindAddr, destPort, originAddr, originPort)
				}()
			}
			lwg.Wait()
			h.Lock()
			delete(h.forwards, addrstr)
			h.Unlock()
			srv.ReversePortForwardingCallback(ctx, addr, nil, -1, nil, nil)
			srv.logMsg("port forward ended on %s for %s", addr, conn.RemoteAddr().String())
		}()
		return true, gossh.Marshal(&remoteForwardSuccess{uint32(destPort)})

	case "cancel-tcpip-forward":
		var reqPayload remoteForwardCancelRequest
		if err := gossh.Unmarshal(req.Payload, &reqPayload); err != nil {
			srv.logMsg("failed to unmarshal %s payload from %s - %s", req.Type, conn.RemoteAddr().String(), err.Error())
			return false, []byte{}
		}
		addrstr := net.JoinHostPort(reqPayload.BindAddr, strconv.Itoa(int(reqPayload.BindPort)))
		addr, err := net.ResolveTCPAddr("tcp", addrstr)
		if err != nil {
			return false, []byte("port forwarding cancellation rejected - invalid address requested")
		}

		h.Lock()
		ln, ok := h.forwards[addrstr]
		h.Unlock()
		srv.ReversePortForwardingCallback(ctx, addr, nil, -1, nil, nil)
		if ok {
			srv.logMsg("port forward cancelled on %s for %s", addrstr, conn.RemoteAddr().String())
			ln.Close()
		}
		return true, nil
	default:
		return false, nil
	}
}
