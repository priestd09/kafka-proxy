package proxy

import (
	"crypto/tls"
	"github.com/grepplabs/kafka-proxy/config"
	"github.com/grepplabs/kafka-proxy/pkg/apis"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"net"
	"sync"
	"time"
)

// Conn represents a connection from a client to a specific instance.
type Conn struct {
	BrokerAddress   string
	LocalConnection net.Conn
}

// Client is a type to handle connecting to a Server. All fields are required
// unless otherwise specified.
type Client struct {
	conns *ConnSet

	// Kafka Net configuration
	config *config.Config

	// Config of Proxy request-response processor (instance p)
	processorConfig ProcessorConfig

	dialer         Dialer
	tcpConnOptions TCPConnOptions

	stopRun  chan struct{}
	stopOnce sync.Once

	saslPlainAuth *SASLPlainAuth
	authClient    *AuthClient
}

func NewClient(conns *ConnSet, c *config.Config, netAddressMappingFunc config.NetAddressMappingFunc, passwordAuthenticator apis.PasswordAuthenticator, tokenProvider apis.TokenProvider, tokenInfo apis.TokenInfo) (*Client, error) {
	tlsConfig, err := newTLSClientConfig(c)
	if err != nil {
		return nil, err
	}
	dialer, err := newDialer(c, tlsConfig)
	if err != nil {
		return nil, err
	}
	tcpConnOptions := TCPConnOptions{
		KeepAlive:       c.Kafka.KeepAlive,
		WriteBufferSize: c.Kafka.ConnectionWriteBufferSize,
		ReadBufferSize:  c.Kafka.ConnectionReadBufferSize,
	}

	forbiddenApiKeys := make(map[int16]struct{})
	if len(c.Kafka.ForbiddenApiKeys) != 0 {
		logrus.Warnf("Kafka operations for Api Keys %v will be forbidden.", c.Kafka.ForbiddenApiKeys)
		for _, apiKey := range c.Kafka.ForbiddenApiKeys {
			forbiddenApiKeys[int16(apiKey)] = struct{}{}
		}
	}
	if c.Auth.Local.Enable && passwordAuthenticator == nil {
		return nil, errors.New("Auth.Local.Enable is enabled but passwordAuthenticator is nil")
	}

	if c.Auth.Gateway.Client.Enable && tokenProvider == nil {
		return nil, errors.New("Auth.Gateway.Client.Enable is enabled but tokenProvider is nil")
	}
	if c.Auth.Gateway.Server.Enable && tokenInfo == nil {
		return nil, errors.New("Auth.Gateway.Server.Enable is enabled but tokenInfo is nil")
	}

	return &Client{conns: conns, config: c, dialer: dialer, tcpConnOptions: tcpConnOptions, stopRun: make(chan struct{}, 1),
		saslPlainAuth: &SASLPlainAuth{
			clientID:     c.Kafka.ClientID,
			writeTimeout: c.Kafka.WriteTimeout,
			readTimeout:  c.Kafka.ReadTimeout,
			username:     c.Kafka.SASL.Username,
			password:     c.Kafka.SASL.Password,
		},
		authClient: &AuthClient{
			enabled:       c.Auth.Gateway.Client.Enable,
			magic:         c.Auth.Gateway.Client.Magic,
			method:        c.Auth.Gateway.Client.Method,
			timeout:       c.Auth.Gateway.Client.Timeout,
			tokenProvider: tokenProvider,
		},
		processorConfig: ProcessorConfig{
			MaxOpenRequests:       c.Kafka.MaxOpenRequests,
			NetAddressMappingFunc: netAddressMappingFunc,
			RequestBufferSize:     c.Proxy.RequestBufferSize,
			ResponseBufferSize:    c.Proxy.ResponseBufferSize,
			ReadTimeout:           c.Kafka.ReadTimeout,
			WriteTimeout:          c.Kafka.WriteTimeout,
			LocalSasl: &LocalSasl{
				enabled:            c.Auth.Local.Enable,
				timeout:            c.Auth.Local.Timeout,
				localAuthenticator: passwordAuthenticator},
			AuthServer: &AuthServer{
				enabled:   c.Auth.Gateway.Server.Enable,
				magic:     c.Auth.Gateway.Server.Magic,
				method:    c.Auth.Gateway.Server.Method,
				timeout:   c.Auth.Gateway.Server.Timeout,
				tokenInfo: tokenInfo,
			},
			ForbiddenApiKeys: forbiddenApiKeys,
		}}, nil
}

func newDialer(c *config.Config, tlsConfig *tls.Config) (Dialer, error) {
	directDialer := directDialer{
		dialTimeout: c.Kafka.DialTimeout,
		keepAlive:   c.Kafka.KeepAlive,
	}

	var rawDialer Dialer
	if c.ForwardProxy.Url != "" {
		switch c.ForwardProxy.Scheme {
		case "socks5":
			logrus.Infof("Kafka clients will connect through the SOCKS5 proxy %s", c.ForwardProxy.Address)
			rawDialer = &socks5Dialer{
				directDialer: directDialer,
				proxyNetwork: "tcp",
				proxyAddr:    c.ForwardProxy.Address,
				username:     c.ForwardProxy.Username,
				password:     c.ForwardProxy.Password,
			}
		case "http":
			logrus.Infof("Kafka clients will connect through the HTTP proxy %s using CONNECT", c.ForwardProxy.Address)

			rawDialer = &httpProxy{
				forwardDialer: directDialer,
				network:       "tcp",
				hostPort:      c.ForwardProxy.Address,
				username:      c.ForwardProxy.Username,
				password:      c.ForwardProxy.Password,
			}
		default:
			return nil, errors.New("Only http or socks5 proxy is supported")
		}
	} else {
		rawDialer = directDialer
	}
	if c.Kafka.TLS.Enable {
		if tlsConfig == nil {
			return nil, errors.New("tlsConfig must not be nil")
		}
		tlsDialer := tlsDialer{
			timeout:   c.Kafka.DialTimeout,
			rawDialer: rawDialer,
			config:    tlsConfig,
		}
		return tlsDialer, nil
	}
	return rawDialer, nil
}

// Run causes the client to start waiting for new connections to connSrc and
// proxy them to the destination instance. It blocks until connSrc is closed.
func (c *Client) Run(connSrc <-chan Conn) error {
STOP:
	for {
		select {
		case conn := <-connSrc:
			go withRecover(func() { c.handleConn(conn) })
		case <-c.stopRun:
			break STOP
		}
	}

	logrus.Info("Closing connections")

	if err := c.conns.Close(); err != nil {
		logrus.Infof("closing client had error: %v", err)
	}

	logrus.Info("Proxy is stopped")
	return nil
}

func (c *Client) Close() {
	c.stopOnce.Do(func() {
		close(c.stopRun)
	})
}

func (c *Client) handleConn(conn Conn) {
	proxyConnectionsTotal.WithLabelValues(conn.BrokerAddress).Inc()

	server, err := c.DialAndAuth(conn.BrokerAddress)
	if err != nil {
		logrus.Infof("couldn't connect to %s: %v", conn.BrokerAddress, err)
		conn.LocalConnection.Close()
		return
	}
	if tcpConn, ok := server.(*net.TCPConn); ok {
		if err := c.tcpConnOptions.setTCPConnOptions(tcpConn); err != nil {
			logrus.Infof("WARNING: Error while setting TCP options for kafka connection %s on %v: %v", conn.BrokerAddress, server.LocalAddr(), err)
		}
	}
	c.conns.Add(conn.BrokerAddress, conn.LocalConnection)
	localDesc := "local connection on " + conn.LocalConnection.LocalAddr().String() + " from " + conn.LocalConnection.RemoteAddr().String() + " (" + conn.BrokerAddress + ")"
	copyThenClose(c.processorConfig, server, conn.LocalConnection, conn.BrokerAddress, conn.BrokerAddress, localDesc)
	if err := c.conns.Remove(conn.BrokerAddress, conn.LocalConnection); err != nil {
		logrus.Info(err)
	}
}

func (c *Client) DialAndAuth(brokerAddress string) (net.Conn, error) {
	conn, err := c.dialer.Dial("tcp", brokerAddress)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	err = c.auth(conn)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *Client) auth(conn net.Conn) error {
	if c.config.Auth.Gateway.Client.Enable {
		if err := c.authClient.sendAndReceiveGatewayAuth(conn); err != nil {
			conn.Close()
			return err
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			conn.Close()
			return err
		}
	}
	if c.config.Kafka.SASL.Enable {
		err := c.saslPlainAuth.sendAndReceiveSASLPlainAuth(conn)
		if err != nil {
			conn.Close()
			return err
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			conn.Close()
			return err
		}
	}
	return nil
}
