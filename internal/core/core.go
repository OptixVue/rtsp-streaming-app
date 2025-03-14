// Package core contains the main struct of the software.
package core

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"reflect"

	"github.com/alecthomas/kong"
	"github.com/aler9/gortsplib/v2"
	"github.com/gin-gonic/gin"

	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/confwatcher"
	"github.com/aler9/rtsp-simple-server/internal/externalcmd"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/rlimit"
	"github.com/aler9/rtsp-simple-server/internal/rpicamera"
)

var version = "v0.0.0"

// Core is an instance of rtsp-simple-server.
type Core struct {
	ctx             context.Context
	ctxCancel       func()
	confPath        string
	conf            *conf.Conf
	confFound       bool
	logger          *logger.Logger
	externalCmdPool *externalcmd.Pool
	metrics         *metrics
	pprof           *pprof
	pathManager     *pathManager
	rtspServer      *rtspServer
	rtspsServer     *rtspServer
	rtmpServer      *rtmpServer
	rtmpsServer     *rtmpServer
	hlsServer       *hlsServer
	webRTCServer    *webRTCServer
	api             *api
	confWatcher     *confwatcher.ConfWatcher

	// in
	chAPIConfigSet chan *conf.Conf

	// out
	done chan struct{}
}

var cli struct {
	Version  bool   `help:"print version"`
	Confpath string `arg:"" default:"rtsp-simple-server.yml"`
}

// New allocates a core.
func New(args []string) (*Core, bool) {
	parser, err := kong.New(&cli,
		kong.Description("MediaMTX / rtsp-simple-server "+version),
		kong.UsageOnError(),
		kong.ValueFormatter(func(value *kong.Value) string {
			switch value.Name {
			case "confpath":
				return "path to a config file. The default is rtsp-simple-server.yml."

			default:
				return kong.DefaultHelpValueFormatter(value)
			}
		}))
	if err != nil {
		panic(err)
	}

	_, err = parser.Parse(args)
	parser.FatalIfErrorf(err)

	if cli.Version {
		fmt.Println(version)
		os.Exit(0)
	}

	ctx, ctxCancel := context.WithCancel(context.Background())

	p := &Core{
		ctx:            ctx,
		ctxCancel:      ctxCancel,
		confPath:       cli.Confpath,
		chAPIConfigSet: make(chan *conf.Conf),
		done:           make(chan struct{}),
	}

	p.conf, p.confFound, err = conf.Load(p.confPath)
	if err != nil {
		fmt.Printf("ERR: %s\n", err)
		return nil, false
	}

	err = p.createResources(true)
	if err != nil {
		if p.logger != nil {
			p.Log(logger.Error, "%s", err)
		} else {
			fmt.Printf("ERR: %s\n", err)
		}
		p.closeResources(nil, false)
		return nil, false
	}

	go p.run()

	return p, true
}

// Close closes Core and waits for all goroutines to return.
func (p *Core) Close() {
	p.ctxCancel()
	<-p.done
}

// Wait waits for the Core to exit.
func (p *Core) Wait() {
	<-p.done
}

// Log is the main logging function.
func (p *Core) Log(level logger.Level, format string, args ...interface{}) {
	p.logger.Log(level, format, args...)
}

func (p *Core) run() {
	defer close(p.done)

	confChanged := func() chan struct{} {
		if p.confWatcher != nil {
			return p.confWatcher.Watch()
		}
		return make(chan struct{})
	}()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

outer:
	for {
		select {
		case <-confChanged:
			p.Log(logger.Info, "reloading configuration (file changed)")

			newConf, _, err := conf.Load(p.confPath)
			if err != nil {
				p.Log(logger.Error, "%s", err)
				break outer
			}

			err = p.reloadConf(newConf, false)
			if err != nil {
				p.Log(logger.Error, "%s", err)
				break outer
			}

		case newConf := <-p.chAPIConfigSet:
			p.Log(logger.Info, "reloading configuration (API request)")

			err := p.reloadConf(newConf, true)
			if err != nil {
				p.Log(logger.Error, "%s", err)
				break outer
			}

		case <-interrupt:
			p.Log(logger.Info, "shutting down gracefully")
			break outer

		case <-p.ctx.Done():
			break outer
		}
	}

	p.ctxCancel()

	p.closeResources(nil, false)
}

func (p *Core) createResources(initial bool) error {
	var err error

	if p.logger == nil {
		p.logger, err = logger.New(
			logger.Level(p.conf.LogLevel),
			p.conf.LogDestinations,
			p.conf.LogFile,
		)
		if err != nil {
			return err
		}
	}

	if initial {
		p.Log(logger.Info, "MediaMTX / rtsp-simple-server %s", version)
		if !p.confFound {
			p.Log(logger.Warn, "configuration file not found, using an empty configuration")
		}

		// on Linux, try to raise the number of file descriptors that can be opened
		// to allow the maximum possible number of clients
		// do not check for errors
		rlimit.Raise()

		gin.SetMode(gin.ReleaseMode)

		p.externalCmdPool = externalcmd.NewPool()
	}

	if p.conf.Metrics {
		if p.metrics == nil {
			p.metrics, err = newMetrics(
				p.conf.MetricsAddress,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if p.conf.PPROF {
		if p.pprof == nil {
			p.pprof, err = newPPROF(
				p.conf.PPROFAddress,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if p.pathManager == nil {
		p.pathManager = newPathManager(
			p.ctx,
			p.conf.RTSPAddress,
			p.conf.ReadTimeout,
			p.conf.WriteTimeout,
			p.conf.ReadBufferCount,
			p.conf.Paths,
			p.externalCmdPool,
			p.metrics,
			p,
		)
	}

	if !p.conf.RTSPDisable &&
		(p.conf.Encryption == conf.EncryptionNo ||
			p.conf.Encryption == conf.EncryptionOptional) {
		if p.rtspServer == nil {
			_, useUDP := p.conf.Protocols[conf.Protocol(gortsplib.TransportUDP)]
			_, useMulticast := p.conf.Protocols[conf.Protocol(gortsplib.TransportUDPMulticast)]
			p.rtspServer, err = newRTSPServer(
				p.ctx,
				p.conf.ExternalAuthenticationURL,
				p.conf.RTSPAddress,
				p.conf.AuthMethods,
				p.conf.ReadTimeout,
				p.conf.WriteTimeout,
				p.conf.ReadBufferCount,
				useUDP,
				useMulticast,
				p.conf.RTPAddress,
				p.conf.RTCPAddress,
				p.conf.MulticastIPRange,
				p.conf.MulticastRTPPort,
				p.conf.MulticastRTCPPort,
				false,
				"",
				"",
				p.conf.RTSPAddress,
				p.conf.Protocols,
				p.conf.RunOnConnect,
				p.conf.RunOnConnectRestart,
				p.externalCmdPool,
				p.metrics,
				p.pathManager,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if !p.conf.RTSPDisable &&
		(p.conf.Encryption == conf.EncryptionStrict ||
			p.conf.Encryption == conf.EncryptionOptional) {
		if p.rtspsServer == nil {
			p.rtspsServer, err = newRTSPServer(
				p.ctx,
				p.conf.ExternalAuthenticationURL,
				p.conf.RTSPSAddress,
				p.conf.AuthMethods,
				p.conf.ReadTimeout,
				p.conf.WriteTimeout,
				p.conf.ReadBufferCount,
				false,
				false,
				"",
				"",
				"",
				0,
				0,
				true,
				p.conf.ServerCert,
				p.conf.ServerKey,
				p.conf.RTSPAddress,
				p.conf.Protocols,
				p.conf.RunOnConnect,
				p.conf.RunOnConnectRestart,
				p.externalCmdPool,
				p.metrics,
				p.pathManager,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if !p.conf.RTMPDisable &&
		(p.conf.RTMPEncryption == conf.EncryptionNo ||
			p.conf.RTMPEncryption == conf.EncryptionOptional) {
		if p.rtmpServer == nil {
			p.rtmpServer, err = newRTMPServer(
				p.ctx,
				p.conf.ExternalAuthenticationURL,
				p.conf.RTMPAddress,
				p.conf.ReadTimeout,
				p.conf.WriteTimeout,
				p.conf.ReadBufferCount,
				false,
				"",
				"",
				p.conf.RTSPAddress,
				p.conf.RunOnConnect,
				p.conf.RunOnConnectRestart,
				p.externalCmdPool,
				p.metrics,
				p.pathManager,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if !p.conf.RTMPDisable &&
		(p.conf.RTMPEncryption == conf.EncryptionStrict ||
			p.conf.RTMPEncryption == conf.EncryptionOptional) {
		if p.rtmpsServer == nil {
			p.rtmpsServer, err = newRTMPServer(
				p.ctx,
				p.conf.ExternalAuthenticationURL,
				p.conf.RTMPSAddress,
				p.conf.ReadTimeout,
				p.conf.WriteTimeout,
				p.conf.ReadBufferCount,
				true,
				p.conf.RTMPServerCert,
				p.conf.RTMPServerKey,
				p.conf.RTSPAddress,
				p.conf.RunOnConnect,
				p.conf.RunOnConnectRestart,
				p.externalCmdPool,
				p.metrics,
				p.pathManager,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if !p.conf.HLSDisable {
		if p.hlsServer == nil {
			p.hlsServer, err = newHLSServer(
				p.ctx,
				p.conf.HLSAddress,
				p.conf.HLSEncryption,
				p.conf.HLSServerKey,
				p.conf.HLSServerCert,
				p.conf.ExternalAuthenticationURL,
				p.conf.HLSAlwaysRemux,
				p.conf.HLSVariant,
				p.conf.HLSSegmentCount,
				p.conf.HLSSegmentDuration,
				p.conf.HLSPartDuration,
				p.conf.HLSSegmentMaxSize,
				p.conf.HLSAllowOrigin,
				p.conf.HLSTrustedProxies,
				p.conf.HLSDirectory,
				p.conf.ReadBufferCount,
				p.pathManager,
				p.metrics,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if !p.conf.WebRTCDisable {
		if p.webRTCServer == nil {
			p.webRTCServer, err = newWebRTCServer(
				p.ctx,
				p.conf.ExternalAuthenticationURL,
				p.conf.WebRTCAddress,
				p.conf.WebRTCEncryption,
				p.conf.WebRTCServerKey,
				p.conf.WebRTCServerCert,
				p.conf.WebRTCAllowOrigin,
				p.conf.WebRTCTrustedProxies,
				p.conf.WebRTCICEServers,
				p.conf.ReadBufferCount,
				p.pathManager,
				p.metrics,
				p,
				p.conf.WebRTCICEHostNAT1To1IPs,
				p.conf.WebRTCICEUDPMuxAddress,
				p.conf.WebRTCICETCPMuxAddress,
			)
			if err != nil {
				return err
			}
		}
	}

	if p.conf.API {
		if p.api == nil {
			p.api, err = newAPI(
				p.conf.APIAddress,
				p.conf,
				p.pathManager,
				p.rtspServer,
				p.rtspsServer,
				p.rtmpServer,
				p.rtmpsServer,
				p.hlsServer,
				p.webRTCServer,
				p,
			)
			if err != nil {
				return err
			}
		}
	}

	if initial && p.confFound {
		p.confWatcher, err = confwatcher.New(p.confPath)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *Core) closeResources(newConf *conf.Conf, calledByAPI bool) {
	closeLogger := newConf == nil ||
		!reflect.DeepEqual(newConf.LogDestinations, p.conf.LogDestinations) ||
		newConf.LogFile != p.conf.LogFile

	closeMetrics := newConf == nil ||
		newConf.Metrics != p.conf.Metrics ||
		newConf.MetricsAddress != p.conf.MetricsAddress

	closePPROF := newConf == nil ||
		newConf.PPROF != p.conf.PPROF ||
		newConf.PPROFAddress != p.conf.PPROFAddress

	closePathManager := newConf == nil ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.ReadBufferCount != p.conf.ReadBufferCount ||
		closeMetrics
	if !closePathManager && !reflect.DeepEqual(newConf.Paths, p.conf.Paths) {
		p.pathManager.confReload(newConf.Paths)
	}

	closeRTSPServer := newConf == nil ||
		newConf.RTSPDisable != p.conf.RTSPDisable ||
		newConf.Encryption != p.conf.Encryption ||
		newConf.ExternalAuthenticationURL != p.conf.ExternalAuthenticationURL ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		!reflect.DeepEqual(newConf.AuthMethods, p.conf.AuthMethods) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.ReadBufferCount != p.conf.ReadBufferCount ||
		!reflect.DeepEqual(newConf.Protocols, p.conf.Protocols) ||
		newConf.RTPAddress != p.conf.RTPAddress ||
		newConf.RTCPAddress != p.conf.RTCPAddress ||
		newConf.MulticastIPRange != p.conf.MulticastIPRange ||
		newConf.MulticastRTPPort != p.conf.MulticastRTPPort ||
		newConf.MulticastRTCPPort != p.conf.MulticastRTCPPort ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		!reflect.DeepEqual(newConf.Protocols, p.conf.Protocols) ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		closeMetrics ||
		closePathManager

	closeRTSPSServer := newConf == nil ||
		newConf.RTSPDisable != p.conf.RTSPDisable ||
		newConf.Encryption != p.conf.Encryption ||
		newConf.ExternalAuthenticationURL != p.conf.ExternalAuthenticationURL ||
		newConf.RTSPSAddress != p.conf.RTSPSAddress ||
		!reflect.DeepEqual(newConf.AuthMethods, p.conf.AuthMethods) ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.ReadBufferCount != p.conf.ReadBufferCount ||
		newConf.ServerCert != p.conf.ServerCert ||
		newConf.ServerKey != p.conf.ServerKey ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		!reflect.DeepEqual(newConf.Protocols, p.conf.Protocols) ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		closeMetrics ||
		closePathManager

	closeRTMPServer := newConf == nil ||
		newConf.RTMPDisable != p.conf.RTMPDisable ||
		newConf.RTMPEncryption != p.conf.RTMPEncryption ||
		newConf.RTMPAddress != p.conf.RTMPAddress ||
		newConf.ExternalAuthenticationURL != p.conf.ExternalAuthenticationURL ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.ReadBufferCount != p.conf.ReadBufferCount ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		closeMetrics ||
		closePathManager

	closeRTMPSServer := newConf == nil ||
		newConf.RTMPDisable != p.conf.RTMPDisable ||
		newConf.RTMPEncryption != p.conf.RTMPEncryption ||
		newConf.RTMPSAddress != p.conf.RTMPSAddress ||
		newConf.ExternalAuthenticationURL != p.conf.ExternalAuthenticationURL ||
		newConf.ReadTimeout != p.conf.ReadTimeout ||
		newConf.WriteTimeout != p.conf.WriteTimeout ||
		newConf.ReadBufferCount != p.conf.ReadBufferCount ||
		newConf.RTMPServerCert != p.conf.RTMPServerCert ||
		newConf.RTMPServerKey != p.conf.RTMPServerKey ||
		newConf.RTSPAddress != p.conf.RTSPAddress ||
		newConf.RunOnConnect != p.conf.RunOnConnect ||
		newConf.RunOnConnectRestart != p.conf.RunOnConnectRestart ||
		closeMetrics ||
		closePathManager

	closeHLSServer := newConf == nil ||
		newConf.HLSDisable != p.conf.HLSDisable ||
		newConf.HLSAddress != p.conf.HLSAddress ||
		newConf.HLSEncryption != p.conf.HLSEncryption ||
		newConf.HLSServerKey != p.conf.HLSServerKey ||
		newConf.HLSServerCert != p.conf.HLSServerCert ||
		newConf.ExternalAuthenticationURL != p.conf.ExternalAuthenticationURL ||
		newConf.HLSAlwaysRemux != p.conf.HLSAlwaysRemux ||
		newConf.HLSVariant != p.conf.HLSVariant ||
		newConf.HLSSegmentCount != p.conf.HLSSegmentCount ||
		newConf.HLSSegmentDuration != p.conf.HLSSegmentDuration ||
		newConf.HLSPartDuration != p.conf.HLSPartDuration ||
		newConf.HLSSegmentMaxSize != p.conf.HLSSegmentMaxSize ||
		newConf.HLSAllowOrigin != p.conf.HLSAllowOrigin ||
		!reflect.DeepEqual(newConf.HLSTrustedProxies, p.conf.HLSTrustedProxies) ||
		newConf.HLSDirectory != p.conf.HLSDirectory ||
		newConf.ReadBufferCount != p.conf.ReadBufferCount ||
		closePathManager ||
		closeMetrics

	closeWebRTCServer := newConf == nil ||
		newConf.WebRTCDisable != p.conf.WebRTCDisable ||
		newConf.ExternalAuthenticationURL != p.conf.ExternalAuthenticationURL ||
		newConf.WebRTCAddress != p.conf.WebRTCAddress ||
		newConf.WebRTCEncryption != p.conf.WebRTCEncryption ||
		newConf.WebRTCServerKey != p.conf.WebRTCServerKey ||
		newConf.WebRTCServerCert != p.conf.WebRTCServerCert ||
		newConf.WebRTCAllowOrigin != p.conf.WebRTCAllowOrigin ||
		!reflect.DeepEqual(newConf.WebRTCTrustedProxies, p.conf.WebRTCTrustedProxies) ||
		!reflect.DeepEqual(newConf.WebRTCICEServers, p.conf.WebRTCICEServers) ||
		newConf.ReadBufferCount != p.conf.ReadBufferCount ||
		closeMetrics ||
		closePathManager ||
		!reflect.DeepEqual(newConf.WebRTCICEHostNAT1To1IPs, p.conf.WebRTCICEHostNAT1To1IPs) ||
		newConf.WebRTCICEUDPMuxAddress != p.conf.WebRTCICEUDPMuxAddress ||
		newConf.WebRTCICETCPMuxAddress != p.conf.WebRTCICETCPMuxAddress

	closeAPI := newConf == nil ||
		newConf.API != p.conf.API ||
		newConf.APIAddress != p.conf.APIAddress ||
		closePathManager ||
		closeRTSPServer ||
		closeRTSPSServer ||
		closeRTMPServer ||
		closeHLSServer ||
		closeWebRTCServer

	if newConf == nil && p.confWatcher != nil {
		p.confWatcher.Close()
		p.confWatcher = nil
	}

	if p.api != nil {
		if closeAPI {
			p.api.close()
			p.api = nil
		} else if !calledByAPI { // avoid a loop
			p.api.confReload(newConf)
		}
	}

	if closeRTSPSServer && p.rtspsServer != nil {
		p.rtspsServer.close()
		p.rtspsServer = nil
	}

	if closeRTSPServer && p.rtspServer != nil {
		p.rtspServer.close()
		p.rtspServer = nil
	}

	if closePathManager && p.pathManager != nil {
		p.pathManager.close()
		p.pathManager = nil
	}

	if closeWebRTCServer && p.webRTCServer != nil {
		p.webRTCServer.close()
		p.webRTCServer = nil
	}

	if closeHLSServer && p.hlsServer != nil {
		p.hlsServer.close()
		p.hlsServer = nil
	}

	if closeRTMPSServer && p.rtmpsServer != nil {
		p.rtmpsServer.close()
		p.rtmpsServer = nil
	}

	if closeRTMPServer && p.rtmpServer != nil {
		p.rtmpServer.close()
		p.rtmpServer = nil
	}

	if closePPROF && p.pprof != nil {
		p.pprof.close()
		p.pprof = nil
	}

	if closeMetrics && p.metrics != nil {
		p.metrics.close()
		p.metrics = nil
	}

	if newConf == nil && p.externalCmdPool != nil {
		p.Log(logger.Info, "waiting for external commands")
		p.externalCmdPool.Close()
	}

	if newConf == nil {
		rpicamera.Cleanup()
	}

	if closeLogger {
		p.logger.Close()
		p.logger = nil
	}
}

func (p *Core) reloadConf(newConf *conf.Conf, calledByAPI bool) error {
	p.closeResources(newConf, calledByAPI)
	p.conf = newConf
	return p.createResources(false)
}

// apiConfigSet is called by api.
func (p *Core) apiConfigSet(conf *conf.Conf) {
	select {
	case p.chAPIConfigSet <- conf:
	case <-p.ctx.Done():
	}
}
