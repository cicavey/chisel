package chclient

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cicavey/chisel/share"
	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
	"golang.org/x/crypto/ssh"
)

//Config represents a client configuration
type Config struct {
	shared      *chshare.Config
	Cert        string
	Key         string
	Fingerprint string
	Auth        string
	KeepAlive   time.Duration
	Server      string
	HTTPProxy   string
	Remotes     []string
}

//Client represents a client instance
type Client struct {
	*chshare.Logger
	config       *Config
	tlsConfig    *tls.Config
	sshConfig    *ssh.ClientConfig
	proxies      []*tcpProxy
	sshConn      ssh.Conn
	httpProxyURL *url.URL
	server       string
	running      bool
	runningc     chan error
}

//NewClient creates a new client instance
func NewClient(config *Config) (*Client, error) {

	//apply default scheme
	if !strings.HasPrefix(config.Server, "http") {
		config.Server = "http://" + config.Server
	}

	u, err := url.Parse(config.Server)
	if err != nil {
		return nil, err
	}

	//apply default port
	if !regexp.MustCompile(`:\d+$`).MatchString(u.Host) {
		if u.Scheme == "https" || u.Scheme == "wss" {
			u.Host = u.Host + ":443"
		} else {
			u.Host = u.Host + ":80"
		}
	}

	//swap to websockets scheme
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)

	shared := &chshare.Config{}
	for _, s := range config.Remotes {
		r, err := chshare.DecodeRemote(s)
		if err != nil {
			return nil, fmt.Errorf("Failed to decode remote '%s': %s", s, err)
		}
		shared.Remotes = append(shared.Remotes, r)
	}
	config.shared = shared

	client := &Client{
		Logger:   chshare.NewLogger("client"),
		config:   config,
		server:   u.String(),
		running:  true,
		runningc: make(chan error, 1),
	}
	client.Info = true

	if config.Cert != "" && config.Key != "" {
		cert, err := tls.LoadX509KeyPair(config.Cert, config.Key)
		if err != nil {
			return nil, fmt.Errorf("Failed to load client keys: %s", err)
		}
		client.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true} // TODO: Verify entire chain
	}

	if p := config.HTTPProxy; p != "" {
		client.httpProxyURL, err = url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("Invalid proxy URL (%s)", err)
		}
	}

	user, pass := chshare.ParseAuth(config.Auth)

	client.sshConfig = &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		ClientVersion:   chshare.ProtocolVersion + "-client",
		HostKeyCallback: client.verifyServer,
	}

	return client, nil
}

//Run starts client and blocks while connected
func (c *Client) Run() error {
	go c.start()
	return c.Wait()
}

func (c *Client) verifyServer(hostname string, remote net.Addr, key ssh.PublicKey) error {
	expect := c.config.Fingerprint
	got := chshare.FingerprintKey(key)
	if expect != "" && !strings.HasPrefix(got, expect) {
		return fmt.Errorf("Invalid fingerprint (%s)", got)
	}
	//overwrite with complete fingerprint
	c.Infof("Fingerprint %s", got)
	return nil
}

//Start client and do not block
func (c *Client) Start() {
	go c.start()
}

func (c *Client) start() {
	via := ""
	if c.httpProxyURL != nil {
		via = " via " + c.httpProxyURL.String()
	}
	c.Infof("Connecting to %s%s\n", c.server, via)

	//prepare proxies
	for i, r := range c.config.shared.Remotes {
		proxy := newTCPProxy(c, i, r)
		go proxy.start()
		c.proxies = append(c.proxies, proxy)
	}

	//optional keepalive loop
	if c.config.KeepAlive > 0 {
		go func() {
			for range time.Tick(c.config.KeepAlive) {
				if c.sshConn != nil {
					c.sshConn.SendRequest("ping", true, nil)
				}
			}
		}()
	}

	//connection loop!
	var connerr error
	b := &backoff.Backoff{Max: 5 * time.Minute}

	for {
		//NOTE: break == dont retry on handshake failures
		if !c.running {
			break
		}
		if connerr != nil {
			d := b.Duration()
			c.Debugf("Connection error: %s", connerr)
			c.Infof("Retrying in %s...", d)
			connerr = nil
			time.Sleep(d)
		}

		d := websocket.Dialer{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			Subprotocols:    []string{chshare.ProtocolVersion},
			TLSClientConfig: c.tlsConfig,
		}
		//optionally CONNECT proxy
		if c.httpProxyURL != nil {
			d.Proxy = func(*http.Request) (*url.URL, error) {
				return c.httpProxyURL, nil
			}
		}
		wsConn, _, err := d.Dial(c.server, nil)

		if err != nil {
			connerr = err
			continue
		}
		conn := chshare.NewWebSocketConn(wsConn)
		// perform SSH handshake on net.Conn
		c.Debugf("Handshaking...")
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, "", c.sshConfig)
		if err != nil {
			if strings.Contains(err.Error(), "unable to authenticate") {
				c.Infof("Authentication failed")
				c.Debugf(err.Error())
			} else {
				c.Infof(err.Error())
			}
			break
		}
		c.config.shared.Version = chshare.BuildVersion
		conf, _ := chshare.EncodeConfig(c.config.shared)
		c.Debugf("Sending configurating")
		t0 := time.Now()
		_, configerr, err := sshConn.SendRequest("config", true, conf)
		if err != nil {
			c.Infof("Config verification failed")
			break
		}
		if len(configerr) > 0 {
			c.Infof(string(configerr))
			break
		}
		c.Infof("Connected (Latency %s)", time.Now().Sub(t0))
		//connected
		b.Reset()
		c.sshConn = sshConn
		go ssh.DiscardRequests(reqs)
		go chshare.RejectStreams(chans) //TODO allow client to ConnectStreams
		err = sshConn.Wait()
		//disconnected
		c.sshConn = nil
		if err != nil && err != io.EOF {
			connerr = err
			continue
		}
		c.Infof("Disconnected\n")
	}
	close(c.runningc)
}

//Wait blocks while the client is running
func (c *Client) Wait() error {
	return <-c.runningc
}

//Close manual stops the client
func (c *Client) Close() error {
	c.running = false
	if c.sshConn == nil {
		return nil
	}
	return c.sshConn.Close()
}
