package tunnel

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	adapters "github.com/whojave/clash/adapters/inbound"
	C "github.com/whojave/clash/constant"

	"github.com/whojave/clash/common/pool"
)

func (t *Tunnel) handleHTTP(request *adapters.HTTPAdapter, outbound net.Conn) {
	req := request.R
	host := req.Host

	inboundReeder := bufio.NewReader(request)
	outboundReeder := bufio.NewReader(outbound)

	for {
		keepAlive := strings.TrimSpace(strings.ToLower(req.Header.Get("Proxy-Connection"))) == "keep-alive"

		req.Header.Set("Connection", "close")
		req.RequestURI = ""
		adapters.RemoveHopByHopHeaders(req.Header)
		err := req.Write(outbound)
		if err != nil {
			break
		}

	handleResponse:
		resp, err := http.ReadResponse(outboundReeder, req)
		if err != nil {
			break
		}
		adapters.RemoveHopByHopHeaders(resp.Header)

		if resp.StatusCode == http.StatusContinue {
			err = resp.Write(request)
			if err != nil {
				break
			}
			goto handleResponse
		}

		if keepAlive || resp.ContentLength >= 0 {
			resp.Header.Set("Proxy-Connection", "keep-alive")
			resp.Header.Set("Connection", "keep-alive")
			resp.Header.Set("Keep-Alive", "timeout=4")
			resp.Close = false
		} else {
			resp.Close = true
		}
		err = resp.Write(request)
		if err != nil || resp.Close {
			break
		}

		// even if resp.Write write body to the connection, but some http request have to Copy to close it
		buf := pool.BufPool.Get().([]byte)
		_, err = io.CopyBuffer(request, resp.Body, buf)
		pool.BufPool.Put(buf[:cap(buf)])
		if err != nil && err != io.EOF {
			break
		}

		req, err = http.ReadRequest(inboundReeder)
		if err != nil {
			break
		}

		// Sometimes firefox just open a socket to process multiple domains in HTTP
		// The temporary solution is close connection when encountering different HOST
		if req.Host != host {
			break
		}
	}
}

func (t *Tunnel) handleUDPToRemote(packet C.UDPPacket, pc net.PacketConn, addr net.Addr) {
	if _, err := pc.WriteTo(packet.Data(), addr); err != nil {
		return
	}
	DefaultManager.Upload() <- int64(len(packet.Data()))
}

func (t *Tunnel) handleUDPToLocal(packet C.UDPPacket, pc net.PacketConn, key string, omitSrcAddr bool, timeout time.Duration) {
	buf := pool.BufPool.Get().([]byte)
	defer pool.BufPool.Put(buf[:cap(buf)])
	defer t.natTable.Delete(key)
	defer pc.Close()

	for {
		pc.SetReadDeadline(time.Now().Add(timeout))
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if from != nil && omitSrcAddr {
			from = nil
		}

		n, err = packet.WriteBack(buf[:n], from)
		if err != nil {
			return
		}
		DefaultManager.Download() <- int64(n)
	}
}

func (t *Tunnel) handleSocket(request *adapters.SocketAdapter, outbound net.Conn) {
	relay(request, outbound)
}

// relay copies between left and right bidirectionally.
func relay(leftConn, rightConn net.Conn) {
	ch := make(chan error)

	go func() {
		buf := pool.BufPool.Get().([]byte)
		_, err := io.CopyBuffer(leftConn, rightConn, buf)
		pool.BufPool.Put(buf[:cap(buf)])
		leftConn.SetReadDeadline(time.Now())
		ch <- err
	}()

	buf := pool.BufPool.Get().([]byte)
	io.CopyBuffer(rightConn, leftConn, buf)
	pool.BufPool.Put(buf[:cap(buf)])
	rightConn.SetReadDeadline(time.Now())
	<-ch
}
