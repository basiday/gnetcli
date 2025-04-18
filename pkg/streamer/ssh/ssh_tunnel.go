package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"

	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	"github.com/annetutil/gnetcli/pkg/credentials"
)

type Tunnel interface {
	Close()
	IsConnected() bool
	CreateConnect(context.Context) error
	StartForward(network Network, addr string) (net.Conn, error)
}

type SSHTunnel struct {
	Server       Endpoint
	Config       *ssh.ClientConfig
	svrConn      *ssh.Client
	stdioForward *ControlConn
	isOpen       bool
	credentials  credentials.Credentials
	logger       *zap.Logger
	mu           sync.Mutex
	controlFile  string
}

func NewSSHTunnel(host string, credentials credentials.Credentials, opts ...SSHTunnelOption) *SSHTunnel {
	h := &SSHTunnel{
		Server:      NewEndpoint(host, defaultPort, TCP),
		Config:      nil,
		svrConn:     nil,
		isOpen:      false,
		credentials: credentials,
		logger:      zap.NewNop(),
		mu:          sync.Mutex{},
	}

	for _, opt := range opts {
		opt(h)
	}
	return h
}

type SSHTunnelOption func(m *SSHTunnel)

func SSHTunnelWithLogger(log *zap.Logger) SSHTunnelOption {
	return func(h *SSHTunnel) {
		h.logger = log
	}
}

func SSHTunnelWithControlFIle(path string) SSHTunnelOption {
	return func(h *SSHTunnel) {
		h.controlFile = path
	}
}

func SSHTunnelWithNetwork(network Network) SSHTunnelOption {
	return func(h *SSHTunnel) {
		h.Server.Network = network
	}
}

func SSHTunnelWitPort(port int) SSHTunnelOption {
	return func(h *SSHTunnel) {
		h.Server.Port = port
	}
}

func (m *SSHTunnel) CreateConnect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	strOpts := []StreamerOption{
		WithLogger(m.logger),
	}
	if len(m.controlFile) > 0 {
		strOpts = append(strOpts, WithSSHControlFIle(m.controlFile))
	}
	connector := NewStreamer(m.Server.Host, m.credentials, strOpts...)
	conf, err := connector.GetConfig(ctx)
	if err != nil {
		m.logger.Error(err.Error())
		return err
	}

	m.Config = conf
	var conn *ssh.Client

	if len(m.controlFile) != 0 {
		mConn, err := dialControlMasterConf(ctx, m.controlFile, m.Server, conf, m.logger)
		if err != nil {
			return err
		}
		m.stdioForward = mConn
		conn = nil
	} else {
		conn, err = DialCtx(ctx, m.Server, nil, m.Config, m.logger)
	}
	if err != nil {
		m.logger.Debug("unable to connect to tunnel", zap.Error(err))
		if !errors.Is(err, context.Canceled) {
			m.logger.Error(err.Error())
		}
		return err
	}
	m.logger.Debug("connected to tunnel", zap.String("server", m.Server.String()))
	m.svrConn = conn
	m.isOpen = true
	return nil
}

func (m *SSHTunnel) StartForward(network Network, remoteAddr string) (net.Conn, error) {
	if m.stdioForward != nil {
		host, port, err := net.SplitHostPort(remoteAddr)
		if err != nil {
			return nil, fmt.Errorf("invalid port: %s", port)
		}
		portVal, err := strconv.ParseInt(port, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid port: %s", port)
		}
		connForward, err := m.stdioForward.DialControlStdioForward(host, int(portVal))
		if err != nil {
			return nil, err
		}
		return connForward, nil
	}
	if !m.isOpen {
		return nil, errors.New("connection is closed")
	}
	lconn, rconn, err := m.makeSocketFromSocketPair()
	if err != nil {
		return nil, err
	}
	remoteConn, err := m.svrConn.Dial(string(network), remoteAddr)
	if err != nil {
		return nil, err
	}

	m.logger.Debug("start forward", zap.String("to", remoteAddr), zap.String("from", m.svrConn.RemoteAddr().String()))

	copyConn := func(writer, reader net.Conn) error {
		_, err := io.Copy(writer, reader)
		m.logger.Debug("forward done", zap.Error(err))
		return err
	}
	wg, _ := errgroup.WithContext(context.Background())
	wg.Go(func() error {
		err := copyConn(rconn, remoteConn)
		_ = rconn.Close()
		return err
	})
	wg.Go(func() error {
		err := copyConn(remoteConn, rconn)
		_ = remoteConn.Close()
		return err
	})

	go func() {
		err := wg.Wait()
		m.logger.Debug("tunnel done", zap.String("remote", remoteAddr), zap.Error(err))
	}()

	// There is no easy way to make key string from unix conn, so we can't track forwarded cons
	return lconn, nil
}

func (m *SSHTunnel) IsConnected() bool {
	return m.isOpen
}

func (m *SSHTunnel) Close() {
	if !m.isOpen {
		err := errors.New("connection is closed")
		m.logger.Error(err.Error())
		return
	}

	m.isOpen = false

	m.logger.Debug("closing the serverConn")
	if m.svrConn != nil {
		err := m.svrConn.Close()
		if err != nil {
			m.logger.Error(err.Error())
		}
	}
	if m.stdioForward != nil {
		err := m.stdioForward.Close()
		if err != nil {
			m.logger.Error(err.Error())
		}
	}
	m.logger.Debug("tunnel closed")
}

func (m *SSHTunnel) makeSocketFromSocketPair() (net.Conn, net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}

	f0 := os.NewFile(uintptr(fds[0]), "socketpair-0")
	defer f0.Close()
	c0, err := net.FileConn(f0)
	if err != nil {
		return nil, nil, err
	}
	f1 := os.NewFile(uintptr(fds[1]), "socketpair-0")
	defer f1.Close()
	c1, err := net.FileConn(f1)
	if err != nil {
		return nil, nil, err
	}

	return c0, c1, nil
}
