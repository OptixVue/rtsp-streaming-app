package core

import (
	"context"
	"crypto/tls"
	_ "embed"
	"fmt"
	"log"
	"net"
	"net/http"
	gopath "path"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pion/ice/v2"
	"github.com/pion/webrtc/v3"

	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/websocket"
)

//go:embed webrtc_index.html
var webrtcIndex []byte

type webRTCServerAPIConnsListItem struct {
	Created                   time.Time `json:"created"`
	RemoteAddr                string    `json:"remoteAddr"`
	PeerConnectionEstablished bool      `json:"peerConnectionEstablished"`
	LocalCandidate            string    `json:"localCandidate"`
	RemoteCandidate           string    `json:"remoteCandidate"`
	BytesReceived             uint64    `json:"bytesReceived"`
	BytesSent                 uint64    `json:"bytesSent"`
}

type webRTCServerAPIConnsListData struct {
	Items map[string]webRTCServerAPIConnsListItem `json:"items"`
}

type webRTCServerAPIConnsListRes struct {
	data *webRTCServerAPIConnsListData
	err  error
}

type webRTCServerAPIConnsListReq struct {
	res chan webRTCServerAPIConnsListRes
}

type webRTCServerAPIConnsKickRes struct {
	err error
}

type webRTCServerAPIConnsKickReq struct {
	id  string
	res chan webRTCServerAPIConnsKickRes
}

type webRTCConnNewReq struct {
	pathName string
	wsconn   *websocket.ServerConn
	res      chan *webRTCConn
}

type webRTCServerParent interface {
	Log(logger.Level, string, ...interface{})
}

type webRTCServer struct {
	externalAuthenticationURL string
	allowOrigin               string
	trustedProxies            conf.IPsOrCIDRs
	iceServers                []string
	readBufferCount           int
	pathManager               *pathManager
	metrics                   *metrics
	parent                    webRTCServerParent

	ctx               context.Context
	ctxCancel         func()
	ln                net.Listener
	udpMuxLn          net.PacketConn
	tcpMuxLn          net.Listener
	tlsConfig         *tls.Config
	conns             map[*webRTCConn]struct{}
	iceHostNAT1To1IPs []string
	iceUDPMux         ice.UDPMux
	iceTCPMux         ice.TCPMux

	// in
	connNew        chan webRTCConnNewReq
	chConnClose    chan *webRTCConn
	chAPIConnsList chan webRTCServerAPIConnsListReq
	chAPIConnsKick chan webRTCServerAPIConnsKickReq

	// out
	done chan struct{}
}

func newWebRTCServer(
	parentCtx context.Context,
	externalAuthenticationURL string,
	address string,
	encryption bool,
	serverKey string,
	serverCert string,
	allowOrigin string,
	trustedProxies conf.IPsOrCIDRs,
	iceServers []string,
	readBufferCount int,
	pathManager *pathManager,
	metrics *metrics,
	parent webRTCServerParent,
	iceHostNAT1To1IPs []string,
	iceUDPMuxAddress string,
	iceTCPMuxAddress string,
) (*webRTCServer, error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	var tlsConfig *tls.Config
	if encryption {
		crt, err := tls.LoadX509KeyPair(serverCert, serverKey)
		if err != nil {
			ln.Close()
			return nil, err
		}

		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{crt},
		}
	}

	var iceUDPMux ice.UDPMux
	var udpMuxLn net.PacketConn
	if iceUDPMuxAddress != "" {
		udpMuxLn, err = net.ListenPacket("udp", iceUDPMuxAddress)
		if err != nil {
			return nil, err
		}
		iceUDPMux = webrtc.NewICEUDPMux(nil, udpMuxLn)
	}

	var iceTCPMux ice.TCPMux
	var tcpMuxLn net.Listener
	if iceTCPMuxAddress != "" {
		tcpMuxLn, err = net.Listen("tcp", iceTCPMuxAddress)
		if err != nil {
			return nil, err
		}
		iceTCPMux = webrtc.NewICETCPMux(nil, tcpMuxLn, 8)
	}

	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &webRTCServer{
		externalAuthenticationURL: externalAuthenticationURL,
		allowOrigin:               allowOrigin,
		trustedProxies:            trustedProxies,
		iceServers:                iceServers,
		readBufferCount:           readBufferCount,
		pathManager:               pathManager,
		metrics:                   metrics,
		parent:                    parent,
		ctx:                       ctx,
		ctxCancel:                 ctxCancel,
		ln:                        ln,
		udpMuxLn:                  udpMuxLn,
		tcpMuxLn:                  tcpMuxLn,
		tlsConfig:                 tlsConfig,
		iceUDPMux:                 iceUDPMux,
		iceTCPMux:                 iceTCPMux,
		iceHostNAT1To1IPs:         iceHostNAT1To1IPs,
		conns:                     make(map[*webRTCConn]struct{}),
		connNew:                   make(chan webRTCConnNewReq),
		chConnClose:               make(chan *webRTCConn),
		chAPIConnsList:            make(chan webRTCServerAPIConnsListReq),
		chAPIConnsKick:            make(chan webRTCServerAPIConnsKickReq),
		done:                      make(chan struct{}),
	}

	str := "listener opened on " + address + " (HTTP)"
	if udpMuxLn != nil {
		str += ", " + iceUDPMuxAddress + " (ICE/UDP)"
	}
	if tcpMuxLn != nil {
		str += ", " + iceTCPMuxAddress + " (ICE/TCP)"
	}
	s.log(logger.Info, str)

	if s.metrics != nil {
		s.metrics.webRTCServerSet(s)
	}

	go s.run()

	return s, nil
}

// Log is the main logging function.
func (s *webRTCServer) log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[WebRTC] "+format, append([]interface{}{}, args...)...)
}

func (s *webRTCServer) close() {
	s.log(logger.Info, "listener is closing")
	s.ctxCancel()
	<-s.done
}

func (s *webRTCServer) run() {
	defer close(s.done)

	rp := newHTTPRequestPool()
	defer rp.close()

	router := gin.New()
	router.NoRoute(rp.mw, httpLoggerMiddleware(s), s.onRequest)

	tmp := make([]string, len(s.trustedProxies))
	for i, entry := range s.trustedProxies {
		tmp[i] = entry.String()
	}
	router.SetTrustedProxies(tmp)

	hs := &http.Server{
		Handler:   router,
		TLSConfig: s.tlsConfig,
		ErrorLog:  log.New(&nilWriter{}, "", 0),
	}

	if s.tlsConfig != nil {
		go hs.ServeTLS(s.ln, "", "")
	} else {
		go hs.Serve(s.ln)
	}

	var wg sync.WaitGroup

outer:
	for {
		select {
		case req := <-s.connNew:
			c := newWebRTCConn(
				s.ctx,
				s.readBufferCount,
				req.pathName,
				req.wsconn,
				s.iceServers,
				&wg,
				s.pathManager,
				s,
				s.iceHostNAT1To1IPs,
				s.iceUDPMux,
				s.iceTCPMux,
			)
			s.conns[c] = struct{}{}
			req.res <- c

		case conn := <-s.chConnClose:
			delete(s.conns, conn)

		case req := <-s.chAPIConnsList:
			data := &webRTCServerAPIConnsListData{
				Items: make(map[string]webRTCServerAPIConnsListItem),
			}

			for c := range s.conns {
				data.Items[c.uuid.String()] = webRTCServerAPIConnsListItem{
					Created:                   c.created,
					RemoteAddr:                c.remoteAddr().String(),
					PeerConnectionEstablished: c.peerConnectionEstablished(),
					LocalCandidate:            c.localCandidate(),
					RemoteCandidate:           c.remoteCandidate(),
					BytesReceived:             c.bytesReceived(),
					BytesSent:                 c.bytesSent(),
				}
			}

			req.res <- webRTCServerAPIConnsListRes{data: data}

		case req := <-s.chAPIConnsKick:
			res := func() bool {
				for c := range s.conns {
					if c.uuid.String() == req.id {
						delete(s.conns, c)
						c.close()
						return true
					}
				}
				return false
			}()
			if res {
				req.res <- webRTCServerAPIConnsKickRes{}
			} else {
				req.res <- webRTCServerAPIConnsKickRes{fmt.Errorf("not found")}
			}

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()

	hs.Shutdown(context.Background())
	s.ln.Close() // in case Shutdown() is called before Serve()

	wg.Wait()

	if s.udpMuxLn != nil {
		s.udpMuxLn.Close()
	}

	if s.tcpMuxLn != nil {
		s.tcpMuxLn.Close()
	}
}

func (s *webRTCServer) onRequest(ctx *gin.Context) {
	ctx.Writer.Header().Set("Access-Control-Allow-Origin", s.allowOrigin)
	ctx.Writer.Header().Set("Access-Control-Allow-Credentials", "true")

	switch ctx.Request.Method {
	case http.MethodGet:

	case http.MethodOptions:
		ctx.Writer.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		ctx.Writer.Header().Set("Access-Control-Allow-Headers", ctx.Request.Header.Get("Access-Control-Request-Headers"))
		ctx.Writer.WriteHeader(http.StatusOK)
		return

	default:
		return
	}

	// remove leading prefix
	pa := ctx.Request.URL.Path[1:]

	switch pa {
	case "", "favicon.ico":
		return
	}

	dir, fname := func() (string, string) {
		if strings.HasSuffix(pa, "/ws") {
			return gopath.Dir(pa), gopath.Base(pa)
		}
		return pa, ""
	}()

	if fname == "" && !strings.HasSuffix(dir, "/") {
		ctx.Writer.Header().Set("Location", "/"+dir+"/")
		ctx.Writer.WriteHeader(http.StatusMovedPermanently)
		return
	}

	dir = strings.TrimSuffix(dir, "/")

	res := s.pathManager.describe(pathDescribeReq{
		pathName: dir,
	})
	if res.err != nil {
		ctx.Writer.WriteHeader(http.StatusNotFound)
		return
	}

	err := s.authenticate(res.path, ctx)
	if err != nil {
		if terr, ok := err.(pathErrAuthCritical); ok {
			s.log(logger.Info, "authentication error: %s", terr.message)
			ctx.Writer.Header().Set("WWW-Authenticate", `Basic realm="rtsp-simple-server"`)
			ctx.Writer.WriteHeader(http.StatusUnauthorized)
			return
		}

		ctx.Writer.Header().Set("WWW-Authenticate", `Basic realm="rtsp-simple-server"`)
		ctx.Writer.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch fname {
	case "":
		ctx.Writer.Header().Set("Content-Type", "text/html")
		ctx.Writer.WriteHeader(http.StatusOK)
		ctx.Writer.Write(webrtcIndex)
		return

	case "ws":
		wsconn, err := websocket.NewServerConn(ctx.Writer, ctx.Request)
		if err != nil {
			return
		}
		defer wsconn.Close()

		c := s.newConn(dir, wsconn)
		if c == nil {
			return
		}

		c.wait()
	}
}

func (s *webRTCServer) newConn(dir string, wsconn *websocket.ServerConn) *webRTCConn {
	req := webRTCConnNewReq{
		pathName: dir,
		wsconn:   wsconn,
		res:      make(chan *webRTCConn),
	}

	select {
	case s.connNew <- req:
		return <-req.res
	case <-s.ctx.Done():
		return nil
	}
}

func (s *webRTCServer) authenticate(pa *path, ctx *gin.Context) error {
	pathConf := pa.safeConf()
	pathIPs := pathConf.ReadIPs
	pathUser := pathConf.ReadUser
	pathPass := pathConf.ReadPass

	if s.externalAuthenticationURL != "" {
		ip := net.ParseIP(ctx.ClientIP())
		user, pass, ok := ctx.Request.BasicAuth()

		err := externalAuth(
			s.externalAuthenticationURL,
			ip.String(),
			user,
			pass,
			pa.name,
			externalAuthProtoWebRTC,
			nil,
			false,
			ctx.Request.URL.RawQuery)
		if err != nil {
			if !ok {
				return pathErrAuthNotCritical{}
			}

			return pathErrAuthCritical{
				message: fmt.Sprintf("external authentication failed: %s", err),
			}
		}
	}

	if pathIPs != nil {
		ip := net.ParseIP(ctx.ClientIP())

		if !ipEqualOrInRange(ip, pathIPs) {
			return pathErrAuthCritical{
				message: fmt.Sprintf("IP '%s' not allowed", ip),
			}
		}
	}

	if pathUser != "" {
		user, pass, ok := ctx.Request.BasicAuth()
		if !ok {
			return pathErrAuthNotCritical{}
		}

		if user != string(pathUser) || pass != string(pathPass) {
			return pathErrAuthCritical{
				message: "invalid credentials",
			}
		}
	}

	return nil
}

// connClose is called by webRTCConn.
func (s *webRTCServer) connClose(c *webRTCConn) {
	select {
	case s.chConnClose <- c:
	case <-s.ctx.Done():
	}
}

// apiConnsList is called by api.
func (s *webRTCServer) apiConnsList() webRTCServerAPIConnsListRes {
	req := webRTCServerAPIConnsListReq{
		res: make(chan webRTCServerAPIConnsListRes),
	}

	select {
	case s.chAPIConnsList <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCServerAPIConnsListRes{err: fmt.Errorf("terminated")}
	}
}

// apiConnsKick is called by api.
func (s *webRTCServer) apiConnsKick(id string) webRTCServerAPIConnsKickRes {
	req := webRTCServerAPIConnsKickReq{
		id:  id,
		res: make(chan webRTCServerAPIConnsKickRes),
	}

	select {
	case s.chAPIConnsKick <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCServerAPIConnsKickRes{err: fmt.Errorf("terminated")}
	}
}
