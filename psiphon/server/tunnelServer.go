/*
 * Copyright (c) 2016, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon"
	"golang.org/x/crypto/ssh"
)

const (
	SSH_HANDSHAKE_TIMEOUT                 = 30 * time.Second
	SSH_CONNECTION_READ_DEADLINE          = 5 * time.Minute
	SSH_TCP_PORT_FORWARD_DIAL_TIMEOUT     = 30 * time.Second
	SSH_TCP_PORT_FORWARD_COPY_BUFFER_SIZE = 8192
)

// Disallowed port forward hosts is a failsafe. The server should
// be run on a host with correctly configured firewall rules, or
// containerization, or both.
var SSH_DISALLOWED_PORT_FORWARD_HOSTS = []string{"localhost", "127.0.0.1"}

// TunnelServer is the main server that accepts Psiphon client
// connections, via various obfuscation protocols, and provides
// port forwarding (TCP and UDP) services to the Psiphon client.
// At its core, TunnelServer is an SSH server. SSH is the base
// protocol that provides port forward multiplexing, and transport
// security. Layered on top of SSH, optionally, is Obfuscated SSH
// and meek protocols, which provide further circumvention
// capabilities.
type TunnelServer struct {
	runWaitGroup      *sync.WaitGroup
	listenerError     chan error
	shutdownBroadcast <-chan struct{}
	sshServer         *sshServer
}

// NewTunnelServer initializes a new tunnel server.
func NewTunnelServer(
	support *SupportServices,
	shutdownBroadcast <-chan struct{}) (*TunnelServer, error) {

	sshServer, err := newSSHServer(support, shutdownBroadcast)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	return &TunnelServer{
		runWaitGroup:      new(sync.WaitGroup),
		listenerError:     make(chan error),
		shutdownBroadcast: shutdownBroadcast,
		sshServer:         sshServer,
	}, nil
}

// GetLoadStats returns load stats for the tunnel server. The stats are
// broken down by protocol ("SSH", "OSSH", etc.) and type. Types of stats
// include current connected client count, total number of current port
// forwards.
func (server *TunnelServer) GetLoadStats() map[string]map[string]int64 {
	return server.sshServer.getLoadStats()
}

// Run runs the tunnel server; this function blocks while running a selection of
// listeners that handle connection using various obfuscation protocols.
//
// Run listens on each designated tunnel port and spawns new goroutines to handle
// each client connection. It halts when shutdownBroadcast is signaled. A list of active
// clients is maintained, and when halting all clients are cleanly shutdown.
//
// Each client goroutine handles its own obfuscation (optional), SSH handshake, SSH
// authentication, and then looping on client new channel requests. "direct-tcpip"
// channels, dynamic port fowards, are supported. When the UDPInterceptUdpgwServerAddress
// config parameter is configured, UDP port forwards over a TCP stream, following
// the udpgw protocol, are handled.
//
// A new goroutine is spawned to handle each port forward for each client. Each port
// forward tracks its bytes transferred. Overall per-client stats for connection duration,
// GeoIP, number of port forwards, and bytes transferred are tracked and logged when the
// client shuts down.
func (server *TunnelServer) Run() error {

	type sshListener struct {
		net.Listener
		localAddress   string
		tunnelProtocol string
	}

	// TODO: should TunnelServer hold its own support pointer?
	support := server.sshServer.support

	// First bind all listeners; once all are successful,
	// start accepting connections on each.

	var listeners []*sshListener

	for tunnelProtocol, listenPort := range support.Config.TunnelProtocolPorts {

		localAddress := fmt.Sprintf(
			"%s:%d", support.Config.ServerIPAddress, listenPort)

		listener, err := net.Listen("tcp", localAddress)
		if err != nil {
			for _, existingListener := range listeners {
				existingListener.Listener.Close()
			}
			return psiphon.ContextError(err)
		}

		log.WithContextFields(
			LogFields{
				"localAddress":   localAddress,
				"tunnelProtocol": tunnelProtocol,
			}).Info("listening")

		listeners = append(
			listeners,
			&sshListener{
				Listener:       listener,
				localAddress:   localAddress,
				tunnelProtocol: tunnelProtocol,
			})
	}

	for _, listener := range listeners {
		server.runWaitGroup.Add(1)
		go func(listener *sshListener) {
			defer server.runWaitGroup.Done()

			log.WithContextFields(
				LogFields{
					"localAddress":   listener.localAddress,
					"tunnelProtocol": listener.tunnelProtocol,
				}).Info("running")

			server.sshServer.runListener(
				listener.Listener,
				server.listenerError,
				listener.tunnelProtocol)

			log.WithContextFields(
				LogFields{
					"localAddress":   listener.localAddress,
					"tunnelProtocol": listener.tunnelProtocol,
				}).Info("stopped")

		}(listener)
	}

	var err error
	select {
	case <-server.shutdownBroadcast:
	case err = <-server.listenerError:
	}

	for _, listener := range listeners {
		listener.Close()
	}
	server.sshServer.stopClients()
	server.runWaitGroup.Wait()

	log.WithContext().Info("stopped")

	return err
}

type sshClientID uint64

type sshServer struct {
	support              *SupportServices
	shutdownBroadcast    <-chan struct{}
	sshHostKey           ssh.Signer
	nextClientID         sshClientID
	clientsMutex         sync.Mutex
	stoppingClients      bool
	acceptedClientCounts map[string]int64
	clients              map[sshClientID]*sshClient
}

func newSSHServer(
	support *SupportServices,
	shutdownBroadcast <-chan struct{}) (*sshServer, error) {

	privateKey, err := ssh.ParseRawPrivateKey([]byte(support.Config.SSHPrivateKey))
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	// TODO: use cert (ssh.NewCertSigner) for anti-fingerprint?
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return nil, psiphon.ContextError(err)
	}

	return &sshServer{
		support:              support,
		shutdownBroadcast:    shutdownBroadcast,
		sshHostKey:           signer,
		nextClientID:         1,
		acceptedClientCounts: make(map[string]int64),
		clients:              make(map[sshClientID]*sshClient),
	}, nil
}

// runListener is intended to run an a goroutine; it blocks
// running a particular listener. If an unrecoverable error
// occurs, it will send the error to the listenerError channel.
func (sshServer *sshServer) runListener(
	listener net.Listener,
	listenerError chan<- error,
	tunnelProtocol string) {

	handleClient := func(clientConn net.Conn) {
		// process each client connection concurrently
		go sshServer.handleClient(tunnelProtocol, clientConn)
	}

	// Note: when exiting due to a unrecoverable error, be sure
	// to try to send the error to listenerError so that the outer
	// TunnelServer.Run will properly shut down instead of remaining
	// running.

	if psiphon.TunnelProtocolUsesMeekHTTP(tunnelProtocol) ||
		psiphon.TunnelProtocolUsesMeekHTTPS(tunnelProtocol) {

		meekServer, err := NewMeekServer(
			sshServer.support,
			listener,
			psiphon.TunnelProtocolUsesMeekHTTPS(tunnelProtocol),
			handleClient,
			sshServer.shutdownBroadcast)
		if err != nil {
			select {
			case listenerError <- psiphon.ContextError(err):
			default:
			}
			return
		}

		meekServer.Run()

	} else {

		for {
			conn, err := listener.Accept()

			select {
			case <-sshServer.shutdownBroadcast:
				if err == nil {
					conn.Close()
				}
				return
			default:
			}

			if err != nil {
				if e, ok := err.(net.Error); ok && e.Temporary() {
					log.WithContextFields(LogFields{"error": err}).Error("accept failed")
					// Temporary error, keep running
					continue
				}

				select {
				case listenerError <- psiphon.ContextError(err):
				default:
				}
				return
			}

			handleClient(conn)
		}
	}
}

// An accepted client has completed a direct TCP or meek connection and has a net.Conn. Registration
// is for tracking the number of connections.
func (sshServer *sshServer) registerAcceptedClient(tunnelProtocol string) {

	sshServer.clientsMutex.Lock()
	defer sshServer.clientsMutex.Unlock()

	sshServer.acceptedClientCounts[tunnelProtocol] += 1
}

func (sshServer *sshServer) unregisterAcceptedClient(tunnelProtocol string) {

	sshServer.clientsMutex.Lock()
	defer sshServer.clientsMutex.Unlock()

	sshServer.acceptedClientCounts[tunnelProtocol] -= 1
}

// An established client has completed its SSH handshake and has a ssh.Conn. Registration is
// for tracking the number of fully established clients and for maintaining a list of running
// clients (for stopping at shutdown time).
func (sshServer *sshServer) registerEstablishedClient(client *sshClient) (sshClientID, bool) {

	sshServer.clientsMutex.Lock()
	defer sshServer.clientsMutex.Unlock()

	if sshServer.stoppingClients {
		return 0, false
	}

	clientID := sshServer.nextClientID
	sshServer.nextClientID += 1

	sshServer.clients[clientID] = client

	return clientID, true
}

func (sshServer *sshServer) unregisterEstablishedClient(clientID sshClientID) {

	sshServer.clientsMutex.Lock()
	client := sshServer.clients[clientID]
	delete(sshServer.clients, clientID)
	sshServer.clientsMutex.Unlock()

	if client != nil {
		client.stop()
	}
}

func (sshServer *sshServer) getLoadStats() map[string]map[string]int64 {

	sshServer.clientsMutex.Lock()
	defer sshServer.clientsMutex.Unlock()

	loadStats := make(map[string]map[string]int64)

	// Explicitly populate with zeros to get 0 counts in log messages derived from getLoadStats()

	for tunnelProtocol, _ := range sshServer.support.Config.TunnelProtocolPorts {
		loadStats[tunnelProtocol] = make(map[string]int64)
		loadStats[tunnelProtocol]["AcceptedClients"] = 0
		loadStats[tunnelProtocol]["EstablishedClients"] = 0
		loadStats[tunnelProtocol]["TCPPortForwards"] = 0
		loadStats[tunnelProtocol]["TotalTCPPortForwards"] = 0
		loadStats[tunnelProtocol]["UDPPortForwards"] = 0
		loadStats[tunnelProtocol]["TotalUDPPortForwards"] = 0
	}

	// Note: as currently tracked/counted, each established client is also an accepted client

	for tunnelProtocol, acceptedClientCount := range sshServer.acceptedClientCounts {
		loadStats[tunnelProtocol]["AcceptedClients"] = acceptedClientCount
	}

	for _, client := range sshServer.clients {
		// Note: can't sum trafficState.peakConcurrentPortForwardCount to get a global peak
		loadStats[client.tunnelProtocol]["EstablishedClients"] += 1
		client.Lock()
		loadStats[client.tunnelProtocol]["TCPPortForwards"] += client.tcpTrafficState.concurrentPortForwardCount
		loadStats[client.tunnelProtocol]["TotalTCPPortForwards"] += client.tcpTrafficState.totalPortForwardCount
		loadStats[client.tunnelProtocol]["UDPPortForwards"] += client.udpTrafficState.concurrentPortForwardCount
		loadStats[client.tunnelProtocol]["TotalUDPPortForwards"] += client.udpTrafficState.totalPortForwardCount
		client.Unlock()
	}

	return loadStats
}

func (sshServer *sshServer) stopClients() {

	sshServer.clientsMutex.Lock()
	sshServer.stoppingClients = true
	clients := sshServer.clients
	sshServer.clients = make(map[sshClientID]*sshClient)
	sshServer.clientsMutex.Unlock()

	for _, client := range clients {
		client.stop()
	}
}

func (sshServer *sshServer) handleClient(tunnelProtocol string, clientConn net.Conn) {

	sshServer.registerAcceptedClient(tunnelProtocol)
	defer sshServer.unregisterAcceptedClient(tunnelProtocol)

	geoIPData := sshServer.support.GeoIPService.Lookup(
		psiphon.IPAddressFromAddr(clientConn.RemoteAddr()))

	// TODO: apply reload of TrafficRulesSet to existing clients

	sshClient := newSshClient(
		sshServer,
		tunnelProtocol,
		geoIPData,
		sshServer.support.TrafficRulesSet.GetTrafficRules(geoIPData.Country))

	// Wrap the base client connection with an ActivityMonitoredConn which will
	// terminate the connection if no data is received before the deadline. This
	// timeout is in effect for the entire duration of the SSH connection. Clients
	// must actively use the connection or send SSH keep alive requests to keep
	// the connection active. Writes are not considered reliable activity indicators
	// due to buffering.

	activityConn, err := NewActivityMonitoredConn(
		clientConn,
		SSH_CONNECTION_READ_DEADLINE,
		false,
		nil)
	if err != nil {
		clientConn.Close()
		log.WithContextFields(LogFields{"error": err}).Error("NewActivityMonitoredConn failed")
		return
	}
	clientConn = activityConn

	// Further wrap the connection in a rate limiting ThrottledConn.

	rateLimits := sshClient.trafficRules.GetRateLimits(tunnelProtocol)
	clientConn = NewThrottledConn(
		clientConn,
		rateLimits.DownstreamUnlimitedBytes,
		int64(rateLimits.DownstreamBytesPerSecond),
		rateLimits.UpstreamUnlimitedBytes,
		int64(rateLimits.UpstreamBytesPerSecond))

	// Run the initial [obfuscated] SSH handshake in a goroutine so we can both
	// respect shutdownBroadcast and implement a specific handshake timeout.
	// The timeout is to reclaim network resources in case the handshake takes
	// too long.

	type sshNewServerConnResult struct {
		conn     net.Conn
		sshConn  *ssh.ServerConn
		channels <-chan ssh.NewChannel
		requests <-chan *ssh.Request
		err      error
	}

	resultChannel := make(chan *sshNewServerConnResult, 2)

	if SSH_HANDSHAKE_TIMEOUT > 0 {
		time.AfterFunc(time.Duration(SSH_HANDSHAKE_TIMEOUT), func() {
			resultChannel <- &sshNewServerConnResult{err: errors.New("ssh handshake timeout")}
		})
	}

	go func(conn net.Conn) {
		sshServerConfig := &ssh.ServerConfig{
			PasswordCallback: sshClient.passwordCallback,
			AuthLogCallback:  sshClient.authLogCallback,
			ServerVersion:    sshServer.support.Config.SSHServerVersion,
		}
		sshServerConfig.AddHostKey(sshServer.sshHostKey)

		result := &sshNewServerConnResult{}

		// Wrap the connection in an SSH deobfuscator when required.

		if psiphon.TunnelProtocolUsesObfuscatedSSH(tunnelProtocol) {
			// Note: NewObfuscatedSshConn blocks on network I/O
			// TODO: ensure this won't block shutdown
			conn, result.err = psiphon.NewObfuscatedSshConn(
				psiphon.OBFUSCATION_CONN_MODE_SERVER,
				clientConn,
				sshServer.support.Config.ObfuscatedSSHKey)
			if result.err != nil {
				result.err = psiphon.ContextError(result.err)
			}
		}

		if result.err == nil {
			result.sshConn, result.channels, result.requests, result.err =
				ssh.NewServerConn(conn, sshServerConfig)
		}

		resultChannel <- result

	}(clientConn)

	var result *sshNewServerConnResult
	select {
	case result = <-resultChannel:
	case <-sshServer.shutdownBroadcast:
		// Close() will interrupt an ongoing handshake
		// TODO: wait for goroutine to exit before returning?
		clientConn.Close()
		return
	}

	if result.err != nil {
		clientConn.Close()
		// This is a Debug log due to noise. The handshake often fails due to I/O
		// errors as clients frequently interrupt connections in progress when
		// client-side load balancing completes a connection to a different server.
		log.WithContextFields(LogFields{"error": result.err}).Debug("handshake failed")
		return
	}

	sshClient.Lock()
	sshClient.sshConn = result.sshConn
	sshClient.activityConn = activityConn
	sshClient.Unlock()

	clientID, ok := sshServer.registerEstablishedClient(sshClient)
	if !ok {
		clientConn.Close()
		log.WithContext().Warning("register failed")
		return
	}
	defer sshServer.unregisterEstablishedClient(clientID)

	sshClient.runClient(result.channels, result.requests)

	// Note: sshServer.unregisterClient calls sshClient.Close(),
	// which also closes underlying transport Conn.
}

type sshClient struct {
	sync.Mutex
	sshServer               *sshServer
	tunnelProtocol          string
	sshConn                 ssh.Conn
	activityConn            *ActivityMonitoredConn
	geoIPData               GeoIPData
	psiphonSessionID        string
	udpChannel              ssh.Channel
	trafficRules            TrafficRules
	tcpTrafficState         *trafficState
	udpTrafficState         *trafficState
	channelHandlerWaitGroup *sync.WaitGroup
	tcpPortForwardLRU       *LRUConns
	stopBroadcast           chan struct{}
}

type trafficState struct {
	bytesUp                        int64
	bytesDown                      int64
	concurrentPortForwardCount     int64
	peakConcurrentPortForwardCount int64
	totalPortForwardCount          int64
}

func newSshClient(
	sshServer *sshServer, tunnelProtocol string, geoIPData GeoIPData, trafficRules TrafficRules) *sshClient {
	return &sshClient{
		sshServer:               sshServer,
		tunnelProtocol:          tunnelProtocol,
		geoIPData:               geoIPData,
		trafficRules:            trafficRules,
		tcpTrafficState:         &trafficState{},
		udpTrafficState:         &trafficState{},
		channelHandlerWaitGroup: new(sync.WaitGroup),
		tcpPortForwardLRU:       NewLRUConns(),
		stopBroadcast:           make(chan struct{}),
	}
}

func (sshClient *sshClient) passwordCallback(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {

	var sshPasswordPayload struct {
		SessionId   string `json:"SessionId"`
		SshPassword string `json:"SshPassword"`
	}
	err := json.Unmarshal(password, &sshPasswordPayload)
	if err != nil {

		// Backwards compatibility case: instead of a JSON payload, older clients
		// send the hex encoded session ID prepended to the SSH password.
		// Note: there's an even older case where clients don't send any session ID,
		// but that's no longer supported.
		if len(password) == 2*psiphon.PSIPHON_API_CLIENT_SESSION_ID_LENGTH+2*SSH_PASSWORD_BYTE_LENGTH {
			sshPasswordPayload.SessionId = string(password[0 : 2*psiphon.PSIPHON_API_CLIENT_SESSION_ID_LENGTH])
			sshPasswordPayload.SshPassword = string(password[2*psiphon.PSIPHON_API_CLIENT_SESSION_ID_LENGTH : len(password)])
		} else {
			return nil, psiphon.ContextError(fmt.Errorf("invalid password payload for %q", conn.User()))
		}
	}

	if !isHexDigits(sshClient.sshServer.support, sshPasswordPayload.SessionId) {
		return nil, psiphon.ContextError(fmt.Errorf("invalid session ID for %q", conn.User()))
	}

	userOk := (subtle.ConstantTimeCompare(
		[]byte(conn.User()), []byte(sshClient.sshServer.support.Config.SSHUserName)) == 1)

	passwordOk := (subtle.ConstantTimeCompare(
		[]byte(sshPasswordPayload.SshPassword), []byte(sshClient.sshServer.support.Config.SSHPassword)) == 1)

	if !userOk || !passwordOk {
		return nil, psiphon.ContextError(fmt.Errorf("invalid password for %q", conn.User()))
	}

	psiphonSessionID := sshPasswordPayload.SessionId

	sshClient.Lock()
	sshClient.psiphonSessionID = psiphonSessionID
	geoIPData := sshClient.geoIPData
	sshClient.Unlock()

	// Store the GeoIP data associated with the session ID. This makes the GeoIP data
	// available to the web server for web transport Psiphon API requests.
	sshClient.sshServer.support.GeoIPService.SetSessionCache(
		psiphonSessionID, geoIPData)

	return nil, nil
}

func (sshClient *sshClient) authLogCallback(conn ssh.ConnMetadata, method string, err error) {

	if err != nil {

		if method == "none" && err.Error() == "no auth passed yet" {
			// In this case, the callback invocation is noise from auth negotiation
			return
		}

		// Note: here we previously logged messages for fail2ban to act on. This is no longer
		// done as the complexity outweighs the benefits.
		//
		// - The SSH credential is not secret -- it's in the server entry. Attackers targetting
		//   the server likely already have the credential. On the other hand, random scanning and
		//   brute forcing is mitigated with high entropy random passwords, rate limiting
		//   (implemented on the host via iptables), and limited capabilities (the SSH session can
		//   only port forward).
		//
		// - fail2ban coverage was inconsistent; in the case of an unfronted meek protocol through
		//   an upstream proxy, the remote address is the upstream proxy, which should not be blocked.
		//   The X-Forwarded-For header cant be used instead as it may be forged and used to get IPs
		//   deliberately blocked; and in any case fail2ban adds iptables rules which can only block
		//   by direct remote IP, not by original client IP. Fronted meek has the same iptables issue.
		//
		// TODO: random scanning and brute forcing of port 22 will result in log noise. To eliminate
		// this, and to also cover meek protocols, and bad obfuscation keys, and bad inputs to the web
		// server, consider implementing fail2ban-type logic directly in this server, with the ability
		// to use X-Forwarded-For (when trustworthy; e.g, from a CDN).

		log.WithContextFields(LogFields{"error": err, "method": method}).Warning("authentication failed")

	} else {

		log.WithContextFields(LogFields{"error": err, "method": method}).Debug("authentication success")
	}
}

func (sshClient *sshClient) stop() {

	sshClient.sshConn.Close()
	sshClient.sshConn.Wait()

	close(sshClient.stopBroadcast)
	sshClient.channelHandlerWaitGroup.Wait()

	// Note: reporting duration based on last confirmed data transfer, which
	// is reads for sshClient.activityConn.GetActiveDuration(), and not
	// connection closing is important for protocols such as meek. For
	// meek, the connection remains open until the HTTP session expires,
	// which may be some time after the tunnel has closed. (The meek
	// protocol has no allowance for signalling payload EOF, and even if
	// it did the client may not have the opportunity to send a final
	// request with an EOF flag set.)

	sshClient.Lock()
	log.WithContextFields(
		LogFields{
			"startTime":                         sshClient.activityConn.GetStartTime(),
			"duration":                          sshClient.activityConn.GetActiveDuration(),
			"psiphonSessionID":                  sshClient.psiphonSessionID,
			"country":                           sshClient.geoIPData.Country,
			"city":                              sshClient.geoIPData.City,
			"ISP":                               sshClient.geoIPData.ISP,
			"bytesUpTCP":                        sshClient.tcpTrafficState.bytesUp,
			"bytesDownTCP":                      sshClient.tcpTrafficState.bytesDown,
			"peakConcurrentPortForwardCountTCP": sshClient.tcpTrafficState.peakConcurrentPortForwardCount,
			"totalPortForwardCountTCP":          sshClient.tcpTrafficState.totalPortForwardCount,
			"bytesUpUDP":                        sshClient.udpTrafficState.bytesUp,
			"bytesDownUDP":                      sshClient.udpTrafficState.bytesDown,
			"peakConcurrentPortForwardCountUDP": sshClient.udpTrafficState.peakConcurrentPortForwardCount,
			"totalPortForwardCountUDP":          sshClient.udpTrafficState.totalPortForwardCount,
		}).Info("tunnel closed")
	sshClient.Unlock()
}

// runClient handles/dispatches new channel and new requests from the client.
// When the SSH client connection closes, both the channels and requests channels
// will close and runClient will exit.
func (sshClient *sshClient) runClient(
	channels <-chan ssh.NewChannel, requests <-chan *ssh.Request) {

	requestsWaitGroup := new(sync.WaitGroup)
	requestsWaitGroup.Add(1)
	go func() {
		defer requestsWaitGroup.Done()

		for request := range requests {

			// Requests are processed serially; API responses must be sent in request order.

			var responsePayload []byte
			var err error

			if request.Type == "keepalive@openssh.com" {
				// Keepalive requests have an empty response.
			} else {
				// All other requests are assumed to be API requests.
				responsePayload, err = sshAPIRequestHandler(
					sshClient.sshServer.support,
					sshClient.geoIPData,
					request.Type,
					request.Payload)
			}

			if err == nil {
				err = request.Reply(true, responsePayload)
			} else {
				log.WithContextFields(LogFields{"error": err}).Warning("request failed")
				err = request.Reply(false, nil)
			}
			if err != nil {
				log.WithContextFields(LogFields{"error": err}).Warning("response failed")
			}

		}
	}()

	for newChannel := range channels {

		if newChannel.ChannelType() != "direct-tcpip" {
			sshClient.rejectNewChannel(newChannel, ssh.Prohibited, "unknown or unsupported channel type")
			continue
		}

		// process each port forward concurrently
		sshClient.channelHandlerWaitGroup.Add(1)
		go sshClient.handleNewPortForwardChannel(newChannel)
	}

	requestsWaitGroup.Wait()
}

func (sshClient *sshClient) rejectNewChannel(newChannel ssh.NewChannel, reason ssh.RejectionReason, logMessage string) {
	log.WithContextFields(
		LogFields{
			"channelType":  newChannel.ChannelType(),
			"logMessage":   logMessage,
			"rejectReason": reason.String(),
		}).Warning("reject new channel")
	// Note: logMessage is internal, for logging only; just the RejectionReason is sent to the client
	newChannel.Reject(reason, reason.String())
}

func (sshClient *sshClient) handleNewPortForwardChannel(newChannel ssh.NewChannel) {
	defer sshClient.channelHandlerWaitGroup.Done()

	// http://tools.ietf.org/html/rfc4254#section-7.2
	var directTcpipExtraData struct {
		HostToConnect       string
		PortToConnect       uint32
		OriginatorIPAddress string
		OriginatorPort      uint32
	}

	err := ssh.Unmarshal(newChannel.ExtraData(), &directTcpipExtraData)
	if err != nil {
		sshClient.rejectNewChannel(newChannel, ssh.Prohibited, "invalid extra data")
		return
	}

	// Intercept TCP port forwards to a specified udpgw server and handle directly.
	// TODO: also support UDP explicitly, e.g. with a custom "direct-udp" channel type?
	isUDPChannel := sshClient.sshServer.support.Config.UDPInterceptUdpgwServerAddress != "" &&
		sshClient.sshServer.support.Config.UDPInterceptUdpgwServerAddress ==
			fmt.Sprintf("%s:%d",
				directTcpipExtraData.HostToConnect,
				directTcpipExtraData.PortToConnect)

	if isUDPChannel {
		sshClient.handleUDPChannel(newChannel)
	} else {
		sshClient.handleTCPChannel(
			directTcpipExtraData.HostToConnect, int(directTcpipExtraData.PortToConnect), newChannel)
	}
}

func (sshClient *sshClient) isPortForwardPermitted(
	host string, port int, allowPorts []int, denyPorts []int) bool {

	if psiphon.Contains(SSH_DISALLOWED_PORT_FORWARD_HOSTS, host) {
		return false
	}

	// TODO: faster lookup?
	if len(allowPorts) > 0 {
		for _, allowPort := range allowPorts {
			if port == allowPort {
				return true
			}
		}
		return false
	}

	if len(denyPorts) > 0 {
		for _, denyPort := range denyPorts {
			if port == denyPort {
				return false
			}
		}
	}

	return true
}

func (sshClient *sshClient) isPortForwardLimitExceeded(
	state *trafficState, maxPortForwardCount int) bool {

	limitExceeded := false
	if maxPortForwardCount > 0 {
		sshClient.Lock()
		limitExceeded = state.concurrentPortForwardCount >= int64(maxPortForwardCount)
		sshClient.Unlock()
	}
	return limitExceeded
}

func (sshClient *sshClient) openedPortForward(
	state *trafficState) {

	sshClient.Lock()
	state.concurrentPortForwardCount += 1
	if state.concurrentPortForwardCount > state.peakConcurrentPortForwardCount {
		state.peakConcurrentPortForwardCount = state.concurrentPortForwardCount
	}
	state.totalPortForwardCount += 1
	sshClient.Unlock()
}

func (sshClient *sshClient) closedPortForward(
	state *trafficState, bytesUp, bytesDown int64) {

	sshClient.Lock()
	state.concurrentPortForwardCount -= 1
	state.bytesUp += bytesUp
	state.bytesDown += bytesDown
	sshClient.Unlock()
}

func (sshClient *sshClient) handleTCPChannel(
	hostToConnect string,
	portToConnect int,
	newChannel ssh.NewChannel) {

	if !sshClient.isPortForwardPermitted(
		hostToConnect,
		portToConnect,
		sshClient.trafficRules.AllowTCPPorts,
		sshClient.trafficRules.DenyTCPPorts) {

		sshClient.rejectNewChannel(
			newChannel, ssh.Prohibited, "port forward not permitted")
		return
	}

	var bytesUp, bytesDown int64
	sshClient.openedPortForward(sshClient.tcpTrafficState)
	defer func() {
		sshClient.closedPortForward(
			sshClient.tcpTrafficState,
			atomic.LoadInt64(&bytesUp),
			atomic.LoadInt64(&bytesDown))
	}()

	// TOCTOU note: important to increment the port forward count (via
	// openPortForward) _before_ checking isPortForwardLimitExceeded
	// otherwise, the client could potentially consume excess resources
	// by initiating many port forwards concurrently.
	// TODO: close LRU connection (after successful Dial) instead of
	// rejecting new connection?
	if sshClient.isPortForwardLimitExceeded(
		sshClient.tcpTrafficState,
		sshClient.trafficRules.MaxTCPPortForwardCount) {

		// Close the oldest TCP port forward. CloseOldest() closes
		// the conn and the port forward's goroutine will complete
		// the cleanup asynchronously.
		//
		// Some known limitations:
		//
		// - Since CloseOldest() closes the upstream socket but does not
		//   clean up all resources associated with the port forward. These
		//   include the goroutine(s) relaying traffic as well as the SSH
		//   channel. Closing the socket will interrupt the goroutines which
		//   will then complete the cleanup. But, since the full cleanup is
		//   asynchronous, there exists a possibility that a client can consume
		//   more than max port forward resources -- just not upstream sockets.
		//
		// - An LRU list entry for this port forward is not added until
		//   after the dial completes, but the port forward is counted
		//   towards max limits. This means many dials in progress will
		//   put established connections in jeopardy.
		//
		// - We're closing the oldest open connection _before_ successfully
		//   dialing the new port forward. This means we are potentially
		//   discarding a good connection to make way for a failed connection.
		//   We cannot simply dial first and still maintain a limit on
		//   resources used, so to address this we'd need to add some
		//   accounting for connections still establishing.

		sshClient.tcpPortForwardLRU.CloseOldest()

		log.WithContextFields(
			LogFields{
				"maxCount": sshClient.trafficRules.MaxTCPPortForwardCount,
			}).Debug("closed LRU TCP port forward")
	}

	// Dial the target remote address. This is done in a goroutine to
	// ensure the shutdown signal is handled immediately.

	remoteAddr := fmt.Sprintf("%s:%d", hostToConnect, portToConnect)

	log.WithContextFields(LogFields{"remoteAddr": remoteAddr}).Debug("dialing")

	type dialTcpResult struct {
		conn net.Conn
		err  error
	}

	resultChannel := make(chan *dialTcpResult, 1)

	go func() {
		// TODO: on EADDRNOTAVAIL, temporarily suspend new clients
		// TODO: IPv6 support
		conn, err := net.DialTimeout(
			"tcp4", remoteAddr, SSH_TCP_PORT_FORWARD_DIAL_TIMEOUT)
		resultChannel <- &dialTcpResult{conn, err}
	}()

	var result *dialTcpResult
	select {
	case result = <-resultChannel:
	case <-sshClient.stopBroadcast:
		// Note: may leave dial in progress
		return
	}

	if result.err != nil {
		sshClient.rejectNewChannel(newChannel, ssh.ConnectionFailed, result.err.Error())
		return
	}

	// The upstream TCP port forward connection has been established. Schedule
	// some cleanup and notify the SSH client that the channel is accepted.

	fwdConn := result.conn
	defer fwdConn.Close()

	fwdChannel, requests, err := newChannel.Accept()
	if err != nil {
		log.WithContextFields(LogFields{"error": err}).Warning("accept new channel failed")
		return
	}
	go ssh.DiscardRequests(requests)
	defer fwdChannel.Close()

	// ActivityMonitoredConn monitors the TCP port forward I/O and updates
	// its LRU status. ActivityMonitoredConn also times out I/O on the port
	// forward if both reads and writes have been idle for the specified
	// duration.

	lruEntry := sshClient.tcpPortForwardLRU.Add(fwdConn)
	defer lruEntry.Remove()

	fwdConn, err = NewActivityMonitoredConn(
		fwdConn,
		time.Duration(sshClient.trafficRules.IdleTCPPortForwardTimeoutMilliseconds)*time.Millisecond,
		true,
		lruEntry)
	if result.err != nil {
		log.WithContextFields(LogFields{"error": err}).Error("NewActivityMonitoredConn failed")
		return
	}

	// Relay channel to forwarded connection.

	log.WithContextFields(LogFields{"remoteAddr": remoteAddr}).Debug("relaying")

	// TODO: relay errors to fwdChannel.Stderr()?
	relayWaitGroup := new(sync.WaitGroup)
	relayWaitGroup.Add(1)
	go func() {
		defer relayWaitGroup.Done()
		// io.Copy allocates a 32K temporary buffer, and each port forward relay uses
		// two of these buffers; using io.CopyBuffer with a smaller buffer reduces the
		// overall memory footprint.
		bytes, err := io.CopyBuffer(
			fwdChannel, fwdConn, make([]byte, SSH_TCP_PORT_FORWARD_COPY_BUFFER_SIZE))
		atomic.AddInt64(&bytesDown, bytes)
		if err != nil && err != io.EOF {
			// Debug since errors such as "connection reset by peer" occur during normal operation
			log.WithContextFields(LogFields{"error": err}).Debug("downstream TCP relay failed")
		}
		// Interrupt upstream io.Copy when downstream is shutting down.
		// TODO: this is done to quickly cleanup the port forward when
		// fwdConn has a read timeout, but is it clean -- upstream may still
		// be flowing?
		fwdChannel.Close()
	}()
	bytes, err := io.CopyBuffer(
		fwdConn, fwdChannel, make([]byte, SSH_TCP_PORT_FORWARD_COPY_BUFFER_SIZE))
	atomic.AddInt64(&bytesUp, bytes)
	if err != nil && err != io.EOF {
		log.WithContextFields(LogFields{"error": err}).Debug("upstream TCP relay failed")
	}
	// Shutdown special case: fwdChannel will be closed and return EOF when
	// the SSH connection is closed, but we need to explicitly close fwdConn
	// to interrupt the downstream io.Copy, which may be blocked on a
	// fwdConn.Read().
	fwdConn.Close()

	relayWaitGroup.Wait()

	log.WithContextFields(
		LogFields{
			"remoteAddr": remoteAddr,
			"bytesUp":    atomic.LoadInt64(&bytesUp),
			"bytesDown":  atomic.LoadInt64(&bytesDown)}).Debug("exiting")
}